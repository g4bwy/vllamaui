package mcpx

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"syscall"

	"llama-webui/server/internal/contracts"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// reply is a tools/call answer in the shape mcp_result_to_response reads: the
// text parts of result.content, the isError flag, and at most one of a
// JSON-RPC error message or a transport failure.
type reply struct {
	texts      []string
	nonText    int // content parts with no text, which the C++ drops too
	structured bool

	hasResult bool   // the answer carried a "result" object
	isError   bool   // result.isError
	rpcErr    string // error.message of a JSON-RPC error answer
	hasRPCErr bool
	xportErr  error // a failure below the protocol: dead pipe, deadline, cancel
}

func replyFromCall(res *mcp.CallToolResult, err error) reply {
	if err != nil {
		var je *jsonrpc.Error
		if errors.As(err, &je) {
			return reply{hasRPCErr: true, rpcErr: je.Message}
		}
		return reply{xportErr: err}
	}
	if res == nil {
		return reply{}
	}
	r := reply{hasResult: true, isError: res.IsError}
	for _, part := range res.Content {
		if tc, ok := part.(*mcp.TextContent); ok {
			r.texts = append(r.texts, tc.Text)
			continue
		}
		r.nonText++
	}
	if hasStructured(res.StructuredContent) {
		r.structured = true
	}
	return r
}

func hasStructured(v any) bool {
	raw, ok := v.(json.RawMessage)
	if !ok {
		return v != nil
	}
	return len(raw) > 0 && string(raw) != "null"
}

// convert is mcp_result_to_response, with the error mapping that
// server_mcp_transport::call_tool and send_rpc do around it.
func (r reply) convert(log *slog.Logger, tool string, ctx context.Context, stopping bool) contracts.Result {
	switch {
	case r.hasRPCErr:
		return contracts.Result{Error: orUnknown(r.rpcErr)}
	case r.xportErr != nil:
		return contracts.Result{Error: classify(r.xportErr, ctx, stopping)}
	case !r.hasResult:
		return contracts.Result{Error: textInvalid}
	}
	// Only text parts make the payload. An empty result is hard to debug, so
	// say what was thrown away.
	if r.nonText > 0 {
		log.Warn(fmt.Sprintf(msgDroppedParts, tool, r.nonText))
	}
	if r.structured {
		log.Warn(fmt.Sprintf(msgDroppedSchema, tool))
	}
	text := strings.Join(r.texts, "\n")
	if r.isError {
		if text == "" {
			text = textToolFailed
		}
		return contracts.Result{Error: text}
	}
	return contracts.Result{PlainText: text}
}

func orUnknown(msg string) string {
	if msg == "" {
		return textUnknown
	}
	return msg
}

// classify names the three ways a call can fail below the protocol. The C++
// reaches the same three texts in send_rpc: cancelled first, then the deadline,
// then a broken pipe.
func classify(err error, ctx context.Context, stopping bool) string {
	if stopping || errors.Is(context.Cause(ctx), context.Canceled) || errors.Is(err, context.Canceled) {
		return textCancelled
	}
	if errors.Is(context.Cause(ctx), context.DeadlineExceeded) || errors.Is(err, context.DeadlineExceeded) {
		return textTimedOut
	}
	switch {
	case errors.Is(err, mcp.ErrConnectionClosed),
		errors.Is(err, io.EOF),
		errors.Is(err, io.ErrUnexpectedEOF),
		errors.Is(err, os.ErrClosed),
		errors.Is(err, syscall.EPIPE),
		errors.Is(err, syscall.ECONNRESET):
		return textClosed
	}
	var je *jsonrpc.Error
	if errors.As(err, &je) {
		return orUnknown(je.Message)
	}
	return orUnknown(err.Error())
}

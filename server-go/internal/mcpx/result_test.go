package mcpx

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"strings"
	"syscall"
	"testing"
	"time"

	"llama-webui/server/internal/contracts"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

var quiet = slog.New(slog.DiscardHandler)

func TestConvertTextParts(t *testing.T) {
	cases := []struct {
		name string
		res  *mcp.CallToolResult
		want contracts.Result
	}{
		{
			name: "one text part",
			res:  &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "hello"}}},
			want: contracts.Result{PlainText: "hello"},
		},
		{
			name: "several text parts joined by newlines",
			res: &mcp.CallToolResult{Content: []mcp.Content{
				&mcp.TextContent{Text: "one"},
				&mcp.TextContent{Text: ""},
				&mcp.TextContent{Text: "two"},
			}},
			want: contracts.Result{PlainText: "one\n\ntwo"},
		},
		{
			name: "no content",
			res:  &mcp.CallToolResult{},
			want: contracts.Result{PlainText: ""},
		},
		{
			name: "isError with text",
			res: &mcp.CallToolResult{
				Content: []mcp.Content{&mcp.TextContent{Text: "bad input"}},
				IsError: true,
			},
			want: contracts.Result{Error: "bad input"},
		},
		{
			name: "isError joins text parts too",
			res: &mcp.CallToolResult{
				Content: []mcp.Content{&mcp.TextContent{Text: "a"}, &mcp.TextContent{Text: "b"}},
				IsError: true,
			},
			want: contracts.Result{Error: "a\nb"},
		},
		{
			name: "isError without text",
			res:  &mcp.CallToolResult{IsError: true},
			want: contracts.Result{Error: "MCP tool returned an error"},
		},
		{
			name: "isError with only an image",
			res: &mcp.CallToolResult{
				Content: []mcp.Content{&mcp.ImageContent{MIMEType: "image/png", Data: []byte("x")}},
				IsError: true,
			},
			want: contracts.Result{Error: "MCP tool returned an error"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := replyFromCall(tc.res, nil).convert(quiet, "srv_tool", context.Background(), false)
			if got.Error != tc.want.Error || got.PlainText != tc.want.PlainText || got.Body != nil {
				t.Errorf("got %+v want %+v", got, tc.want)
			}
		})
	}
}

func TestConvertErrors(t *testing.T) {
	// A context with room left, so classify looks at the error itself.
	live, cancel := context.WithTimeout(context.Background(), time.Hour)
	defer cancel()

	cases := []struct {
		name     string
		err      error
		ctx      context.Context
		stopping bool
		want     string
	}{
		{name: "jsonrpc error message", err: &jsonrpc.Error{Code: -32601, Message: "tool not found"}, ctx: live, want: "tool not found"},
		{name: "jsonrpc error without a message", err: &jsonrpc.Error{Code: -32603}, ctx: live, want: "unknown error"},
		{name: "wrapped jsonrpc error", err: fmt.Errorf("call: %w", &jsonrpc.Error{Message: "no such tool"}), ctx: live, want: "no such tool"},
		{name: "connection closed", err: fmt.Errorf("call: %w", mcp.ErrConnectionClosed), ctx: live, want: "transport closed"},
		{name: "eof on a dead child", err: io.ErrUnexpectedEOF, ctx: live, want: "transport closed"},
		{name: "broken pipe", err: &fs.PathError{Op: "write", Path: "|1", Err: syscall.EPIPE}, ctx: live, want: "transport closed"},
		{name: "cancelled by the caller", err: context.Canceled, ctx: live, want: "cancelled"},
		{name: "cancelled by shutdown", err: io.EOF, ctx: live, stopping: true, want: "cancelled"},
		{name: "anything else keeps the message", err: errors.New("unexpected content type"), ctx: live, want: "unexpected content type"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := replyFromCall(nil, tc.err).convert(quiet, "srv_tool", tc.ctx, tc.stopping)
			if got.Error != tc.want {
				t.Errorf("got %q want %q", got.Error, tc.want)
			}
			if got.PlainText != "" {
				t.Errorf("PlainText = %q, want empty", got.PlainText)
			}
			if out := string(got.JSON()); !strings.Contains(out, `"error"`) {
				t.Errorf("JSON = %s, want an error object", out)
			}
		})
	}
}

// The two real ways a call runs out of road, from a live context rather than a
// hand made error.
func TestClassifyRealDeadlineAndCancel(t *testing.T) {
	dead, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	<-dead.Done()
	if got := classify(dead.Err(), dead, false); got != textTimedOut {
		t.Errorf("deadline = %q, want %q", got, textTimedOut)
	}

	gone, cancelGone := context.WithCancel(context.Background())
	cancelGone()
	if got := classify(gone.Err(), gone, false); got != textCancelled {
		t.Errorf("cancel = %q, want %q", got, textCancelled)
	}

	// Shutdown wins over a pipe error, like the should_stop flag in send_rpc.
	if got := classify(io.EOF, context.Background(), true); got != textCancelled {
		t.Errorf("stopping = %q, want %q", got, textCancelled)
	}
}

// An answer with neither "result" nor "error" is a failure, not an empty
// success.
func TestConvertInvalidResponse(t *testing.T) {
	got := reply{}.convert(quiet, "srv_tool", context.Background(), false)
	if got.Error != textInvalid {
		t.Errorf("got %+v, want %q", got, textInvalid)
	}
	if string(got.JSON()) != `{"error":"invalid response from MCP server"}` {
		t.Errorf("JSON = %s", got.JSON())
	}
}

func TestConvertCountsDroppedParts(t *testing.T) {
	rec := &recorder{}
	res := &mcp.CallToolResult{Content: []mcp.Content{
		&mcp.TextContent{Text: "keep me"},
		&mcp.ImageContent{MIMEType: "image/png", Data: []byte("a")},
		&mcp.AudioContent{MIMEType: "audio/wav", Data: []byte("b")},
		&mcp.ResourceLink{Name: "link", URI: "file:///x"},
		&mcp.EmbeddedResource{},
	}}
	got := replyFromCall(res, nil).convert(rec.logger(), "fs_read", context.Background(), false)
	if got.PlainText != "keep me" {
		t.Errorf("PlainText = %q, want the text part only", got.PlainText)
	}
	if !rec.hasAll("fs_read", "discarded 4 non-text content part") {
		t.Errorf("no discard line, log:\n%s", strings.Join(rec.lines, "\n"))
	}
	if rec.count("discarded") != 1 {
		t.Error("the discard report must be one line")
	}
}

func TestConvertDropsStructuredContent(t *testing.T) {
	rec := &recorder{}
	res := &mcp.CallToolResult{
		Content:           []mcp.Content{&mcp.TextContent{Text: "text wins"}},
		StructuredContent: map[string]any{"a": 1},
	}
	got := replyFromCall(res, nil).convert(rec.logger(), "fs_read", context.Background(), false)
	if got.PlainText != "text wins" {
		t.Errorf("PlainText = %q", got.PlainText)
	}
	if !rec.hasAll("fs_read", "ignored structuredContent") {
		t.Error("missing the structuredContent line")
	}

	rec2 := &recorder{}
	replyFromCall(&mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "x"}}}, nil).
		convert(rec2.logger(), "fs_read", context.Background(), false)
	if len(rec2.lines) != 0 {
		t.Errorf("log = %v, want nothing", rec2.lines)
	}
}

func TestResultJSONShape(t *testing.T) {
	// The webui reads error, then plain_text_response. contracts owns that
	// order, so check the shapes this package produces.
	if got := string(contracts.Result{Error: "boom"}.JSON()); got != `{"error":"boom"}` {
		t.Errorf("error result = %s", got)
	}
	if got := string(contracts.Result{PlainText: "hi"}.JSON()); got != `{"plain_text_response":"hi"}` {
		t.Errorf("text result = %s", got)
	}
	if got := string(contracts.Result{}.JSON()); got != `{"plain_text_response":""}` {
		t.Errorf("empty result = %s", got)
	}
}

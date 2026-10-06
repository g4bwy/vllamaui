// Command testserver is the fixture MCP server for the mcpx tests. It speaks
// stdio, so the tests exercise the real transport against the real SDK.
//
// Flags:
//
//	-exit-after N  answer N tool calls, then exit, to test a respawn
//	-sleep-start d wait before the handshake, to test the warmup cap
//
// It writes one banner line to stderr at startup. The mcpx tests read it back
// out of the stderr tail, which also proves that env and cwd reach the child.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"sync/atomic"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

var calls atomic.Int64

func main() {
	exitAfter := flag.Int("exit-after", 0, "exit after this many tool calls")
	sleepStart := flag.Duration("sleep-start", 0, "sleep before serving")
	flag.Parse()

	if *sleepStart > 0 {
		time.Sleep(*sleepStart)
	}
	cwd, _ := os.Getwd()
	fmt.Fprintf(os.Stderr, "testserver pid=%d cwd=%s banner=%s\n", os.Getpid(), cwd, os.Getenv("MCPX_TEST_BANNER"))

	s := mcp.NewServer(&mcp.Implementation{Name: "mcpx-testserver", Version: "1.0"}, nil)
	addTools(s, int64(*exitAfter))

	if err := s.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
		fmt.Fprintf(os.Stderr, "testserver: %v\n", err)
		os.Exit(1)
	}
}

func addTools(s *mcp.Server, exitAfter int64) {
	raw := func(schema string) json.RawMessage { return json.RawMessage(schema) }

	// guard answers a call, then ends the process, so a test can watch the next
	// call respawn the child.
	answer := func(h func(args json.RawMessage) *mcp.CallToolResult) mcp.ToolHandler {
		return func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			n := calls.Add(1)
			if exitAfter > 0 && n > exitAfter {
				os.Exit(0)
			}
			return h(req.Params.Arguments), nil
		}
	}

	s.AddTool(&mcp.Tool{
		Name:        "echo",
		Description: "Return the text argument.",
		InputSchema: raw(`{"type":"object","properties":{"text":{"type":"string"}},"required":["text"]}`),
	}, answer(func(args json.RawMessage) *mcp.CallToolResult {
		var in struct {
			Text string `json:"text"`
		}
		_ = json.Unmarshal(args, &in)
		return text(in.Text)
	}))

	s.AddTool(&mcp.Tool{
		Name:        "slow",
		Description: "Sleep for ms milliseconds, then answer.",
		InputSchema: raw(`{"type":"object","properties":{"ms":{"type":"integer"}}}`),
	}, answer(func(args json.RawMessage) *mcp.CallToolResult {
		var in struct {
			MS int `json:"ms"`
		}
		_ = json.Unmarshal(args, &in)
		time.Sleep(time.Duration(in.MS) * time.Millisecond)
		return text(fmt.Sprintf("slept %dms", in.MS))
	}))

	s.AddTool(&mcp.Tool{
		Name:        "boom",
		Description: "Always fail.",
		InputSchema: raw(`{"type":"object","properties":{}}`),
	}, answer(func(json.RawMessage) *mcp.CallToolResult {
		res := text("boom: it went wrong")
		res.IsError = true
		return res
	}))

	s.AddTool(&mcp.Tool{
		Name:        "multitext",
		Description: "Two text parts.",
		InputSchema: raw(`{"type":"object","properties":{}}`),
	}, answer(func(json.RawMessage) *mcp.CallToolResult {
		return &mcp.CallToolResult{Content: []mcp.Content{
			&mcp.TextContent{Text: "one"},
			&mcp.TextContent{Text: "two"},
		}}
	}))

	s.AddTool(&mcp.Tool{
		Name:        "hasimage",
		Description: "One text part and one image part.",
		InputSchema: raw(`{"type":"object","properties":{}}`),
	}, answer(func(json.RawMessage) *mcp.CallToolResult {
		return &mcp.CallToolResult{Content: []mcp.Content{
			&mcp.TextContent{Text: "here is a picture"},
			&mcp.ImageContent{MIMEType: "image/png", Data: []byte("not-really-a-png")},
		}}
	}))

	s.AddTool(&mcp.Tool{
		Name:        "counter",
		Description: "Number of calls answered by this process.",
		InputSchema: raw(`{"type":"object","properties":{}}`),
	}, answer(func(json.RawMessage) *mcp.CallToolResult {
		return text(fmt.Sprintf("count=%d", calls.Load()))
	}))

	s.AddTool(&mcp.Tool{
		Name:        "pid",
		Description: "Process id of the server.",
		InputSchema: raw(`{"type":"object","properties":{}}`),
	}, answer(func(json.RawMessage) *mcp.CallToolResult {
		return text(fmt.Sprintf("pid=%d", os.Getpid()))
	}))

	// add_late grows the tool list while a session is open, so the client has
	// to react to notifications/tools/list_changed.
	s.AddTool(&mcp.Tool{
		Name:        "add_late",
		Description: "Add a tool to this server and announce it.",
		InputSchema: raw(`{"type":"object","properties":{}}`),
	}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		s.AddTool(&mcp.Tool{
			Name:        "late",
			Description: "Only there after add_late.",
			InputSchema: raw(`{"type":"object","properties":{}}`),
		}, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return text("late answer"), nil
		})
		return text("added"), nil
	})
}

func text(s string) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: s}}}
}

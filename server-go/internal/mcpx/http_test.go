package mcpx

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// The HTTP transport is an addition: llama.cpp only speaks stdio. The webui own
// MCP support is HTTP, so an entry with "url" talks to a streamable endpoint.
// The endpoint here is the SDK own server, in process.
func TestStreamableHTTPServer(t *testing.T) {
	srv := mcp.NewServer(&mcp.Implementation{Name: "inproc", Version: "1.0"}, nil)
	add := func(name, reply string) {
		srv.AddTool(&mcp.Tool{
			Name:        name,
			Description: "answers " + reply,
			InputSchema: json.RawMessage(`{"type":"object","properties":{}}`),
		}, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: reply}}}, nil
		})
	}
	add("ping", "pong")

	httpSrv := httptest.NewServer(mcp.NewStreamableHTTPHandler(
		func(*http.Request) *mcp.Server { return srv }, nil))
	t.Cleanup(httpSrv.Close)

	cfg := Config{Servers: []ServerConfig{{Name: "web", URL: httpSrv.URL, TimeoutMS: 5000}}}
	c, _ := newClient(t, cfg)
	mustHaveTool(t, c, "web_ping")

	if res := call(t, c, "web_ping", nil); res.PlainText != "pong" {
		t.Fatalf("ping = %+v, want pong", res)
	}
	if c.entries["web"].child != nil {
		t.Error("an http server must not have a child process")
	}

	// A tool added after the session is open arrives as
	// notifications/tools/list_changed, and the registry follows.
	add("late", "sorry i am new here")
	if !waitUntil(func() bool { _, ok := c.Get("web_late"); return ok }) {
		t.Fatal("web_late never showed up after the notification")
	}
	if res := call(t, c, "web_late", nil); res.PlainText != "sorry i am new here" {
		t.Errorf("late = %+v", res)
	}

	srv.RemoveTools("ping")
	if !waitUntil(func() bool { _, ok := c.Get("web_ping"); return !ok }) {
		t.Error("web_ping stayed after the server removed it")
	}
	mustHaveTool(t, c, "web_late")
}

// A url that points at nothing is a warmup failure, not a panic.
func TestHTTPServerUnreachable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)

	cfg := Config{Servers: []ServerConfig{{Name: "gone", URL: srv.URL, TimeoutMS: 1000}}}
	c, rec := newClient(t, cfg)
	if n := len(c.List()); n != 0 {
		t.Errorf("discovered %d tools from a 404 endpoint", n)
	}
	if !rec.has("MCP warmup: failed to spawn 'gone'") {
		t.Error("missing the warmup failure for the http server")
		rec.dump(t)
	}
	if res := c.entries["gone"].call(context.Background(), "ping", nil); res.Error != "MCP server unavailable: gone" {
		t.Errorf("call = %+v, want unavailable", res)
	}
}

// Package contracts holds the types that cross package boundaries, so the
// engine, the built-in tools and the MCP client can be built apart.
package contracts

import (
	"context"
	"encoding/json"
)

// ToolDefinition is an OpenAI function tool, exactly as the webui reads it from
// GET /tools[].definition and from the completion request tools array.
type ToolDefinition struct {
	Type         string          `json:"type"` // always "function"
	FunctionName string          `json:"-"`
	Definition   json.RawMessage `json:"definition"`
}

// ToolInfo is one element of the GET /tools array. Field names are fixed by
// llama-server: server-tools.cpp builds display_name, tool, type, permissions,
// uses_cwd and definition.
type ToolInfo struct {
	DisplayName string `json:"display_name"`
	Tool        string `json:"tool"`
	Type        string `json:"type"` // "server" or "mcp"
	Permissions struct {
		Write bool `json:"write"`
	} `json:"permissions"`
	UsesCwd    bool            `json:"uses_cwd"`
	Definition json.RawMessage `json:"definition"`
}

// ToolRequest is a POST /tools call after header merging: cwd and resp_type
// come from x-tool-cwd and x-resp-type, and a header wins over params.
type ToolRequest struct {
	Name     string
	Params   map[string]any
	Cwd      string
	RespType string
	Stream   bool
}

// Result is what a tool returns. The webui reads, in order: error, then
// plain_text_response, else it stringifies the whole value.
type Result struct {
	PlainText string
	Body      map[string]any // used when PlainText is empty
	Error     string
}

// JSON renders the wire object for a result. Error wins, then plain text, then
// the body map, mirroring how llama-server serializes what a tool returned.
func (r Result) JSON() json.RawMessage {
	switch {
	case r.Error != "":
		return marshal(map[string]any{"error": r.Error})
	case r.PlainText != "":
		return marshal(map[string]any{"plain_text_response": r.PlainText})
	case r.Body != nil:
		return marshal(r.Body)
	default:
		return marshal(map[string]any{"plain_text_response": ""})
	}
}

// Sink receives streamed output chunks while a tool runs. Only exec uses it
// today; the frame format is owned by the HTTP layer.
type Sink func(text string)

// Tool is one callable tool, built-in or discovered over MCP.
type Tool interface {
	Info() ToolInfo
	// Invoke runs the tool. Implementations must honour ctx cancellation and
	// must not return an error value: report failure through Result.Error,
	// using the same wording llama-server uses.
	Invoke(ctx context.Context, req ToolRequest, out Sink) Result
}

// Registry looks tools up by the name the model sees.
type Registry interface {
	List() []ToolInfo
	Get(name string) (Tool, bool)
}

// marshal is split out so Result.JSON stays readable.
func marshal(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage(`{"error":"serialization failed"}`)
	}
	return b
}

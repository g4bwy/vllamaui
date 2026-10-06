package toolsapi

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"llama-webui/server/internal/contracts"
)

func TestMergeFirstRegistryWins(t *testing.T) {
	builtIn := newSet("read_file", execShellCommand)
	builtIn.stream[execShellCommand] = true
	mcp := newSet("read_file", "mcp_only")
	// The MCP copy is the one a collision must not pick.
	mcp.tools["read_file"] = &fakeTool{
		info: toolInfo("read_file"),
		fn: func(context.Context, contracts.ToolRequest, contracts.Sink) contracts.Result {
			return contracts.Result{PlainText: "from mcp"}
		},
	}

	merged := Merge(builtIn, mcp)
	if got := len(merged.List()); got != 3 {
		t.Fatalf("merged list has %d tools, want 3", got)
	}
	tl, ok := merged.Get("read_file")
	if !ok {
		t.Fatal("read_file must survive the merge")
	}
	res := tl.Invoke(context.Background(), contracts.ToolRequest{Name: "read_file"}, nil)
	if res.PlainText != "ok" {
		t.Errorf("read_file came from the wrong registry: %+v", res)
	}
	if _, ok := merged.Get("mcp_only"); !ok {
		t.Error("a later registry keeps its unique tools")
	}

	// Names in the merged list, in registry order and without duplicates.
	var names []string
	for _, info := range merged.List() {
		names = append(names, info.Tool)
	}
	want := []string{"read_file", execShellCommand, "mcp_only"}
	if len(names) != len(want) {
		t.Fatalf("names = %v, want %v", names, want)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Errorf("name %d = %q, want %q", i, names[i], want[i])
		}
	}
}

func TestMergeKeepsDeclaredCapability(t *testing.T) {
	// silent says nothing about streaming, so the fallback rule applies to its
	// tools even after a merge. declared answers for its own tool.
	silent := quiet{newSet("mcp_tool", execShellCommand)}
	declared := quietSet{newSet("streaming_tool")}
	declared.s.stream["streaming_tool"] = true

	merged := Merge(silent, declared)
	if s, ok := merged.(streamer); !ok || !s.SupportsStream("streaming_tool") {
		t.Error("the owner registry must still answer for its own tool")
	}
	if s := merged.(streamer); s.SupportsStream("mcp_tool") {
		t.Error("a tool of a silent registry is not streamable")
	}
	// The fallback rule still holds through a merge.
	if s := merged.(streamer); !s.SupportsStream(execShellCommand) {
		t.Errorf("%s must stream by the fallback rule", execShellCommand)
	}
}

// quietSet exposes SupportsStream only when its own map says so.
type quietSet struct{ s *set }

func (q quietSet) List() []contracts.ToolInfo             { return q.s.List() }
func (q quietSet) Get(name string) (contracts.Tool, bool) { return q.s.Get(name) }
func (q quietSet) SupportsStream(name string) bool        { return q.s.stream[name] }

func TestMergeEmpty(t *testing.T) {
	merged := Merge()
	if len(merged.List()) != 0 {
		t.Error("Merge of nothing holds no tools")
	}
	if _, ok := merged.Get("read_file"); ok {
		t.Error("Get must fail on an empty merge")
	}
	// New turns that into the nil API, so the caller answers 403.
	if a := New(merged, nil); a != nil {
		t.Error("an empty merge must give a nil API")
	}
	b, err := json.Marshal(Merge(nil, nil).List())
	if err != nil || string(b) != "[]" {
		t.Errorf("List of an empty merge = %s (%v), want []", b, err)
	}
}

func TestMergeThroughAPI(t *testing.T) {
	builtIn := newSet("read_file")
	mcp := newSet("mcp_tool")
	a := New(Merge(builtIn, mcp), testLog(t))

	rec := get(t, a)
	var infos []contracts.ToolInfo
	if err := json.Unmarshal(rec.Body.Bytes(), &infos); err != nil {
		t.Fatalf("body = %s (%v)", rec.Body, err)
	}
	if len(infos) != 2 || infos[0].Tool != "mcp_tool" || infos[1].Tool != "read_file" {
		t.Errorf("list = %+v", infos)
	}

	rec = post(t, a, `{"tool":"mcp_tool"}`, nil)
	wantBody(t, rec, http.StatusOK, `{"plain_text_response":"ok"}`)
}

// ghost lists a tool it cannot hand back, which happens when a registry is
// updated between the two calls. Merge must skip it, not panic.
type ghost struct{}

func (ghost) List() []contracts.ToolInfo {
	return []contracts.ToolInfo{toolInfo("gone"), toolInfo("kept")}
}

func (ghost) Get(name string) (contracts.Tool, bool) {
	if name == "kept" {
		return &fakeTool{info: toolInfo("kept")}, true
	}
	return nil, false
}

func TestMergeSkipsToolWithoutImplementation(t *testing.T) {
	merged := Merge(ghost{})
	if _, ok := merged.Get("gone"); ok {
		t.Error("a tool that cannot be looked up must not be in the merge")
	}
	infos := merged.List()
	if len(infos) != 1 || infos[0].Tool != "kept" {
		t.Errorf("list = %+v, want only kept", infos)
	}
}

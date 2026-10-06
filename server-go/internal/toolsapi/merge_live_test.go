package toolsapi

import (
	"context"
	"testing"

	"llama-webui/server/internal/contracts"
)

// The merged registry the HTTP layer holds is built once, while an MCP server
// refreshes its own list when the server sends notifications/tools/list_changed.
// So the merge must ask the registries on every call: a revoked tool has to stop
// answering and a new one has to start, with no rebuild and no restart.
func TestMergeIsLiveInBothDirections(t *testing.T) {
	builtIn := newSet("read_file")
	mcpSet := newSet("alpha", "beta")
	merged := Merge(builtIn, mcpSet)

	if _, ok := merged.Get("beta"); !ok {
		t.Fatal("the setup is wrong: beta must be there to start with")
	}

	// A refresh that drops beta and adds gamma.
	dropTool(t, mcpSet, "beta")
	mcpSet.add(taggedTool("gamma", "gamma"))

	if _, ok := merged.Get("beta"); ok {
		t.Error("a revoked tool is still callable through the merge")
	}
	if _, ok := merged.Get("gamma"); !ok {
		t.Error("a new tool is not reachable through the merge")
	}

	// The order is the first registry's tools, then the additions of the later
	// ones, in the order they list them.
	want := []string{"read_file", "alpha", "gamma"}
	if got := toolNames(merged.List()); !equalNames(got, want) {
		t.Fatalf("List = %v, want %v", got, want)
	}

	// The same instance sees a second refresh, so nothing was cached in it.
	dropTool(t, mcpSet, "alpha")
	if _, ok := merged.Get("alpha"); ok {
		t.Error("the second refresh did not reach the merge")
	}
	if got := toolNames(merged.List()); !equalNames(got, []string{"read_file", "gamma"}) {
		t.Errorf("List = %v, want read_file and gamma", got)
	}
}

// First-registry-wins has to survive the liveness: a name the earlier registry
// holds is never answered from a later one, and the earlier copy is used again
// once the earlier registry has the name back.
func TestFirstWinsCollisionsStayStable(t *testing.T) {
	builtIn := newSet()
	builtIn.add(taggedTool("shared", "built-in"))
	mcpSet := newSet()
	mcpSet.add(taggedTool("shared", "from-mcp"))
	merged := Merge(builtIn, mcpSet)

	// Three identical answers: the view is recomputed, and stays the same.
	for i := 0; i < 3; i++ {
		if got := invokeFrom(t, merged, "shared"); got != "built-in" {
			t.Fatalf("call %d = %q, want the first registry's copy", i, got)
		}
	}

	// A refresh on the MCP side that re-declares the name must not steal it.
	dropTool(t, mcpSet, "shared")
	mcpSet.add(taggedTool("shared", "from-mcp-again"))
	if got := invokeFrom(t, merged, "shared"); got != "built-in" {
		t.Errorf("after the refresh = %q, want the first registry to keep it", got)
	}

	// When the built-in loses the name, the merged view answers from the MCP
	// copy. That is a live decision, not the one made at startup.
	dropTool(t, builtIn, "shared")
	if got := invokeFrom(t, merged, "shared"); got != "from-mcp-again" {
		t.Errorf("after the built-in dropped it = %q, want the MCP copy", got)
	}

	builtIn.add(taggedTool("shared", "built-in"))
	if got := invokeFrom(t, merged, "shared"); got != "built-in" {
		t.Errorf("after the built-in took it back = %q, want the first registry", got)
	}
}

// The registry that owns a name is the one that answers for streaming, so the
// answer has to follow the current owner.
func TestMergeStreamingFollowsTheOwner(t *testing.T) {
	silent := newSet("tool")
	declared := newSet()
	declared.stream["tool"] = true
	declared.add(taggedTool("tool", "declared"))

	// Only the first registry has it, and it says nothing about streaming, so the
	// fallback rule of the merged view applies to its tool.
	merged := Merge(silent, declared)
	if merged.(streamer).SupportsStream("tool") {
		t.Error("a silent owner must not be overridden by a later registry")
	}

	// The silent one loses it and the declaring one has it: streaming follows.
	dropTool(t, silent, "tool")
	if !merged.(streamer).SupportsStream("tool") {
		t.Error("SupportsStream must ask the current owner")
	}
}

// taggedTool answers with its tag, so a test can tell which registry served it.
func taggedTool(name, tag string) *fakeTool {
	return &fakeTool{
		info: toolInfo(name),
		fn: func(context.Context, contracts.ToolRequest, contracts.Sink) contracts.Result {
			return contracts.Result{PlainText: tag}
		},
	}
}

// invokeFrom is the tag of the tool that answered for name.
func invokeFrom(t *testing.T, reg interface {
	Get(string) (contracts.Tool, bool)
}, name string) string {
	t.Helper()
	tl, ok := reg.Get(name)
	if !ok {
		t.Fatalf("no tool %q in the merged view", name)
	}
	return tl.Invoke(context.Background(), contracts.ToolRequest{Name: name}, nil).PlainText
}

// dropTool takes a name out of a set the way an MCP refresh does.
func dropTool(t *testing.T, s *set, name string) {
	t.Helper()
	if _, ok := s.tools[name]; !ok {
		t.Fatalf("the fixture has no tool %q", name)
	}
	delete(s.tools, name)
	for i, n := range s.order {
		if n == name {
			s.order = append(s.order[:i], s.order[i+1:]...)
			return
		}
	}
}

func toolNames(infos []contracts.ToolInfo) []string {
	out := make([]string, 0, len(infos))
	for _, info := range infos {
		out = append(out, info.Tool)
	}
	return out
}

func equalNames(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

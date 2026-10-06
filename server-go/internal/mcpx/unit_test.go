package mcpx

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// defsFor mirrors list_tools: a tool with no name is not usable, and a schema
// that cannot be marshalled stays empty, which the definition then fills with {}.
func TestDefsForSkipsUnusableTools(t *testing.T) {
	defs := defsFor([]*mcp.Tool{
		nil,
		{Name: "", Description: "no name"},
		{Name: "ok", Description: "fine", InputSchema: map[string]any{"type": "object"}},
		{Name: "bad", InputSchema: make(chan int)},
	}, "srv")

	if len(defs) != 2 {
		t.Fatalf("got %d defs, want 2: %+v", len(defs), defs)
	}
	if defs[0].namespaced() != "srv_ok" {
		t.Errorf("first def is %q, want srv_ok", defs[0].namespaced())
	}
	if got := string(defs[0].inputSchema); got != `{"type":"object"}` {
		t.Errorf("inputSchema = %s", got)
	}
	// The namespaced name is what the webui sees, the bare name goes on the wire.
	if defs[0].name != "ok" || defs[0].description != "fine" {
		t.Errorf("first def = %+v", defs[0])
	}
	if defs[1].inputSchema != nil {
		t.Errorf("an unusable schema became %s", defs[1].inputSchema)
	}
	e := &entry{cfg: ServerConfig{Name: "srv"}}
	def := json.RawMessage(newTool(e, defs[1]).Info().Definition)
	if !json.Valid(def) {
		t.Errorf("definition for a bad schema is not json: %s", def)
	}
}

func TestHasStructured(t *testing.T) {
	cases := []struct {
		in   any
		want bool
	}{
		{nil, false},
		{json.RawMessage(nil), false},
		{json.RawMessage(`null`), false},
		{json.RawMessage(`{}`), true},
		{map[string]any{"a": 1}, true},
		{"text", true},
	}
	for _, tc := range cases {
		if got := hasStructured(tc.in); got != tc.want {
			t.Errorf("hasStructured(%#v) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

// An http server has no child, and the diagnostics must still work.
func TestNilChild(t *testing.T) {
	var ch *child
	if ch.pid() != 0 {
		t.Error("pid of no child is not 0")
	}
	var tr *tail
	if tr.String() != "" {
		t.Error("String of no tail is not empty")
	}
	if _, err := tr.Write([]byte("x")); err != nil {
		t.Error(err)
	}
	e := &entry{cfg: ServerConfig{Name: "web"}}
	if got := e.diagnosticsLocked(); got != "" {
		t.Errorf("diagnostics = %q, want empty", got)
	}
	e.last = "boom"
	e.child = &child{tail: &tail{max: 8}}
	e.child.tail.Write([]byte("abcdefghij"))
	if got := e.diagnosticsLocked(); got != "boom; last stderr: cdefghij" {
		t.Errorf("diagnostics = %q, want the error plus the trimmed tail", got)
	}
}

func TestChildEnvOverridesTheParent(t *testing.T) {
	t.Setenv("MCPX_TEST_PARENT", "parent-value")
	env := childEnv(ServerConfig{Env: map[string]string{"MCPX_TEST_PARENT": "child-value", "MCPX_TEST_ONLY": "x"}})
	var parent, only int
	for _, kv := range env {
		switch kv {
		case "MCPX_TEST_PARENT=child-value":
			parent++
		case "MCPX_TEST_ONLY=x":
			only++
		case "MCPX_TEST_PARENT=parent-value":
			t.Error("the parent value survived the override")
		}
	}
	if parent != 1 || only != 1 {
		t.Errorf("env = %v, want the override applied once", env)
	}
	// The base is the allowlist, so every allowlisted key the parent has is
	// still there next to the config entries.
	got := map[string]bool{}
	for _, kv := range env {
		got[kv] = true
	}
	for _, key := range childEnvKeys {
		if v, ok := os.LookupEnv(key); ok && !got[key+"="+v] {
			t.Errorf("env lost the allowlisted %s", key)
		}
	}
}

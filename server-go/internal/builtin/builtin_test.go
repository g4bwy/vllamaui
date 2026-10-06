package builtin

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"llama-webui/server/internal/contracts"
)

// newSet returns a registry with every tool, rooted at cwd.
func newSet(t *testing.T, cwd string) *Set {
	t.Helper()
	s, err := New(allToolNames(), cwd)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

func invoke(t *testing.T, s *Set, req contracts.ToolRequest, out contracts.Sink) contracts.Result {
	t.Helper()
	return invokeCtx(t, s, t.Context(), req, out)
}

func invokeCtx(t *testing.T, s *Set, ctx context.Context, req contracts.ToolRequest, out contracts.Sink) contracts.Result {
	t.Helper()
	tool, ok := s.Get(req.Name)
	if !ok {
		t.Fatalf("tool %q not registered", req.Name)
	}
	return tool.Invoke(ctx, req, out)
}

func call(t *testing.T, s *Set, cwd, name string, params map[string]any) contracts.Result {
	t.Helper()
	return invoke(t, s, contracts.ToolRequest{Name: name, Params: params, Cwd: cwd}, nil)
}

func TestNewAllTools(t *testing.T) {
	s, err := New([]string{"all"}, "")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	got := s.List()
	want := allToolNames()
	if len(got) != len(want) {
		t.Fatalf("got %d tools, want %d", len(got), len(want))
	}
	for i, name := range want {
		if got[i].Tool != name {
			t.Errorf("tool %d is %q, want %q", i, got[i].Tool, name)
		}
		if got[i].Type != "server" {
			t.Errorf("%s: type is %q, want server", name, got[i].Type)
		}
		if !got[i].UsesCwd {
			t.Errorf("%s: uses_cwd is false, want true", name)
		}
		if _, ok := s.Get(name); !ok {
			t.Errorf("Get(%q) not found", name)
		}
	}
}

func TestNewUnknownTool(t *testing.T) {
	want := `unknown tool "nope". available tools: read_file, file_glob_search, grep_search, ` +
		"exec_shell_command, write_file, edit_file, get_info"
	_, err := New([]string{"read_file", "nope"}, "")
	if err == nil {
		t.Fatal("expected an error for an unknown tool")
	}
	if err.Error() != want {
		t.Errorf("error is:\n%s\nwant:\n%s", err.Error(), want)
	}
}

func TestNewEmptySpec(t *testing.T) {
	s, err := New(nil, t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if len(s.List()) != 0 {
		t.Errorf("empty spec must register no tool, got %d", len(s.List()))
	}
	if _, ok := s.Get("read_file"); ok {
		t.Error("Get must fail when no tool is registered")
	}
}

func TestNewSubsetAndDuplicates(t *testing.T) {
	s, err := New([]string{"read_file", "read_file", "get_info"}, t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	var names []string
	for _, info := range s.List() {
		names = append(names, info.Tool)
	}
	if strings.Join(names, ",") != "read_file,get_info" {
		t.Errorf("names = %v, want read_file,get_info in build_tools order", names)
	}
}

func TestWritePermissionFlags(t *testing.T) {
	writable := map[string]bool{
		"exec_shell_command": true,
		"write_file":         true,
		"edit_file":          true,
	}
	s, err := New([]string{"all"}, "")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for _, info := range s.List() {
		if info.Permissions.Write != writable[info.Tool] {
			t.Errorf("%s: permissions.write = %v, want %v", info.Tool, info.Permissions.Write, writable[info.Tool])
		}
	}
}

// TestDefinitionMatchesInfo checks the name inside the OpenAI function object
// against the name Info reports, so the two can never drift apart.
func TestDefinitionMatchesInfo(t *testing.T) {
	s, err := New([]string{"all"}, "")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for _, info := range s.List() {
		var def struct {
			Type     string `json:"type"`
			Function struct {
				Name       string `json:"name"`
				Parameters struct {
					Type       string         `json:"type"`
					Properties map[string]any `json:"properties"`
					Required   []string       `json:"required"`
				} `json:"parameters"`
			} `json:"function"`
		}
		if err := json.Unmarshal(info.Definition, &def); err != nil {
			t.Fatalf("%s: bad definition json: %v", info.Tool, err)
		}
		if def.Type != "function" {
			t.Errorf("%s: definition.type = %q, want function", info.Tool, def.Type)
		}
		if def.Function.Name != info.Tool {
			t.Errorf("%s: definition name is %q, want %q", info.Tool, def.Function.Name, info.Tool)
		}
		if def.Function.Parameters.Type != "object" {
			t.Errorf("%s: parameters.type = %q, want object", info.Tool, def.Function.Parameters.Type)
		}
		if def.Function.Parameters.Properties == nil {
			t.Errorf("%s: parameters.properties missing", info.Tool)
		}
		if info.DisplayName == "" {
			t.Errorf("%s: display_name is empty", info.Tool)
		}
	}
}

func TestGetInfoDefinitionHasNoParams(t *testing.T) {
	s, err := New([]string{"get_info"}, t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	var def map[string]any
	if err := json.Unmarshal(s.List()[0].Definition, &def); err != nil {
		t.Fatal(err)
	}
	fn := def["function"].(map[string]any)
	params := fn["parameters"].(map[string]any)
	if _, has := params["required"]; has {
		t.Error("get_info must not declare required parameters, llama-server does not")
	}
	props, ok := params["properties"].(map[string]any)
	if !ok || len(props) != 0 {
		t.Errorf("get_info properties = %v, want an empty object", params["properties"])
	}
}

func TestMissingAndBadTypedParamsNeverPanic(t *testing.T) {
	cwd := t.TempDir()
	s := newSet(t, cwd)

	cases := []struct {
		name   string
		tool   string
		params map[string]any
		want   string
	}{
		{"read no path", "read_file", map[string]any{}, "key 'path' not found"},
		{"read null path", "read_file", map[string]any{"path": nil}, "type must be string, but is null"},
		{"glob no path", "file_glob_search", map[string]any{}, "key 'path' not found"},
		{"grep no pattern", "grep_search", map[string]any{"path": "x"}, "key 'pattern' not found"},
		{"edit no edits", "edit_file", map[string]any{"path": "x"}, "key 'edits' not found"},
		{"write no content", "write_file", map[string]any{"path": "x"}, "key 'content' not found"},
		{"exec no command", "exec_shell_command", map[string]any{}, "key 'command' not found"},
		{"path is a number", "read_file", map[string]any{"path": float64(3)}, "type must be string, but is number"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := call(t, s, cwd, tc.tool, tc.params)
			if res.Error != tc.want {
				t.Errorf("error = %q, want %q", res.Error, tc.want)
			}
		})
	}
}

func TestOptionalParamsFallBackOnBadType(t *testing.T) {
	cwd := t.TempDir()
	s := newSet(t, cwd)
	writeTestFile(t, cwd, "a.txt", "one\ntwo\n")

	// a non-number start_line is ignored, exactly like json_value()
	res := call(t, s, cwd, "read_file", map[string]any{"path": "a.txt", "start_line": "first"})
	if res.Error != "" {
		t.Fatalf("unexpected error: %s", res.Error)
	}
	if res.PlainText != "one\ntwo\n" {
		t.Errorf("plain text = %q, want the whole file", res.PlainText)
	}
}

// TestParamCoercion covers the conversions json_value() applies: any JSON
// number feeds an integer parameter, anything else falls back to the default.
func TestParamCoercion(t *testing.T) {
	cases := []struct {
		key   string
		value any
		want  int
	}{
		{"missing", nil, 7},
		{"explicit null", nil, 7},
		{"integer", float64(3), 3},
		{"float truncates toward zero", 4.9, 4},
		{"negative float", -2.7, -2},
		{"native int", 11, 11},
		{"native int64", int64(12), 12},
		{"json.Number", json.Number("13"), 13},
		{"json.Number float", json.Number("13.8"), 13},
		{"string is not a number", "5", 7},
		{"bool is not a number", true, 7},
		{"array is not a number", []any{5}, 7},
		{"object is not a number", map[string]any{"a": 1}, 7},
	}
	for _, tc := range cases {
		t.Run(tc.key, func(t *testing.T) {
			params := map[string]any{"n": tc.value}
			if tc.key == "missing" {
				params = map[string]any{}
			}
			if got := optInt(params, "n", 7); got != tc.want {
				t.Errorf("optInt = %d, want %d", got, tc.want)
			}
		})
	}

	if got := optString(map[string]any{"s": 5}, "s", "dflt"); got != "dflt" {
		t.Errorf("optString on a number = %q, want the default", got)
	}
	if got := optBool(map[string]any{"b": "yes"}, "b", true); !got {
		t.Error("optBool on a string should keep the default")
	}

	// the messages a caller sees for a value of the wrong shape
	for _, tc := range []struct {
		value any
		want  string
	}{
		{nil, "type must be string, but is null"},
		{true, "type must be string, but is boolean"},
		{float64(1), "type must be string, but is number"},
		{"5", ""}, // a string is the right type, so no message
		{[]any{}, "type must be string, but is array"},
		{map[string]any{}, "type must be string, but is object"},
	} {
		_, msg := reqString(map[string]any{"p": tc.value}, "p")
		if msg != tc.want {
			t.Errorf("reqString(%#v) message = %q, want %q", tc.value, msg, tc.want)
		}
	}
	if _, msg := reqString(map[string]any{}, "p"); msg != "key 'p' not found" {
		t.Errorf("missing key message = %q", msg)
	}
}

// TestSupportsStream: only exec_shell_command may stream, so the HTTP layer
// can refuse the rest the way find_tool() does.
func TestSupportsStream(t *testing.T) {
	s, err := New([]string{"all"}, t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for _, info := range s.List() {
		want := info.Tool == "exec_shell_command"
		if got := s.SupportsStream(info.Tool); got != want {
			t.Errorf("SupportsStream(%q) = %v, want %v", info.Tool, got, want)
		}
	}
}

// TestResultWireShape checks what each result kind turns into on the wire.
func TestResultWireShape(t *testing.T) {
	cases := []struct {
		name string
		res  contracts.Result
		want string
	}{
		{"error wins", contracts.Result{Error: "boom", PlainText: "x"}, `{"error":"boom"}`},
		{"plain text", contracts.Result{PlainText: "hi\n"}, `{"plain_text_response":"hi\n"}`},
		{"empty", contracts.Result{}, `{"plain_text_response":""}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := string(tc.res.JSON()); got != tc.want {
				t.Errorf("json = %s, want %s", got, tc.want)
			}
		})
	}
}

func writeTestFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o666); err != nil {
		t.Fatal(err)
	}
	return path
}

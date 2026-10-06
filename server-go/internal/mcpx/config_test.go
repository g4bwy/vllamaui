package mcpx

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"llama-webui/server/internal/contracts"
)

func TestLoadMergesFileAndInline(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "mcp.json")
	writeFile(t, file, `{
	  "mcpServers": {
	    "fs": {
	      "command": "/bin/fs-server",
	      "args": ["--stdio", "x"],
	      "env": {"TOKEN": "abc"},
	      "cwd": "/tmp",
	      "timeout_ms": 1500
	    }
	  },
	  "other": true
	}`)
	inline := `{"mcpServers":{"web":{"command":"npx","args":["-y","web"]}}}`

	rec := &recorder{}
	cfg, err := Load(file, inline, rec.logger())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.Servers) != 2 {
		t.Fatalf("got %d servers, want 2: %+v", len(cfg.Servers), cfg.Servers)
	}
	if cfg.Empty() {
		t.Error("Empty is true for a config with servers")
	}
	fs := cfg.Servers[0]
	if fs.Name != "fs" || fs.Command != "/bin/fs-server" {
		t.Errorf("first server is %+v, want fs", fs)
	}
	if strings.Join(fs.Args, "|") != "--stdio|x" {
		t.Errorf("args = %v, want --stdio and x", fs.Args)
	}
	if fs.Env["TOKEN"] != "abc" {
		t.Errorf("env = %v, want TOKEN=abc", fs.Env)
	}
	if fs.Cwd != "/tmp" {
		t.Errorf("cwd = %q, want /tmp", fs.Cwd)
	}
	if fs.TimeoutMS != 1500 {
		t.Errorf("timeout_ms = %d, want 1500", fs.TimeoutMS)
	}
	if fs.Transport() != "stdio" {
		t.Errorf("transport = %q, want stdio", fs.Transport())
	}
	web := cfg.Servers[1]
	if web.Name != "web" || web.TimeoutMS != DefaultTimeoutMS {
		t.Errorf("inline server = %+v, want timeout_ms %d", web, DefaultTimeoutMS)
	}
	if len(web.Args) != 2 || web.Args[0] != "-y" {
		t.Errorf("inline args = %v", web.Args)
	}
}

func TestLoadDropsInlineDuplicate(t *testing.T) {
	file := filepath.Join(t.TempDir(), "mcp.json")
	writeFile(t, file, `{"mcpServers":{"dup":{"command":"from-file"},"only":{"command":"o"}}}`)

	rec := &recorder{}
	cfg, err := Load(file, `{"mcpServers":{"dup":{"command":"from-inline"},"new":{"command":"other"}}}`, rec.logger())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.Servers) != 3 {
		t.Fatalf("got %v, want dup, new and only", cfg.Servers)
	}
	if cfg.Servers[0].Command != "from-file" {
		t.Errorf("dup came from %q, the file must win", cfg.Servers[0].Command)
	}
	if rec.has("MCP config: duplicate server name 'dup'") {
		if !rec.has("skipping") {
			t.Error("the duplicate line does not say skipping")
		}
	} else {
		t.Error("missing the duplicate warning")
		rec.dump(t)
	}
}

func TestLoadSkipsEmptyCommand(t *testing.T) {
	rec := &recorder{}
	cfg, err := Load("", `{"mcpServers":{"good":{"command":"x"},"bad":{"command":""},"nokey":{"args":["a"]}}}`, rec.logger())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.Servers) != 1 || cfg.Servers[0].Name != "good" {
		t.Fatalf("got %v, want only good", cfg.Servers)
	}
	for _, name := range []string{"bad", "nokey"} {
		if !rec.has("MCP server '" + name + "' has no command, skipping") {
			t.Errorf("missing the no-command warning for %s", name)
			rec.dump(t)
		}
	}
}

func TestLoadMissingMCPServersKey(t *testing.T) {
	for _, body := range []string{`{"a":1}`, `{}`, `[]`, `"text"`, `{"mcpServers":[]}`, `{"mcpServers":null}`} {
		rec := &recorder{}
		cfg, err := Load("", body, rec.logger())
		if err != nil {
			t.Fatalf("Load(%s): %v", body, err)
		}
		if len(cfg.Servers) != 0 || !cfg.Empty() {
			t.Errorf("Load(%s) = %v, want zero servers", body, cfg.Servers)
		}
		if !rec.has(msgNoServers) {
			t.Errorf("Load(%s) did not log %q", body, msgNoServers)
		}
	}
}

func TestLoadFatalErrors(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "good.json")
	writeFile(t, good, `{"mcpServers":{"a":{"command":"x"}}}`)
	sub := filepath.Join(dir, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name   string
		file   string
		inline string
		want   string
	}{
		{"bad json", "", `{"mcpServers": {"a": }}`, prefixParseConfig},
		{"not json", "", `nope`, prefixParseConfig},
		{"empty string", "", ` `, prefixParseConfig},
		{"wrong command type", "", `{"mcpServers":{"a":{"command":5}}}`, prefixParseConfig},
		{"bad args element", "", `{"mcpServers":{"a":{"command":"x","args":[1]}}}`, prefixParseConfig},
		{"bad env value", "", `{"mcpServers":{"a":{"command":"x","env":{"k":2}}}}`, prefixParseConfig},
		{"bad entry", "", `{"mcpServers":{"a":5}}`, prefixParseConfig},
		{"missing file", filepath.Join(dir, "nope.json"), "", prefixOpenConfig},
		{"directory as file", sub, "", prefixOpenConfig},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(tc.file, tc.inline, nil)
			if err == nil {
				t.Fatalf("Load(%q, %q) succeeded, want an error", tc.file, tc.inline)
			}
			if !strings.HasPrefix(err.Error(), tc.want) {
				t.Errorf("error = %q, want prefix %q", err, tc.want)
			}
		})
	}
	if _, err := Load(good, "", nil); err != nil {
		t.Errorf("the good file must load: %v", err)
	}
	// A bad inline string still fails when the file is fine.
	if _, err := Load(good, `{{`, nil); !strings.HasPrefix(err.Error(), prefixParseConfig) {
		t.Errorf("inline error = %v, want the parse prefix", err)
	}
}

// The C++ ignores an args or env value of the wrong kind instead of failing, and
// it casts timeout_ms to int.
func TestLoadIgnoresWrongShapeForArgsAndEnv(t *testing.T) {
	cfg, err := Load("", `{"mcpServers":{"a":{"command":"x","args":"nope","env":["nope"],"timeout_ms":2500.9}}}`, nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.Servers) != 1 {
		t.Fatalf("got %v, want one server", cfg.Servers)
	}
	s := cfg.Servers[0]
	if s.Args != nil || s.Env != nil {
		t.Fatalf("got args %v env %v, want both dropped", s.Args, s.Env)
	}
	if s.TimeoutMS != 2500 {
		t.Errorf("timeout_ms = %d, want the fraction cut off", s.TimeoutMS)
	}
}

func TestLoadKeepsServersSortedByName(t *testing.T) {
	cfg, err := Load("", `{"mcpServers":{"zeta":{"command":"z"},"alpha":{"command":"a"},"mid":{"command":"m"}}}`, nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	var got []string
	for _, s := range cfg.Servers {
		got = append(got, s.Name)
	}
	if strings.Join(got, ",") != "alpha,mid,zeta" {
		t.Errorf("order = %v, want the C++ std::map order", got)
	}
}

func TestLoadURLKeepsEntryWithoutCommand(t *testing.T) {
	cfg, err := Load("", `{"mcpServers":{"remote":{"url":"http://127.0.0.1:9/mcp"},"ftp":{"url":"ftp://x"}}}`, nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.Servers) != 2 {
		t.Fatalf("got %v, want both entries", cfg.Servers)
	}
	if cfg.Servers[0].Transport() != "http" {
		t.Errorf("transport = %q, want http", cfg.Servers[0].Transport())
	}
	ftp := Config{}
	for _, s := range cfg.Servers {
		if s.Name == "ftp" {
			ftp.Servers = append(ftp.Servers, s)
		}
	}
	rec := &recorder{}
	c, err := New(t.Context(), ftp, &Options{Logger: rec.logger()})
	if err == nil {
		c.Shutdown()
		t.Fatal("New took a url that is not http or https")
	}
	if !strings.Contains(err.Error(), "ftp") {
		t.Errorf("error %v should name the bad server", err)
	}
}

// A hand built Config cannot repeat a name: Load folds duplicates away, and New
// refuses the collision instead of quietly sharing one entry.
func TestNewRejectsDuplicateServerNames(t *testing.T) {
	sc := ServerConfig{Name: "dup", Command: fixtureBin, TimeoutMS: 1000}
	if _, err := New(t.Context(), Config{Servers: []ServerConfig{sc, sc}}, nil); err == nil {
		t.Error("New took two servers with the same name")
	} else if !strings.Contains(err.Error(), "duplicate server name 'dup'") {
		t.Errorf("New = %v", err)
	}
}

func TestToolDefinitionSerialization(t *testing.T) {
	e := &entry{cfg: ServerConfig{Name: "fs"}}
	d := toolDef{
		serverName:  "fs",
		name:        "read_file",
		description: "Read a file.",
		inputSchema: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}`),
	}
	got := string(newTool(e, d).Info().Definition)
	want := `{"type":"function","function":{"name":"fs_read_file","description":"Read a file.","parameters":{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}}}`
	if got != want {
		t.Errorf("definition\n     got %s\n    want %s", got, want)
	}

	// A description that is absent stays an empty string, and a schema that is
	// missing, null, or not an object becomes {}.
	for _, schema := range []string{"", "null", `"x"`, `[1]`} {
		d.inputSchema = json.RawMessage(schema)
		def := newTool(e, d).Info().Definition
		var parsed struct {
			Type     string `json:"type"`
			Function struct {
				Name        string          `json:"name"`
				Description string          `json:"description"`
				Parameters  json.RawMessage `json:"parameters"`
			} `json:"function"`
		}
		if err := json.Unmarshal(def, &parsed); err != nil {
			t.Fatalf("schema %q: %s is not json: %v", schema, def, err)
		}
		if parsed.Type != "function" || parsed.Function.Name != "fs_read_file" {
			t.Errorf("schema %q: got %+v", schema, parsed)
		}
		if string(parsed.Function.Parameters) != "{}" {
			t.Errorf("schema %q: parameters = %s, want {}", schema, parsed.Function.Parameters)
		}
	}
}

func TestToolInfoFields(t *testing.T) {
	e := &entry{cfg: ServerConfig{Name: "fs"}}
	info := newTool(e, toolDef{serverName: "fs", name: "ls"}).Info()

	raw, err := json.Marshal([]contracts.ToolInfo{info})
	if err != nil {
		t.Fatal(err)
	}
	var out []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	// display_name, tool, type, permissions.write and uses_cwd are what
	// server_tool::to_json writes.
	for k, want := range map[string]string{
		"tool":         `"fs_ls"`,
		"type":         `"mcp"`,
		"display_name": `"fs_ls"`,
		"uses_cwd":     `false`,
		"definition":   `{"type":"function","function":{"name":"fs_ls","description":"","parameters":{}}}`,
	} {
		if got := string(out[0][k]); got != want {
			t.Errorf("%s = %s, want %s", k, got, want)
		}
	}
	for _, key := range []string{"display_name", "tool", "type", "permissions", "uses_cwd", "definition"} {
		if out[0][key] == nil {
			t.Errorf("GET /tools field %q is missing", key)
		}
	}
	var perms struct {
		Write bool `json:"write"`
	}
	if err := json.Unmarshal(out[0]["permissions"], &perms); err != nil {
		t.Fatal(err)
	}
	if perms.Write {
		t.Error("permissions.write must be false for an MCP tool")
	}
}

func TestNamespaceAndCollision(t *testing.T) {
	c, _ := newClient(t, fixtureConfig("fx"))
	got := toolNames(c)
	if len(got) == 0 {
		t.Fatal("no tools discovered")
	}
	for _, name := range got {
		if !strings.HasPrefix(name, "fx_") {
			t.Errorf("tool %q has no server prefix", name)
		}
	}
	mustHaveTool(t, c, "fx_echo")
	if tool, _ := c.Get("fx_echo"); tool.Info().Tool != "fx_echo" {
		t.Errorf("Info().Tool = %q", tool.Info().Tool)
	}
	// A call sends the bare name, so the fixture must answer.
	if res := call(t, c, "fx_echo", map[string]any{"text": "hi"}); res.Error != "" || res.PlainText != "hi" {
		t.Errorf("call = %+v, want plain_text_response hi", res)
	}

	// A name the caller said is taken must not be registered.
	rec := &recorder{}
	c2, err := New(t.Context(), fixtureConfig("fx"), &Options{Logger: rec.logger(), ReservedNames: []string{"fx_echo", "fx_pid"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c2.Shutdown)
	if _, ok := c2.Get("fx_echo"); ok {
		t.Error("fx_echo was registered over a reserved name")
	}
	if _, ok := c2.Get("fx_pid"); ok {
		t.Error("fx_pid was registered over a reserved name")
	}
	mustHaveTool(t, c2, "fx_slow")
	for _, name := range []string{"fx_echo", "fx_pid"} {
		if !rec.hasAll("collides with an existing tool, skipping", fmt.Sprintf("%q", name), `"fx"`) {
			t.Errorf("missing the collision line for %s", name)
			rec.dump(t)
		}
	}

	// Two servers with the same prefixed tool name: the first wins, the second
	// is skipped. That needs two server names that produce one tool name, so
	// register one of them by hand against the other.
	c3, rec3 := newClient(t, fixtureConfig("fx"))
	if n := c3.replace("fx", []toolDef{{serverName: "fx", name: "echo"}, {serverName: "fx", name: "echo"}}); n != 1 {
		t.Errorf("replace added %d tools, want 1", n)
	}
	if !rec3.hasAll("collides with an existing tool", `"fx_echo"`) {
		t.Error("missing the collision line for a repeated tool name")
		rec3.dump(t)
	}
}

// replace must not touch the tools of another server.
func TestReplaceKeepsOtherServers(t *testing.T) {
	c := &Client{
		log:      slog.New(slog.DiscardHandler),
		tools:    map[string]*tool{},
		reserved: map[string]bool{},
	}
	c.entries = map[string]*entry{
		"a": {c: c, cfg: ServerConfig{Name: "a"}},
		"b": {c: c, cfg: ServerConfig{Name: "b"}},
	}
	for _, d := range []toolDef{{serverName: "a", name: "x"}, {serverName: "b", name: "y"}} {
		c.register(d)
	}
	c.replace("a", []toolDef{{serverName: "a", name: "z"}})
	var names []string
	for _, info := range c.List() {
		names = append(names, info.Tool)
	}
	if strings.Join(names, ",") != "b_y,a_z" {
		t.Errorf("order after replace = %v, want b_y then a_z", names)
	}
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

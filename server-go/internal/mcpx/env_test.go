package mcpx

import (
	"log/slog"
	"os"
	"testing"
)

// A stdio server is often a third-party package that the config asks uvx or npx
// to fetch and run. It must not read the web server's secrets. The default is an
// allowlist of the parent environment, and only "inherit_env": true hands the
// child everything.
func TestStdioChildEnvIsAnAllowlistUnlessInheritEnv(t *testing.T) {
	t.Setenv("UPSTREAM_API_KEY", "super-secret")
	t.Setenv("MCPX_TEST_OUTSIDE_ALLOWLIST", "keep-it-out")
	if _, ok := os.LookupEnv("PATH"); !ok {
		t.Skip("the suite needs a PATH")
	}

	cases := []struct {
		name       string
		inheritEnv bool
		wantSecret bool
	}{
		{"default", false, false},
		{"inherit_env", true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := fixtureConfig("fx")
			cfg.Servers[0].Env = map[string]string{"MCPX_TEST_BANNER": "hello"}
			cfg.Servers[0].InheritEnv = tc.inheritEnv
			c, _ := newClient(t, cfg)

			e := c.entries["fx"]
			if e == nil {
				t.Fatal("no entry for the fixture server")
			}
			if !waitUntil(func() bool { return e.child != nil && e.child.cmd != nil }) {
				t.Fatal("no child was spawned")
			}
			got := envMap(e.child.cmd.Env)
			_, hasSecret := got["UPSTREAM_API_KEY"]
			if hasSecret != tc.wantSecret {
				t.Errorf("UPSTREAM_API_KEY reached the child = %v, want %v", hasSecret, tc.wantSecret)
			}
			if !tc.wantSecret {
				if _, ok := got["MCPX_TEST_OUTSIDE_ALLOWLIST"]; ok {
					t.Error("a default child inherited a variable outside the allowlist")
				}
			}
			if got["PATH"] == "" {
				t.Error("the child lost PATH, so it could not run anything")
			}
			if got["MCPX_TEST_BANNER"] != "hello" {
				t.Errorf("config env = %q, want the entry to reach the child", got["MCPX_TEST_BANNER"])
			}
		})
	}
}

// childEnv on its own: the allowlist and the config entries by default, the
// whole parent under inherit_env, and a config entry always wins.
func TestChildEnvAllowlistAndOverrides(t *testing.T) {
	t.Setenv("UPSTREAM_API_KEY", "super-secret")
	t.Setenv("PATH", "/usr/bin")
	t.Setenv("MCPX_TEST_OVERRIDE", "parent")

	got := envMap(childEnv(ServerConfig{Env: map[string]string{
		"MCPX_TEST_OVERRIDE": "child",
		"MCPX_TEST_EXTRA":    "x",
	}}))
	if _, ok := got["UPSTREAM_API_KEY"]; ok {
		t.Error("the default child environment carries a secret")
	}
	if got["PATH"] != "/usr/bin" {
		t.Error("the default child environment lost an allowlisted variable")
	}
	if got["MCPX_TEST_OVERRIDE"] != "child" {
		t.Errorf("override = %q, want the config value to win", got["MCPX_TEST_OVERRIDE"])
	}
	if got["MCPX_TEST_EXTRA"] != "x" {
		t.Error("the config entry did not reach the child")
	}

	full := envMap(childEnv(ServerConfig{InheritEnv: true}))
	if full["UPSTREAM_API_KEY"] != "super-secret" {
		t.Error("inherit_env did not carry the parent environment")
	}
}

// TestInheritEnvJSONKey is the config half of the switch: the key parses, and an
// entry without it stays on the safe default.
func TestInheritEnvJSONKey(t *testing.T) {
	cfg, err := parseCursorJSON([]byte(`{"mcpServers":{
		"plain":{"command":"cat"},
		"full":{"command":"cat","inherit_env":true}
	}}`), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	byName := map[string]ServerConfig{}
	for _, s := range cfg {
		byName[s.Name] = s
	}
	if byName["plain"].InheritEnv {
		t.Error("a server without the key must not inherit the parent environment")
	}
	if !byName["full"].InheritEnv {
		t.Error("inherit_env:true did not reach ServerConfig")
	}
}

// envMap indexes an environment slice by variable name.
func envMap(env []string) map[string]string {
	out := make(map[string]string, len(env))
	for _, kv := range env {
		for i := 0; i < len(kv); i++ {
			if kv[i] == '=' {
				out[kv[:i]] = kv[i+1:]
				break
			}
		}
	}
	return out
}

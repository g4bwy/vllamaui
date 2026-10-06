package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"llama-webui/server/internal/appconf"
	"llama-webui/server/internal/builtin"
	"llama-webui/server/internal/contracts"
)

// toolList makes a registry list where the names in writes are flagged write=true.
func toolList(reads, writes []string) []contracts.ToolInfo {
	var out []contracts.ToolInfo
	add := func(name string, write bool) {
		info := contracts.ToolInfo{Tool: name, Type: "server"}
		info.Permissions.Write = write
		out = append(out, info)
	}
	for _, n := range reads {
		add(n, false)
	}
	for _, n := range writes {
		add(n, true)
	}
	return out
}

func TestToolsGuard(t *testing.T) {
	powerful := toolList([]string{"read_file"}, []string{"exec_shell_command", "write_file"})

	// A key is set, so the tools are reachable only by someone who has it.
	warn, err := toolsGuard(&appconf.Config{InboundKey: "k", Host: "127.0.0.1", Port: 8080}, powerful)
	if err != nil || warn != "" {
		t.Errorf("with a key: warn=%q err=%v, want neither", warn, err)
	}

	// No key and no opt-out: startup stops, and the message names the tools and
	// both ways out.
	warn, err = toolsGuard(&appconf.Config{Host: "127.0.0.1", Port: 8080}, powerful)
	if err == nil {
		t.Fatal("a write tool with no key must stop the server")
	}
	if warn != "" {
		t.Errorf("warn = %q, want the error path only", warn)
	}
	for _, want := range []string{"exec_shell_command", "write_file", "--require-api-key", "--allow-unauthenticated-tools"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error must name %s: %s", want, err)
		}
	}
	// A read-only list needs no key at all.
	if warn, err := toolsGuard(&appconf.Config{Host: "127.0.0.1", Port: 8080}, toolList([]string{"read_file", "grep_search"}, nil)); err != nil || warn != "" {
		t.Errorf("read-only tools: warn=%q err=%v, want neither", warn, err)
	}
	// Nor does a server with no tools.
	if warn, err := toolsGuard(&appconf.Config{}, nil); err != nil || warn != "" {
		t.Errorf("no tools: warn=%q err=%v, want neither", warn, err)
	}
}

// TestToolsGuardOptOut: --allow-unauthenticated-tools starts the server, and the
// boot log says loudly what was accepted.
func TestToolsGuardOptOut(t *testing.T) {
	warn, err := toolsGuard(&appconf.Config{
		Host: "127.0.0.1", Port: 8080, AllowUnauthenticatedTools: true,
	}, toolList(nil, []string{"edit_file"}))
	if err != nil {
		t.Fatalf("the opt-out must start the server, got %v", err)
	}
	if !strings.HasPrefix(warn, "  WARNING   ") {
		t.Errorf("warn = %q, want a boot note in the aligned style", warn)
	}
	for _, want := range []string{"edit_file", "127.0.0.1:8080", "api key"} {
		if !strings.Contains(warn, want) {
			t.Errorf("warn must mention %s: %q", want, warn)
		}
	}
}

// TestToolsGuardReadsTheRegistry: the write flag is what the built-in tools
// report about themselves, so a guard built on it cannot drift from the registry.
func TestToolsGuardReadsTheRegistry(t *testing.T) {
	set, err := builtin.New([]string{"exec_shell_command", "read_file"}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := toolsGuard(&appconf.Config{Host: "127.0.0.1", Port: 8080}, set.List()); err == nil {
		t.Error("exec_shell_command must be flagged write=true by the registry")
	}

	reads, err := builtin.New([]string{"read_file", "grep_search"}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := toolsGuard(&appconf.Config{Host: "127.0.0.1", Port: 8080}, reads.List()); err != nil {
		t.Errorf("read-only built-ins must not need a key: %v", err)
	}
}

func TestDefaultOrigins(t *testing.T) {
	// The loopback default: the port answers on both spellings of localhost, and
	// listing one twice would add nothing.
	got := defaultOrigins(&appconf.Config{Host: "127.0.0.1", Port: 8080})
	want := []string{"http://127.0.0.1:8080"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("origins = %v, want %v", got, want)
	}

	// A wildcard bind is what an operator asks for by name, and the page is then
	// opened by some other address, so both are allowed.
	got = defaultOrigins(&appconf.Config{Host: "0.0.0.0", Port: 9})
	want = []string{"http://localhost:9", "http://127.0.0.1:9"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("origins = %v, want %v", got, want)
	}

	got = defaultOrigins(&appconf.Config{Host: "10.0.0.5", Port: 8080})
	want = []string{"http://10.0.0.5:8080", "http://127.0.0.1:8080"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("origins = %v, want %v", got, want)
	}
}

// requestWith makes a request carrying one header, for the key check.
func requestWith(name, value string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/cors-proxy", nil)
	if name != "" {
		r.Header.Set(name, value)
	}
	return r
}

// TestAuthorizedAcceptsBothHeaderForms: /tools and /cors-proxy share this check,
// so a gate wired for one cannot be missing the other's header.
func TestAuthorizedAcceptsBothHeaderForms(t *testing.T) {
	allow := authorized("secret")
	if !allow(requestWith("X-Api-Key", "secret")) {
		t.Error("X-Api-Key must be accepted")
	}
	if !allow(requestWith("Authorization", "Bearer secret")) {
		t.Error("a bearer token must be accepted")
	}
	if allow(requestWith("X-Api-Key", "wrong")) {
		t.Error("a wrong key must be refused")
	}
	if allow(requestWith("", "")) {
		t.Error("no credentials must be refused")
	}
}

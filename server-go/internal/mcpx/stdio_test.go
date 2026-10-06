package mcpx

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"llama-webui/server/internal/contracts"
)

// The whole path for real: a subprocess, NDJSON over stdin and stdout, the SDK
// on the other end, and a shutdown that leaves no child behind.
func TestStdioEndToEnd(t *testing.T) {
	before := runtime.NumGoroutine()

	dir := t.TempDir()
	cfg := fixtureConfig("fx")
	cfg.Servers[0].Cwd = dir
	cfg.Servers[0].Env = map[string]string{"MCPX_TEST_BANNER": "hello"}

	c, rec := newClient(t, cfg)
	if !rec.has(fmt.Sprintf(msgWarmup, "fx", 8)) {
		t.Errorf("no warmup line, log:\n%s", strings.Join(rec.lines, "\n"))
	}

	want := "fx_add_late,fx_boom,fx_counter,fx_echo,fx_hasimage,fx_multitext,fx_pid,fx_slow"
	if got := strings.Join(toolNames(c), ","); got != want {
		t.Errorf("tools = %s\n   want %s", got, want)
	}

	// The banner the fixture writes to stderr proves that env and cwd reached
	// the child, and that the tail keeps it for diagnostics.
	e := c.entries["fx"]
	if !waitUntil(func() bool { return e.child != nil && strings.Contains(e.child.tail.String(), "banner=hello") }) {
		t.Errorf("stderr tail = %q, want the banner", e.diagnosticsLocked())
	}
	if got := e.child.tail.String(); !strings.Contains(got, "cwd="+dir) {
		t.Errorf("stderr tail = %q, want cwd %s", got, dir)
	}

	if res := call(t, c, "fx_echo", map[string]any{"text": "ping"}); res.PlainText != "ping" || res.Error != "" {
		t.Errorf("echo = %+v", res)
	}
	if res := call(t, c, "fx_multitext", nil); res.PlainText != "one\ntwo" {
		t.Errorf("multitext = %+v, want one and two on two lines", res)
	}
	if res := call(t, c, "fx_boom", nil); res.Error != "boom: it went wrong" {
		t.Errorf("boom = %+v", res)
	}

	// The image part is dropped, and the drop is reported with the tool name.
	if res := call(t, c, "fx_hasimage", nil); res.PlainText != "here is a picture" {
		t.Errorf("hasimage = %+v", res)
	}
	if !rec.hasAll("fx_hasimage", "discarded 1 non-text content part") {
		t.Error("hasimage did not report the dropped part")
		rec.dump(t)
	}
	if n := rec.count("discarded"); n != 1 {
		t.Errorf("got %d discard lines, want 1", n)
	}

	// Both calls run on the same child, so the counter moves.
	first := call(t, c, "fx_counter", nil)
	second := call(t, c, "fx_counter", nil)
	if first.Error != "" || second.Error != "" {
		t.Fatalf("counter = %+v then %+v", first, second)
	}
	if first.PlainText == second.PlainText {
		t.Errorf("counter did not move: %q then %q", first.PlainText, second.PlainText)
	}

	for _, name := range []string{"fx_add_late", "fx_boom", "fx_counter", "fx_echo", "fx_hasimage", "fx_multitext", "fx_pid", "fx_slow"} {
		def := string(mustGet(t, c, name).Info().Definition)
		if !strings.Contains(def, `"name":"`+name+`"`) {
			t.Errorf("%s definition = %s", name, def)
		}
	}

	pid := e.child.pid()
	c.Shutdown()
	mustBeDead(t, pid)
	if state := procState(pid); state == 'Z' {
		t.Errorf("child %d is a zombie, nobody reaped it", pid)
	}
	if !waitUntil(func() bool { return runtime.NumGoroutine() <= before }) {
		t.Errorf("goroutines went from %d to %d", before, runtime.NumGoroutine())
	}
}

// The list_changed notification works over stdio too, on the session that a
// call left open.
func TestToolListChangedOverStdio(t *testing.T) {
	c, rec := newClient(t, fixtureConfig("fx"))
	if _, ok := c.Get("fx_late"); ok {
		t.Fatal("fx_late is there before add_late ran")
	}
	if res := call(t, c, "fx_add_late", nil); res.PlainText != "added" {
		t.Fatalf("add_late = %+v", res)
	}
	if !waitUntil(func() bool { _, ok := c.Get("fx_late"); return ok }) {
		t.Error("fx_late never appeared after the notification")
		rec.dump(t)
	}
	if res := call(t, c, "fx_late", nil); res.PlainText != "late answer" {
		t.Errorf("fx_late = %+v", res)
	}
	if !rec.has("rediscovered 9 tools") {
		t.Error("no line says the tool list was refreshed")
	}
	// The tools that were there before are still there.
	mustHaveTool(t, c, "fx_echo")
}

// A name the server does not know comes back as a JSON-RPC error, so the webui
// hears the server's own words.
func TestUnknownToolReportsProtocolError(t *testing.T) {
	c, _ := newClient(t, fixtureConfig("fx"))
	res := c.entries["fx"].call(context.Background(), "no_such_tool", nil)
	if res.Error == "" {
		t.Fatalf("got %+v, want an error", res)
	}
	if res.PlainText != "" {
		t.Errorf("PlainText = %q, want empty", res.PlainText)
	}
	t.Logf("unknown tool error = %q", res.Error)
}

// Arguments travel as the params object, unchanged.
func TestArgumentsReachTheServer(t *testing.T) {
	c, _ := newClient(t, fixtureConfig("fx"))
	if res := call(t, c, "fx_echo", map[string]any{"text": "a", "extra": 1}); res.PlainText != "a" {
		t.Errorf("echo = %+v", res)
	}
	// No params at all still works: the server gets an empty object.
	if res := call(t, c, "fx_pid", nil); res.Error != "" {
		t.Errorf("pid = %+v", res)
	}
}

// A child that dies during a call gives the transport closed text, and the next
// call starts a fresh child.
func TestChildDiesDuringCall(t *testing.T) {
	if !canSignal {
		t.Skip("killing a pid needs a unix process table")
	}
	c, rec := newClient(t, fixtureConfig("fx"))
	e := c.entries["fx"]
	// Open the session first, so the pid below is the live child.
	if res := call(t, c, "fx_echo", map[string]any{"text": "warm"}); res.Error != "" {
		t.Fatal(res.Error)
	}
	pid := e.child.pid()
	if !alivePID(pid) {
		t.Fatalf("child %d is not running", pid)
	}

	slow := make(chan contracts.Result, 1)
	go func() { slow <- call(t, c, "fx_slow", map[string]any{"ms": 5000}) }()
	time.Sleep(300 * time.Millisecond) // let the call reach the child
	if err := signalPID(pid, killSignal); err != nil {
		t.Fatal(err)
	}
	select {
	case res := <-slow:
		if res.Error == "" {
			t.Errorf("interrupted call = %+v, want an error", res)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("the killed call never returned")
	}

	if res := call(t, c, "fx_echo", map[string]any{"text": "again"}); res.PlainText != "again" {
		t.Errorf("call after the kill = %+v", res)
	}
	if !rec.hasAll("fx", "is no longer alive") {
		t.Error("missing the respawn line")
		rec.dump(t)
	}
	if newPID := e.child.pid(); newPID == pid {
		t.Errorf("the respawn reused pid %d", pid)
	}
	mustBeDead(t, pid)
}

// An unresponsive server must not stall startup for longer than the warmup cap,
// and must not leave its child running.
func TestWarmupCap(t *testing.T) {
	if !canSignal {
		t.Skip("checking the child needs a unix process table")
	}
	orig := warmupLimit
	warmupLimit = 400 * time.Millisecond
	t.Cleanup(func() { warmupLimit = orig })

	cfg := fixtureConfig("late", "-sleep-start", "60s")
	c, rec := newClient(t, cfg)
	if n := len(c.List()); n != 0 {
		t.Errorf("discovered %d tools behind a child that never answered", n)
	}
	if !rec.has("MCP warmup: failed to spawn 'late'") {
		t.Error("missing the warmup failure line")
		rec.dump(t)
	}
	e := c.entries["late"]
	pid := e.child.pid()
	if pid == 0 {
		t.Fatal("no child was recorded")
	}
	if !waitUntil(func() bool { return procState(pid) == 0 }) {
		t.Errorf("child %d survived the failed warmup, state %q", pid, string(procState(pid)))
	}
}

// The fixture binary comes from this test run, not from a stale path.
func TestFixtureBuilt(t *testing.T) {
	if _, err := os.Stat(fixtureBin); err != nil {
		t.Fatalf("fixture %s: %v", fixtureBin, err)
	}
	if !strings.HasSuffix(fixtureBin, "mcp-testserver") {
		t.Errorf("fixtureBin = %s", fixtureBin)
	}
	if _, err := filepath.Abs(fixtureBin); err != nil {
		t.Fatal(err)
	}
}

func mustGet(t *testing.T, c *Client, name string) contracts.Tool {
	t.Helper()
	tool, ok := c.Get(name)
	if !ok {
		t.Fatalf("no tool %q", name)
	}
	return tool
}

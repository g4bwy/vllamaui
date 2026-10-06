package mcpx

import (
	"context"
	"sync"
	"testing"
	"time"

	"llama-webui/server/internal/contracts"
)

// The shipped timings come from the C++ constants. TestMain shortens them, so
// the originals are stashed there.
func TestCppTimingConstants(t *testing.T) {
	if defaultCooldown != 5*time.Second {
		t.Errorf("cooldown = %v, want 5s like MCP_COOLDOWN_SECONDS", defaultCooldown)
	}
	if defaultWarmupLimit != 10*time.Second {
		t.Errorf("warmupLimit = %v, want 10s like MCP_WARMUP_TIMEOUT_SECONDS", defaultWarmupLimit)
	}
	if stderrTailMax != 4096 {
		t.Errorf("stderr tail = %d bytes, want 4096 like ERR_TAIL_MAX", stderrTailMax)
	}
}

func TestCallTimeout(t *testing.T) {
	c, _ := newClient(t, fixtureConfig("fx"))
	e := c.entries["fx"]
	// Spawn first, so the deadline only has to cover the tool, not the fork.
	if res := call(t, c, "fx_echo", map[string]any{"text": "warm"}); res.Error != "" {
		t.Fatal(res.Error)
	}
	e.mu.Lock()
	e.cfg.TimeoutMS = 200
	e.mu.Unlock()

	start := time.Now()
	res := call(t, c, "fx_slow", map[string]any{"ms": 4000})
	if res.Error != "request timed out" {
		t.Errorf("got %+v, want %q", res, "request timed out")
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Errorf("the call took %v, timeout_ms=200 should cut it short", took)
	}
	if res.PlainText != "" {
		t.Errorf("PlainText = %q, want empty", res.PlainText)
	}

	// An empty budget must be caught before the fork. The command is bogus on
	// purpose: a spawn attempt would log "failed to start" and park the server.
	cfg := Config{Servers: []ServerConfig{{
		Name: "zero", Command: "/nonexistent/mcp-server-here", TimeoutMS: 0,
	}}}
	c2, rec2 := newClient(t, cfg)
	if res := c2.entries["zero"].call(context.Background(), "echo", nil); res.Error != "request timed out" {
		t.Errorf("timeout_ms 0 gave %+v, want a timeout", res)
	}
	if n := rec2.count("failed to start"); n != 0 {
		t.Errorf("a call with no budget tried to spawn %d times", n)
	}
}

// A caller that walks away gets the cancelled text, like should_stop in C++.
func TestCallerCancel(t *testing.T) {
	c, _ := newClient(t, fixtureConfig("fx"))
	tool, ok := c.Get("fx_echo")
	if !ok {
		t.Fatal("no echo tool")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	res := tool.Invoke(ctx, contracts.ToolRequest{Name: "fx_echo"}, nil)
	if res.Error != "cancelled" {
		t.Errorf("got %+v, want cancelled", res)
	}

	// Cancelling mid call also lands on cancelled, not on a transport error.
	slow, ok := c.Get("fx_slow")
	if !ok {
		t.Fatal("no fx_slow")
	}
	ctx2, cancel2 := context.WithCancel(context.Background())
	go func() {
		time.Sleep(150 * time.Millisecond)
		cancel2()
	}()
	res = slow.Invoke(ctx2, contracts.ToolRequest{Name: "fx_slow", Params: map[string]any{"ms": 5000}}, nil)
	if res.Error != "cancelled" {
		t.Errorf("cancelled mid call = %+v, want cancelled", res)
	}
}

// A server that will not start is parked for the cooldown. During the cooldown
// every call says unavailable without touching the fork bomb.
func TestCooldownAfterFailedSpawn(t *testing.T) {
	cfg := Config{Servers: []ServerConfig{{
		Name:      "broken",
		Command:   "/nonexistent/mcp-server-here",
		TimeoutMS: 2000,
	}}}
	c, rec := newClient(t, cfg)
	if n := len(c.List()); n != 0 {
		t.Fatalf("a broken server offered %d tools", n)
	}
	if !rec.has("MCP warmup: failed to spawn 'broken'") {
		t.Error("missing the warmup spawn failure")
	}
	// A broken server has no tool to call by name, so go through the entry.
	e := c.entries["broken"]
	first := e.call(context.Background(), "echo", nil)
	if first.Error != "MCP server unavailable: broken" {
		t.Errorf("first call = %+v", first)
	}
	attempts := rec.count("failed to start")
	if attempts != 1 {
		t.Errorf("got %d start attempts, want 1", attempts)
	}

	// Still inside the cooldown: the answer is the same, and no new attempt.
	second := e.call(context.Background(), "echo", nil)
	if second.Error != "MCP server unavailable: broken" {
		t.Errorf("cooldown call = %+v", second)
	}
	if n := rec.count("failed to start"); n != attempts {
		t.Errorf("the cooldown did not hold: %d start lines, was %d", n, attempts)
	}
	if !rec.hasAll("broken", "in cooldown") && !rec.has("failed to start") {
		t.Error("nothing says why the server is parked")
	}

	time.Sleep(cooldown + 50*time.Millisecond)
	third := e.call(context.Background(), "echo", nil)
	if third.Error != "MCP server unavailable: broken" {
		t.Errorf("call after the cooldown = %+v", third)
	}
	if n := rec.count("failed to start"); n != attempts+1 {
		t.Errorf("after the cooldown there were %d start lines, want %d", n, attempts+1)
	}
}

// A cached transport that died is dropped, and the next call starts a new one.
func TestRespawnAfterChildExits(t *testing.T) {
	if !canSignal {
		t.Skip("checking the pid needs a unix process table")
	}
	// The fixture ends itself on the third call it answers.
	cfg := fixtureConfig("fx", "-exit-after", "2")
	c, _ := newClient(t, cfg)
	e := c.entries["fx"]

	for _, want := range []string{"first", "second"} {
		if res := call(t, c, "fx_echo", map[string]any{"text": want}); res.PlainText != want {
			t.Fatalf("echo %s = %+v", want, res)
		}
	}
	pid := e.child.pid()
	t.Logf("the call that kills the child = %+v", call(t, c, "fx_echo", map[string]any{"text": "third"}))
	if !waitUntil(func() bool { return procState(pid) == 0 }) {
		t.Fatalf("child %d is still there", pid)
	}

	// The next call respawns, and the fresh process counts from one.
	if got := call(t, c, "fx_counter", nil).PlainText; got != "count=1" {
		t.Errorf("counter after respawn = %q, want count=1", got)
	}
	if e.child.pid() == pid {
		t.Error("the respawn reused the old pid")
	}
	if res := call(t, c, "fx_echo", map[string]any{"text": "back"}); res.PlainText != "back" {
		t.Errorf("echo after respawn = %+v", res)
	}
}

// Shutdown stops the work, kills the children, and is safe to call twice.
func TestShutdown(t *testing.T) {
	c, _ := newClient(t, fixtureConfig("fx"))
	e := c.entries["fx"]
	if res := call(t, c, "fx_echo", map[string]any{"text": "hi"}); res.Error != "" {
		t.Fatal(res.Error)
	}
	pid := e.child.pid()

	c.Shutdown()
	c.Shutdown() // second call must not panic or block

	if res := call(t, c, "fx_echo", map[string]any{"text": "hi"}); res.Error != "MCP server unavailable: fx" {
		t.Errorf("call after shutdown = %+v", res)
	}
	mustBeDead(t, pid)
	// List keeps working after the shutdown, so a late /tools request is safe.
	if len(c.List()) == 0 {
		t.Error("List went empty after Shutdown")
	}
}

// One server answers at a time, so two calls to it queue up.
func TestCallsToOneServerSerialize(t *testing.T) {
	cfg := fixtureConfig("fx")
	cfg.Servers[0].TimeoutMS = 8000
	c, _ := newClient(t, cfg)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); call(t, c, "fx_slow", map[string]any{"ms": 600}) }()
	go func() { defer wg.Done(); call(t, c, "fx_slow", map[string]any{"ms": 600}) }()
	start := time.Now()
	wg.Wait()
	if took := time.Since(start); took < 1100*time.Millisecond {
		t.Errorf("two 600ms calls finished in %v, they ran in parallel", took)
	}
}

// A slow server must not hold up a different one.
func TestServersDoNotBlockEachOther(t *testing.T) {
	cfg := Config{Servers: []ServerConfig{
		{Name: "slow", Command: fixtureBin, TimeoutMS: 8000},
		{Name: "fast", Command: fixtureBin, TimeoutMS: 8000},
	}}
	c, _ := newClient(t, cfg)

	done := make(chan struct{})
	go func() {
		defer close(done)
		call(t, c, "slow_slow", map[string]any{"ms": 1500})
	}()
	time.Sleep(200 * time.Millisecond)

	start := time.Now()
	if res := call(t, c, "fast_echo", map[string]any{"text": "now"}); res.PlainText != "now" {
		t.Fatalf("fast call = %+v", res)
	}
	if took := time.Since(start); took > time.Second {
		t.Errorf("the idle server waited %v behind the busy one", took)
	}
	<-done
}

// The child stderr is kept only as a bounded tail.
func TestStderrTailIsBounded(t *testing.T) {
	tr := &tail{max: 16}
	for range 10 {
		tr.Write([]byte("0123456789abcdef"))
	}
	if got := tr.String(); len(got) != 16 {
		t.Errorf("tail = %q (%d bytes), want 16", got, len(got))
	}
	if got := (&tail{max: 4}).String(); got != "" {
		t.Errorf("empty tail = %q", got)
	}
}

func TestEmptyConfigGivesAnIdleClient(t *testing.T) {
	c, rec := newClient(t, Config{})
	if n := len(c.List()); n != 0 {
		t.Errorf("%d tools from no servers", n)
	}
	if got := c.Registry().List(); len(got) != 0 {
		t.Errorf("Registry().List() = %v", got)
	}
	if _, ok := c.Get("anything"); ok {
		t.Error("Get found a tool that is not there")
	}
	c.Shutdown()
	if rec.count("MCP warmup") != 0 {
		t.Error("a client with no servers must not warm anything up")
	}
}

// Registry is the small interface the HTTP layer holds, and List and Get are
// the same view through it.
func TestClientSatisfiesRegistry(t *testing.T) {
	c, _ := newClient(t, fixtureConfig("fx"))
	var reg contracts.Registry = c.Registry()
	if len(reg.List()) != len(c.List()) || len(reg.List()) == 0 {
		t.Errorf("Registry().List() has %d tools, the client has %d", len(reg.List()), len(c.List()))
	}
	if _, ok := reg.Get("fx_echo"); !ok {
		t.Error("Registry().Get lost a tool")
	}
	if _, ok := reg.Get("nope"); ok {
		t.Error("Registry().Get invented a tool")
	}
}

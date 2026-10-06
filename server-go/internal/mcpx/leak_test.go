package mcpx

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Every path that can start a goroutine or a child is run here, so a leak shows
// up as a count that does not come back down.
func TestNoGoroutineOrChildLeaks(t *testing.T) {
	if !canSignal {
		t.Skip("counting children needs a unix process table")
	}
	// One warm-up pass so a lazy SDK goroutine started by the first client is
	// not counted as a leak below.
	c := newClientQuiet(t, fixtureConfig("warm"))
	c.Shutdown()

	goroutines := runtime.NumGoroutine()
	fds := openFDs(t)
	children := childPIDs(t)

	// 1. A server that never starts: cooldown, then a retry, then shutdown.
	broken := newClientQuiet(t, Config{Servers: []ServerConfig{
		{Name: "broken", Command: "/nonexistent/mcp-server-here", TimeoutMS: 1000},
	}})
	broken.entries["broken"].call(context.Background(), "echo", nil)
	broken.entries["broken"].call(context.Background(), "echo", nil)
	broken.Shutdown()

	// 2. A server that starts, answers, and gets a cancelled call, an expired
	// call, and a tool list refresh.
	fx := newClientQuiet(t, fixtureConfig("fx"))
	fx.entries["fx"].call(context.Background(), "echo", map[string]any{"text": "a"})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	fx.entries["fx"].call(ctx, "echo", nil)
	fx.entries["fx"].refresh()
	fx.entries["fx"].call(context.Background(), "hasimage", nil)
	fx.Shutdown()

	// 3. A child that dies on its own while a call is in flight.
	dying := newClientQuiet(t, fixtureConfig("dying", "-exit-after", "1"))
	dying.entries["dying"].call(context.Background(), "echo", map[string]any{"text": "a"})
	dying.entries["dying"].call(context.Background(), "echo", map[string]any{"text": "b"})
	dying.Shutdown()

	// 4. A server whose handshake times out.
	slow := warmupStalled(t)
	slow.Shutdown()

	// Let a teardown that is still running finish, then look once and poll.
	time.Sleep(250 * time.Millisecond)
	if !waitUntil(func() bool {
		return runtime.NumGoroutine() <= goroutines && openFDs(t) <= fds && len(childPIDs(t)) <= len(children)
	}) {
		t.Errorf("goroutines %d -> %d, fds %d -> %d, children %d -> %v",
			goroutines, runtime.NumGoroutine(), fds, openFDs(t), len(children), childPIDs(t))
		for _, p := range childPIDs(t) {
			data, _ := os.ReadFile(filepath.Join("/proc", p, "cmdline"))
			t.Errorf("leftover child %s: %s", p, strings.ReplaceAll(string(data), "\x00", " "))
		}
	}
}

// newClientQuiet is newClient without the cleanup double shutdown noise.
func newClientQuiet(t *testing.T, cfg Config) *Client {
	t.Helper()
	c, err := New(context.Background(), cfg, &Options{Logger: (&recorder{}).logger()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

// warmupStalled builds a client whose server never answers the handshake.
func warmupStalled(t *testing.T) *Client {
	t.Helper()
	orig := warmupLimit
	warmupLimit = 300 * time.Millisecond
	defer func() { warmupLimit = orig }()
	return newClientQuiet(t, fixtureConfig("late", "-sleep-start", "60s"))
}

// childPIDs lists the direct children of the test process.
func childPIDs(t *testing.T) []string {
	t.Helper()
	if !canSignal {
		return nil
	}
	entries, err := filepath.Glob("/proc/[0-9]*/stat")
	if err != nil {
		t.Fatal(err)
	}
	var pids []string
	self := os.Getpid()
	for _, e := range entries {
		data, err := os.ReadFile(e)
		if err != nil {
			continue
		}
		line := string(data)
		// The comm field can hold spaces and parentheses, so read after the
		// last ')': state, then ppid.
		i := strings.LastIndex(line, ")")
		fields := strings.Fields(line[i+1:])
		if i < 0 || len(fields) < 2 {
			continue
		}
		if ppid, err := strconv.Atoi(fields[1]); err == nil && ppid == self {
			pids = append(pids, filepath.Base(filepath.Dir(e)))
		}
	}
	return pids
}

func openFDs(t *testing.T) int {
	t.Helper()
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Skip("no /proc/self/fd")
	}
	return len(entries)
}

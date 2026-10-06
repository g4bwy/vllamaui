package mcpx

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"llama-webui/server/internal/contracts"
)

// fixtureBin is the compiled testserver. TestMain builds it from source, so the
// stdio tests run the real transport.
var fixtureBin string

// The shipped timings, before this test run shortens them.
var (
	defaultCooldown    time.Duration
	defaultWarmupLimit time.Duration
)

func TestMain(m *testing.M) {
	defaultCooldown, defaultWarmupLimit = cooldown, warmupLimit
	// Shorten the two C++ timings so the lifecycle tests stay fast.
	cooldown = 300 * time.Millisecond

	root, err := moduleRoot()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	dir, err := os.MkdirTemp("", "mcpx-fixture")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fixtureBin = filepath.Join(dir, "mcp-testserver")
	build := exec.Command("go", "build", "-o", fixtureBin, "./internal/mcpx/testserver")
	build.Dir = root
	if out, err := build.CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "build fixture: %v\n%s\n", err, out)
		os.Exit(1)
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

func moduleRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("no go.mod above %s", dir)
		}
		dir = parent
	}
}

// recorder keeps the log lines of one test as plain text, so an assertion can
// search for a message exactly as the code formatted it.
type recorder struct {
	mu    sync.Mutex
	lines []string
}

func (r *recorder) logger() *slog.Logger {
	return slog.New(&lineHandler{rec: r})
}

func (r *recorder) add(line string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lines = append(r.lines, line)
}

// has is true when one line holds substr.
func (r *recorder) has(substr string) bool { return r.count(substr) > 0 }

func (r *recorder) count(substr string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, l := range r.lines {
		if strings.Contains(l, substr) {
			n++
		}
	}
	return n
}

// hasAll is true when one line holds every substr.
func (r *recorder) hasAll(subs ...string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, l := range r.lines {
		ok := true
		for _, s := range subs {
			if !strings.Contains(l, s) {
				ok = false
				break
			}
		}
		if ok {
			return true
		}
	}
	return false
}

func (r *recorder) dump(t *testing.T) {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, l := range r.lines {
		t.Log(l)
	}
}

// lineHandler writes "LEVEL message key=value" with no quoting.
type lineHandler struct {
	rec   *recorder
	attrs []string
}

func (h *lineHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *lineHandler) Handle(_ context.Context, r slog.Record) error {
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s", r.Level, r.Message)
	b.WriteString(strings.Join(h.attrs, ""))
	r.Attrs(func(a slog.Attr) bool {
		fmt.Fprintf(&b, " %s=%v", a.Key, a.Value.Any())
		return true
	})
	h.rec.add(b.String())
	return nil
}

func (h *lineHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	out := &lineHandler{rec: h.rec, attrs: append([]string{}, h.attrs...)}
	for _, a := range attrs {
		out.attrs = append(out.attrs, fmt.Sprintf(" %s=%v", a.Key, a.Value.Any()))
	}
	return out
}

func (h *lineHandler) WithGroup(string) slog.Handler { return h }

// newClient builds a client over the fixture binary.
func newClient(t *testing.T, cfg Config) (*Client, *recorder) {
	t.Helper()
	rec := &recorder{}
	c, err := New(context.Background(), cfg, &Options{Logger: rec.logger()})
	if err != nil {
		t.Fatalf("New: %v\nlog:\n%s", err, strings.Join(rec.lines, "\n"))
	}
	t.Cleanup(c.Shutdown)
	return c, rec
}

// fixtureConfig returns a config for one server that runs the fixture binary.
func fixtureConfig(name string, args ...string) Config {
	return Config{Servers: []ServerConfig{{
		Name:      name,
		Command:   fixtureBin,
		Args:      args,
		TimeoutMS: 5000,
	}}}
}

func call(t *testing.T, c *Client, name string, params map[string]any) contracts.Result {
	t.Helper()
	tool, ok := c.Get(name)
	if !ok {
		t.Fatalf("no tool %q, have %v", name, toolNames(c))
	}
	return tool.Invoke(context.Background(), contracts.ToolRequest{Name: name, Params: params}, nil)
}

func toolNames(c *Client) []string {
	var out []string
	for _, info := range c.List() {
		out = append(out, info.Tool)
	}
	return out
}

// waitUntil polls cond for up to 5 seconds.
func waitUntil(cond func() bool) bool {
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
		if cond() {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return cond()
}

func mustHaveTool(t *testing.T, c *Client, name string) {
	t.Helper()
	if _, ok := c.Get(name); !ok {
		t.Fatalf("tool %q is missing, have %v", name, toolNames(c))
	}
}

// mustBeDead waits until the pid is gone. A reaped child leaves no process
// table entry, so this is the check that nothing outlived the client.
func mustBeDead(t *testing.T, pid int) {
	t.Helper()
	if !waitUntil(func() bool { return !alivePID(pid) }) {
		t.Errorf("child %d is still running after shutdown", pid)
	}
}

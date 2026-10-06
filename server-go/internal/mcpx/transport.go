package mcpx

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// stderrTailMax bounds the child stderr kept for diagnostics. ERR_TAIL_MAX in
// server-mcp.cpp.
const stderrTailMax = 4096

// childGrace is how long a close waits for a child to exit on its own before it
// signals it. The SDK default is 5 seconds, which is too slow for a shutdown.
const childGrace = 1 * time.Second

// child is the subprocess behind a stdio session, plus the tail of its stderr.
// A session with no child talks to an http endpoint.
type child struct {
	cmd  *exec.Cmd
	tail *tail
}

func (ch *child) pid() int {
	if ch == nil || ch.cmd == nil || ch.cmd.Process == nil {
		return 0
	}
	return ch.cmd.Process.Pid
}

// connect starts a server and runs the initialize handshake.
func (c *Client) connect(ctx context.Context, sc ServerConfig) (*mcp.ClientSession, *child, error) {
	var transport mcp.Transport
	var ch *child
	if sc.URL != "" {
		transport = &mcp.StreamableClientTransport{Endpoint: sc.URL}
	} else {
		cmd, tail, err := c.command(sc)
		if err != nil {
			return nil, nil, err
		}
		ch = &child{cmd: cmd, tail: tail}
		transport = &mcp.CommandTransport{Command: cmd, TerminateDuration: childGrace}
	}
	sess, err := c.sdk.Connect(ctx, transport, nil)
	if err != nil {
		// A handshake that failed after the fork must not leave a running child.
		// The SDK has usually reaped it already, so Kill may just report that.
		if ch != nil && ch.cmd.Process != nil {
			_ = ch.cmd.Process.Kill()
		}
		return nil, ch, err
	}
	return sess, ch, nil
}

// childEnvKeys are the parent variables a stdio child may start with. The child
// environment begins with these and nothing else, so the web server's secrets
// (UPSTREAM_API_KEY and friends) never reach a third-party package that the
// config asks uvx or npx to download and run. A server that needs the whole
// parent environment asks for it with "inherit_env": true.
var childEnvKeys = []string{
	"PATH", "HOME", "USER", "LOGNAME", "SHELL", "TERM",
	"LANG", "LC_ALL", "TZ", "TMPDIR",
}

// command is the stdio transport of server_mcp_stdio::start: the program with
// its args, a minimal environment with the config overrides on top, and cwd.
func (c *Client) command(sc ServerConfig) (*exec.Cmd, *tail, error) {
	if sc.Command == "" {
		return nil, nil, fmt.Errorf("server %q has no command", sc.Name)
	}
	cmd := exec.CommandContext(c.ctx, sc.Command, sc.Args...)
	cmd.Dir = sc.Cwd
	// Always an explicit environment: leaving cmd.Env nil would hand the child
	// the whole parent environment.
	cmd.Env = childEnv(sc)
	t := &tail{max: stderrTailMax}
	cmd.Stderr = t
	// The lifetime context is the child's leash: Shutdown cancels it, so no
	// child can outlive the process.
	cmd.Cancel = func() error { return cmd.Process.Kill() }
	cmd.WaitDelay = childGrace
	return cmd, t, nil
}

// childEnv is mcp_build_env: the environment a stdio child starts with. It is
// the config's env entries on top of the parent's allowlisted variables, or on
// top of the whole parent environment when the entry asks for inherit_env.
func childEnv(sc ServerConfig) []string {
	base := os.Environ()
	if !sc.InheritEnv {
		base = minimalParentEnv()
	}
	return overrideEnv(base, sc.Env)
}

// minimalParentEnv is the parent's childEnvKeys variables, as many as it has.
func minimalParentEnv() []string {
	var env []string
	for _, key := range childEnvKeys {
		if v, ok := os.LookupEnv(key); ok {
			env = append(env, key+"="+v)
		}
	}
	return env
}

// overrideEnv is base minus the keys overrides sets, plus those keys.
func overrideEnv(env []string, overrides map[string]string) []string {
	out := make([]string, 0, len(env)+len(overrides))
	for _, kv := range env {
		key, _, _ := strings.Cut(kv, "=")
		if _, ok := overrides[key]; !ok {
			out = append(out, kv)
		}
	}
	keys := make([]string, 0, len(overrides))
	for k := range overrides {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		out = append(out, k+"="+overrides[k])
	}
	return out
}

// tail keeps the last max bytes written to it. It stands in for errlog_loop,
// which drains the child stderr so the child never blocks on a full pipe.
type tail struct {
	mu  sync.Mutex
	buf []byte
	max int
}

func (t *tail) Write(p []byte) (int, error) {
	if t == nil {
		return len(p), nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if over := len(t.buf) - t.max; over > 0 {
		t.buf = t.buf[over:]
	}
	return len(p), nil
}

func (t *tail) String() string {
	if t == nil {
		return ""
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.buf)
}

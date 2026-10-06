package builtin

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"llama-webui/server/internal/contracts"
)

func execShellCommand(ctx context.Context, s *Set, req contracts.ToolRequest, out contracts.Sink) contracts.Result {
	params := req.Params
	command, bad := reqString(params, "command")
	if bad != "" {
		return fail(bad)
	}

	// both numbers are clamped down only, so a caller cannot raise a limit
	timeout := optInt(params, "timeout", execDefaultTimeout)
	if timeout > execMaxTimeout {
		timeout = execMaxTimeout
	}
	maxOutput := optInt(params, "max_output_size", execMaxOutputSize)
	if maxOutput < 0 {
		maxOutput = execMaxOutputSize // the C++ casts it to size_t, which then clamps to the max
	}
	if maxOutput > execMaxOutputSize {
		maxOutput = execMaxOutputSize
	}

	push := func(string) {}
	if req.Stream {
		push = out
	}

	res := runProc(ctx, s.dir(req), []string{"sh", "-c", command}, maxOutput, timeout, push)

	tail := fmt.Sprintf("\n[exit code: %d]", res.exitCode)
	if res.timedOut {
		// A killed child has no exit code worth quoting, and reporting one next
		// to the timeout contradicts itself: the timeout is the whole outcome.
		tail = "\n[exit due to timed out]"
	}

	if req.Stream {
		if res.stopped {
			// the client went away, so the closing frame is not sent
			return contracts.Result{}
		}
		out(tail)
		return contracts.Result{}
	}
	return contracts.Result{PlainText: res.output + tail}
}

type execOutcome struct {
	output   string
	exitCode int
	timedOut bool
	stopped  bool // the caller's context was cancelled
}

// execKillGrace is how long a cancelled child gets to close the output pipe on
// its own before this side shuts the read end.
const execKillGrace = time.Second

// execEnv is the allowlist a child process is started with. The server holds
// upstream API keys and cloud credentials in its own environment, and none of
// them may reach a command the model asked for.
var execEnv = []string{
	"PATH", "HOME", "USER", "LOGNAME", "SHELL", "TERM", "LANG", "LC_ALL", "TZ", "TMPDIR",
}

// childEnv copies the allowlist out of the parent environment. A name the
// parent does not carry is left out rather than invented.
func childEnv() []string {
	env := make([]string, 0, len(execEnv))
	for _, name := range execEnv {
		if v, ok := os.LookupEnv(name); ok {
			env = append(env, name+"="+v)
		}
	}
	return env
}

// runProc runs args in dir with stdout and stderr merged into one stream.
// Output past maxOutput bytes is dropped but still drained, so the child never
// blocks on a full pipe. push, when set, gets each chunk as it arrives.
//
// The timeout arms after the child is running, so a zero or negative value
// stops it at once instead of refusing to start it. The whole process group is
// killed, on timeout and on context cancellation alike, and the call returns
// about a second after the kill even when something escapes the group.
//
// The child sees execEnv only, never the full server environment.
func runProc(ctx context.Context, dir string, args []string, maxOutput, timeoutSecs int, push func(string)) execOutcome {
	cctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var timedOut atomic.Bool

	cmd := exec.CommandContext(cctx, args[0], args[1:]...)
	cmd.Dir = dir
	cmd.Env = childEnv()
	// a new process group, so stopping the shell stops its children too
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}

	r, w, err := os.Pipe()
	if err != nil {
		return execOutcome{output: "failed to spawn process", exitCode: -1}
	}
	cmd.Stdout = w
	cmd.Stderr = w

	if err := cmd.Start(); err != nil {
		r.Close()
		w.Close()
		return execOutcome{output: "failed to spawn process", exitCode: -1, stopped: ctx.Err() != nil}
	}
	// from now on the child side alone holds a write end, so the read side sees
	// EOF once the child and everything it spawned are gone
	w.Close()

	alarm := time.AfterFunc(time.Duration(timeoutSecs)*time.Second, func() {
		timedOut.Store(true)
		cancel()
	})
	defer alarm.Stop()

	// The kill and a cancelled caller both land on cctx. SIGKILL reaches the
	// process group, but a grandchild that detached itself with setsid keeps an
	// inherited copy of the write end open, so drain would wait for an EOF that
	// only comes when that stray exits. After the grace the read end is closed
	// here, which is what ends a blocked Read with an error.
	stopped := make(chan struct{})
	defer close(stopped)
	go func() {
		select {
		case <-cctx.Done():
			select {
			case <-time.After(execKillGrace):
				// Closing an *os.File while a Read is blocked on it is safe in
				// Go, and here it is the only way back.
				r.Close()
			case <-stopped:
			}
		case <-stopped:
		}
	}()

	res := drain(r, maxOutput, push)
	r.Close()

	// Wait must always run: it is what reaps the child.
	if err := cmd.Wait(); err != nil && cmd.ProcessState == nil {
		res.output = "failed to spawn process"
		res.exitCode = -1
	} else if cmd.ProcessState != nil && cmd.ProcessState.Exited() {
		res.exitCode = cmd.ProcessState.ExitCode()
	} else {
		// stopped by a signal: subprocess_join reports EXIT_FAILURE here
		res.exitCode = 1
	}

	res.timedOut = timedOut.Load()
	res.stopped = ctx.Err() != nil
	return res
}

func drain(r *os.File, maxOutput int, push func(string)) execOutcome {
	var res execOutcome
	var sb strings.Builder
	buf := make([]byte, 4096)
	truncated := false

	for {
		n, err := r.Read(buf)
		if n > 0 && !truncated {
			if sb.Len()+n <= maxOutput {
				sb.Write(buf[:n])
				pushBytes(push, buf[:n])
			} else {
				remaining := maxOutput - sb.Len()
				sb.Write(buf[:remaining])
				pushBytes(push, buf[:remaining])
				truncated = true
			}
		}
		if err != nil {
			break
		}
	}

	res.output = sb.String()
	if truncated {
		res.output += "\n" + truncatedMarker
	}
	return res
}

func pushBytes(push func(string), b []byte) {
	if push != nil && len(b) > 0 {
		push(string(b))
	}
}

package builtin

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"llama-webui/server/internal/contracts"
)

func TestExecShellCommand(t *testing.T) {
	root := t.TempDir()
	s := newSet(t, root)

	cases := []struct {
		name     string
		cwd      string
		params   map[string]any
		want     string
		contains string
	}{
		{
			name:   "stdout and exit code",
			params: map[string]any{"command": "printf 'hi\\n'"},
			want:   "hi\n\n[exit code: 0]",
		},
		{
			name:     "stderr is combined",
			params:   map[string]any{"command": "printf 'out\\n'; printf 'err\\n' >&2"},
			contains: "out\nerr",
		},
		{
			name:   "nonzero exit code",
			params: map[string]any{"command": "exit 3"},
			want:   "\n[exit code: 3]",
		},
		{
			name:   "empty output still reports the code",
			params: map[string]any{"command": "true"},
			want:   "\n[exit code: 0]",
		},
		{
			name:   "runs in the request directory",
			cwd:    root,
			params: map[string]any{"command": "pwd"},
			want:   root + "\n\n[exit code: 0]",
		},
		{
			name:   "a relative directory is resolved first",
			cwd:    root + "/./",
			params: map[string]any{"command": "printf '%s\\n' \"$PWD\""},
			want:   root + "\n\n[exit code: 0]",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cwd := tc.cwd
			if cwd == "" {
				cwd = root
			}
			res := call(t, s, cwd, "exec_shell_command", tc.params)
			if res.Error != "" {
				t.Fatalf("unexpected error: %s", res.Error)
			}
			if tc.want != "" && res.PlainText != tc.want {
				t.Errorf("output = %q, want %q", res.PlainText, tc.want)
			}
			if tc.contains != "" && !strings.Contains(res.PlainText, tc.contains) {
				t.Errorf("output %q does not contain %q", res.PlainText, tc.contains)
			}
		})
	}
}

func TestExecShellCommandOutputCap(t *testing.T) {
	root := t.TempDir()
	s := newSet(t, root)

	cases := []struct {
		name   string
		params map[string]any
		want   int // bytes of output kept before the truncation marker
	}{
		{"default cap", map[string]any{"command": "head -c 40000 /dev/zero | tr '\\0' 'x'"}, execMaxOutputSize},
		{"a request above the max is clamped down", map[string]any{
			"command": "head -c 40000 /dev/zero | tr '\\0' 'x'", "max_output_size": float64(1e9)},
			execMaxOutputSize},
		{"a request below the max is honoured", map[string]any{
			"command": "printf 'abcdefghijkl'", "max_output_size": float64(10)},
			10},
		{"a negative request is clamped to the max", map[string]any{
			"command": "head -c 40000 /dev/zero | tr '\\0' 'x'", "max_output_size": float64(-1)},
			execMaxOutputSize},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := call(t, s, root, "exec_shell_command", tc.params)
			wantTail := "\n" + truncatedMarker + "\n[exit code: 0]"
			if !strings.HasSuffix(res.PlainText, wantTail) {
				t.Fatalf("output tail = %q, want %q", tail(res.PlainText), wantTail)
			}
			body := res.PlainText[:len(res.PlainText)-len(wantTail)]
			if len(body) != tc.want {
				t.Errorf("kept %d bytes, want %d", len(body), tc.want)
			}
		})
	}
}

func TestExecShellCommandTimeout(t *testing.T) {
	root := t.TempDir()
	// the marker sits in a script name, so it is unique to this test, and the
	// script is the grandchild: stopping it proves the whole group died
	script := filepath.Join(root, "timeoutmarker.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nsleep 30\n"), 0o777); err != nil {
		t.Fatal(err)
	}
	s := newSet(t, root)

	start := time.Now()
	res := call(t, s, root, "exec_shell_command", map[string]any{"command": "./timeoutmarker.sh", "timeout": float64(1)})
	elapsed := time.Since(start)

	if elapsed > 5*time.Second {
		t.Errorf("the command ran %v, the timeout did not stop it", elapsed)
	}
	want := "\n[exit code: 1] [exit due to timed out]"
	if res.PlainText != want {
		t.Errorf("output = %q, want %q", res.PlainText, want)
	}
	assertNoProcess(t, "timeoutmarker.sh")
}

// TestExecShellCommandZeroTimeout: llama-server arms its watchdog with the
// value as given, so 0 stops the child at once rather than meaning "no limit".
func TestExecShellCommandZeroTimeout(t *testing.T) {
	root := t.TempDir()
	s := newSet(t, root)

	start := time.Now()
	res := call(t, s, root, "exec_shell_command", map[string]any{"command": "sleep 25", "timeout": float64(0)})
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("the command ran %v", elapsed)
	}
	if want := "\n[exit code: 1] [exit due to timed out]"; res.PlainText != want {
		t.Errorf("output = %q, want %q", res.PlainText, want)
	}
	assertNoProcess(t, "sleep 25")
}

// TestExecShellCommandNegativeTimeout is the same clamp in the other
// direction: a negative timeout is kept, never raised to the max.
func TestExecShellCommandNegativeTimeout(t *testing.T) {
	root := t.TempDir()
	s := newSet(t, root)

	res := call(t, s, root, "exec_shell_command", map[string]any{"command": "sleep 26", "timeout": float64(-5)})
	if !strings.HasSuffix(res.PlainText, " [exit due to timed out]") {
		t.Errorf("output = %q, want the timed out tail", res.PlainText)
	}
	assertNoProcess(t, "sleep 26")
}

// TestExecShellCommandCancelStopsTheGroup: a cancelled caller kills the whole
// process group. The text still comes back with its exit line, because
// llama-server has no early stop for a non-streaming call.
func TestExecShellCommandCancelStopsTheGroup(t *testing.T) {
	root := t.TempDir()
	script := filepath.Join(root, "cancelmarker.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nsleep 40\n"), 0o777); err != nil {
		t.Fatal(err)
	}
	s := newSet(t, root)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tool, _ := mustTool(t, s, "exec_shell_command")
	go func() {
		time.Sleep(200 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	res := tool.Invoke(ctx, contracts.ToolRequest{
		Name:   "exec_shell_command",
		Params: map[string]any{"command": "./cancelmarker.sh", "timeout": float64(50)},
		Cwd:    root,
	}, nil)

	if res.Error != "" {
		t.Errorf("unexpected error: %s", res.Error)
	}
	if want := "\n[exit code: 1]"; res.PlainText != want {
		t.Errorf("output = %q, want %q", res.PlainText, want)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("Invoke took %v, cancellation did not stop the child", elapsed)
	}
	assertNoProcess(t, "cancelmarker.sh")
}

// TestExecShellCommandStreamCancel: the closing frame is not pushed once the
// caller is gone.
func TestExecShellCommandStreamCancel(t *testing.T) {
	root := t.TempDir()
	s := newSet(t, root)

	var frames []string
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(200 * time.Millisecond)
		cancel()
	}()
	res := invokeCtx(t, s, ctx, contracts.ToolRequest{
		Name:   "exec_shell_command",
		Params: map[string]any{"command": "printf 'a\\n'; sleep 21", "timeout": float64(50)},
		Cwd:    root,
		Stream: true,
	}, func(text string) { frames = append(frames, text) })
	cancel()

	if res.Error != "" || res.PlainText != "" {
		t.Errorf("expected an empty result, got %+v", res)
	}
	for _, f := range frames {
		if strings.Contains(f, "exit code") {
			t.Errorf("the exit frame must be skipped after a cancel, got %q", f)
		}
	}
}

// TestExecShellCommandTimeoutClamped: timeout is only ever lowered. A request
// far above the max must still behave like a normal short command.
func TestExecShellCommandTimeoutClamped(t *testing.T) {
	root := t.TempDir()
	s := newSet(t, root)

	res := call(t, s, root, "exec_shell_command", map[string]any{"command": "printf 'ok\\n'", "timeout": float64(1e6)})
	if res.PlainText != "ok\n\n[exit code: 0]" {
		t.Errorf("output = %q", res.PlainText)
	}
}

func TestExecShellCommandStreaming(t *testing.T) {
	root := t.TempDir()
	s := newSet(t, root)

	var frames []string
	sink := func(text string) { frames = append(frames, text) }

	res := invoke(t, s, contracts.ToolRequest{
		Name:   "exec_shell_command",
		Params: map[string]any{"command": "printf 'one\\ntwo\\n'"},
		Cwd:    root,
		Stream: true,
	}, sink)

	if res.Error != "" {
		t.Fatalf("unexpected error: %s", res.Error)
	}
	if res.PlainText != "" || res.Body != nil {
		t.Errorf("a streaming call must return an empty result, got %+v", res)
	}
	if len(frames) < 2 {
		t.Fatalf("expected output frames plus the exit frame, got %v", frames)
	}
	if last := frames[len(frames)-1]; last != "\n[exit code: 0]" {
		t.Errorf("last frame = %q, want the exit code line", last)
	}
	if joined := strings.Join(frames[:len(frames)-1], ""); joined != "one\ntwo\n" {
		t.Errorf("output frames joined = %q", joined)
	}
}

func TestExecShellCommandCancelledKillsTheProcessGroup(t *testing.T) {
	root := t.TempDir()
	// the marker sits in a grandchild, so finding it after the call means the
	// shell was stopped but its children were not
	script := filepath.Join(root, "groupmarker.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nsleep 30\n"), 0o777); err != nil {
		t.Fatal(err)
	}
	s := newSet(t, root)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan contracts.Result, 1)
	go func() {
		tool, ok := s.Get("exec_shell_command")
		if !ok {
			t.Error("exec_shell_command not registered")
			done <- contracts.Result{}
			return
		}
		done <- tool.Invoke(ctx, contracts.ToolRequest{
			Name:   "exec_shell_command",
			Params: map[string]any{"command": root + "/groupmarker.sh"},
			Cwd:    root,
		}, func(string) {})
	}()

	time.Sleep(300 * time.Millisecond)
	cancel()

	select {
	case res := <-done:
		if res.Error != "" {
			t.Errorf("unexpected error: %s", res.Error)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Invoke did not return after the context was cancelled")
	}

	assertNoProcess(t, "groupmarker.sh")
}

// assertNoProcess waits for any process whose command line holds marker to
// disappear, then fails if it is still there.
func assertNoProcess(t *testing.T, marker string) {
	t.Helper()
	if _, err := os.Stat("/proc"); err != nil {
		t.Skip("/proc is not available")
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		found := runningWith(marker)
		if len(found) == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Errorf("processes still alive: %v", found)
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func runningWith(marker string) []string {
	var found []string
	dirs, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	for _, d := range dirs {
		if !d.IsDir() || strings.HasPrefix(d.Name(), ".") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join("/proc", d.Name(), "cmdline"))
		if err != nil {
			continue
		}
		cmdline := strings.ReplaceAll(string(raw), "\x00", " ")
		if strings.Contains(cmdline, marker) {
			found = append(found, fmt.Sprintf("%s: %s", d.Name(), strings.TrimSpace(cmdline)))
		}
	}
	return found
}

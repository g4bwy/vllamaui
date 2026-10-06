package builtin

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"llama-webui/server/internal/contracts"
)

func TestReadFileText(t *testing.T) {
	cwd := t.TempDir()
	writeTestFile(t, cwd, "a.txt", "one\ntwo\nthree")
	s := newSet(t, cwd)

	cases := []struct {
		name   string
		params map[string]any
		want   string
	}{
		{"whole file", map[string]any{"path": "a.txt"}, "one\ntwo\nthree\n"},
		{"range", map[string]any{"path": "a.txt", "start_line": float64(2), "end_line": float64(3)}, "two\nthree\n"},
		{"start only", map[string]any{"path": "a.txt", "start_line": float64(3)}, "three\n"},
		{"end only", map[string]any{"path": "a.txt", "end_line": float64(1)}, "one\n"},
		{"past the end", map[string]any{"path": "a.txt", "start_line": float64(9)}, ""},
		{"end past the end", map[string]any{"path": "a.txt", "end_line": float64(99)}, "one\ntwo\nthree\n"},
		{"append loc", map[string]any{"path": "a.txt", "append_loc": true}, "1\u2192one\n2\u2192two\n3\u2192three\n"},
		{"append loc with range", map[string]any{"path": "a.txt", "start_line": float64(2), "append_loc": true}, "2\u2192two\n3\u2192three\n"},
		{"float line numbers truncate", map[string]any{"path": "a.txt", "start_line": 2.9}, "two\nthree\n"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := call(t, s, cwd, "read_file", tc.params)
			if res.Error != "" {
				t.Fatalf("unexpected error: %s", res.Error)
			}
			if res.PlainText != tc.want {
				t.Errorf("plain text = %q, want %q", res.PlainText, tc.want)
			}
		})
	}
}

func TestReadFileEmptyFile(t *testing.T) {
	cwd := t.TempDir()
	writeTestFile(t, cwd, "empty.txt", "")
	s := newSet(t, cwd)

	res := call(t, s, cwd, "read_file", map[string]any{"path": "empty.txt"})
	if res.Error != "" || res.PlainText != "" {
		t.Errorf("got %+v, want an empty result with no error", res)
	}
}

func TestReadFileSizeCap(t *testing.T) {
	cwd := t.TempDir()
	// 200 lines of 100 chars, so the file is well past the 16 KB text cap
	var sb strings.Builder
	for i := 0; i < 200; i++ {
		fmt.Fprintf(&sb, "%03d %s\n", i, strings.Repeat("x", 96))
	}
	size := len(sb.String())
	writeTestFile(t, cwd, "big.txt", sb.String())
	s := newSet(t, cwd)

	t.Run("no range is refused", func(t *testing.T) {
		res := call(t, s, cwd, "read_file", map[string]any{"path": "big.txt"})
		want := fmt.Sprintf("file too large (%d bytes, max %d). Use start_line/end_line to read a portion.", size, readFileMaxSize)
		if res.Error != want {
			t.Errorf("error = %q, want %q", res.Error, want)
		}
	})

	t.Run("a range reads but stops at the cap", func(t *testing.T) {
		res := call(t, s, cwd, "read_file", map[string]any{"path": "big.txt", "start_line": 1, "end_line": 200})
		if res.Error != "" {
			t.Fatalf("unexpected error: %s", res.Error)
		}
		if !strings.HasSuffix(res.PlainText, truncatedMarker) {
			t.Errorf("output must end with the truncation marker, got %q", tail(res.PlainText))
		}
		kept := res.PlainText[:len(res.PlainText)-len(truncatedMarker)]
		if len(kept) > readFileMaxSize {
			t.Errorf("kept %d bytes, want at most %d", len(kept), readFileMaxSize)
		}
		lines := strings.Count(kept, "\n")
		if lines != 162 {
			t.Errorf("kept %d lines, want 162 (16384 / 101 bytes per line)", lines)
		}
	})

	t.Run("a small window inside a big file", func(t *testing.T) {
		res := call(t, s, cwd, "read_file", map[string]any{"path": "big.txt", "start_line": 5, "end_line": 7})
		want := "004 " + strings.Repeat("x", 96) + "\n005 " + strings.Repeat("x", 96) + "\n006 " + strings.Repeat("x", 96) + "\n"
		if res.PlainText != want {
			t.Errorf("unexpected output %q", tail(res.PlainText))
		}
	})
}

func TestReadFileBase64(t *testing.T) {
	cwd := t.TempDir()
	raw := "\x00\x01binary\xff payload\nsecond line\n"
	if err := os.WriteFile(filepath.Join(cwd, "bin.dat"), []byte(raw), 0o666); err != nil {
		t.Fatal(err)
	}
	s := newSet(t, cwd)

	res := invoke(t, s, contracts.ToolRequest{
		Name:     "read_file",
		Params:   map[string]any{"path": "bin.dat"},
		Cwd:      cwd,
		RespType: "base64",
	}, nil)
	if res.Error != "" {
		t.Fatalf("unexpected error: %s", res.Error)
	}
	got, _ := base64.StdEncoding.DecodeString(res.Body["base64"].(string))
	if string(got) != raw {
		t.Errorf("decoded payload mismatch")
	}
	if res.Body["size_bytes"] != len(raw) {
		t.Errorf("size_bytes = %v, want %d", res.Body["size_bytes"], len(raw))
	}
	if len(res.PlainText) != 0 {
		t.Errorf("plain_text_response must be empty in base64 mode, got %q", res.PlainText)
	}
}

func TestReadFileBase64TooLarge(t *testing.T) {
	cwd := t.TempDir()
	name := filepath.Join(cwd, "huge.bin")
	f, err := os.Create(name)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(readFileMaxSizeB64 + 1); err != nil {
		t.Fatal(err)
	}
	f.Close()
	s := newSet(t, cwd)

	res := invoke(t, s, contracts.ToolRequest{
		Name:     "read_file",
		Params:   map[string]any{"path": "huge.bin"},
		Cwd:      cwd,
		RespType: "base64",
	}, nil)
	want := fmt.Sprintf("file too large (%d bytes, max %d)", readFileMaxSizeB64+1, readFileMaxSizeB64)
	if res.Error != want {
		t.Errorf("error = %q, want %q", res.Error, want)
	}
}

func TestReadFileErrors(t *testing.T) {
	cwd := t.TempDir()
	if err := os.Mkdir(filepath.Join(cwd, "adir"), 0o777); err != nil {
		t.Fatal(err)
	}
	s := newSet(t, cwd)

	cases := []struct {
		name string
		path string
		want string
	}{
		{"missing", "nope.txt", "cannot stat file: nope.txt"},
		// A directory is not a regular file, so the size gates below never see
		// it: the mode check refuses it first.
		{"directory", "adir", "cannot read file: adir"},
		{"escape the cwd", "../outside.txt", "cannot stat file: ../outside.txt"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := call(t, s, cwd, "read_file", map[string]any{"path": tc.path})
			if res.Error != tc.want {
				t.Errorf("error = %q, want %q", res.Error, tc.want)
			}
		})
	}
}

// TestReadFileResolvesRelativePath checks that "." and ".." are folded away
// before the filesystem is touched, and that a path outside the cwd still
// works: llama-server confines nothing.
func TestReadFileResolvesRelativePath(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, root, "a.txt", "top\n")
	sub := filepath.Join(root, "sub")
	if err := os.Mkdir(sub, 0o777); err != nil {
		t.Fatal(err)
	}
	s := newSet(t, root)

	res := call(t, s, sub, "read_file", map[string]any{"path": "./../a.txt"})
	if res.Error != "" || res.PlainText != "top\n" {
		t.Errorf("got %+v, want the file one level up", res)
	}

	writeTestFile(t, root, "outside.txt", "outside\n")
	outside := filepath.Join(root, "outside.txt")
	res = call(t, s, sub, "read_file", map[string]any{"path": outside})
	if res.PlainText != "outside\n" {
		t.Errorf("absolute path outside the cwd: got %q", res.PlainText)
	}
}

// readWholeFile is the behaviour the streaming scanner replaced: cut the
// content with splitLines, clamp to the range, stop at the output cap.
func readWholeFile(content string, startLine, endLine int, appendLoc bool) string {
	var sb strings.Builder
	for i, line := range splitLines(content) {
		lineno := i + 1
		if lineno < startLine {
			continue
		}
		if endLine != -1 && lineno > endLine {
			break
		}
		out := line + "\n"
		if appendLoc {
			out = fmt.Sprintf("%d\u2192%s\n", lineno, line)
		}
		if sb.Len()+len(out) > readFileMaxSize {
			sb.WriteString(truncatedMarker)
			break
		}
		sb.WriteString(out)
	}
	return sb.String()
}

// TestReadFileMatchesWholeFileRead is the parity net for the streaming
// rewrite: every case must come out byte for byte as reading the file into
// memory and cutting it with splitLines would.
func TestReadFileMatchesWholeFileRead(t *testing.T) {
	longLine := strings.Repeat("y", 3*readFileChunk) // one line, past two buffer fills
	var big strings.Builder
	for i := 0; i < 2000; i++ {
		fmt.Fprintf(&big, "%04d %s\n", i, strings.Repeat("z", 95))
	}

	cases := []struct {
		name      string
		content   string
		startLine int
		endLine   int
		appendLoc bool
	}{
		{"no trailing newline", "one\ntwo", 1, -1, false},
		{"empty", "", 1, -1, false},
		{"only a newline", "\n", 1, -1, false},
		{"blank lines kept", "a\n\n\nb\n", 1, -1, false},
		{"crlf keeps the carriage return", "a\r\nb\r\n", 1, -1, false},
		{"unicode and the arrow glyph", "h\u00e9llo \u2192 w\u00f6rld\nsecond\r\n", 1, -1, true},
		{"range inside a short file", "a\nb\nc\nd\n", 2, 3, false},
		{"end before start", "a\nb\n", 3, 1, false},
		{"end line zero", "a\nb\n", 1, 0, false},
		{"start line zero", "a\nb\n", 0, -1, false},
		{"start past the end of a long line file", longLine, 2, 2, false},
		{"long line cannot fit", longLine, 1, 2, false},
		{"big file cut at the cap", big.String(), 1, 2000, false},
		{"big file cut at the cap with line numbers", big.String(), 1, 2000, true},
		{"window past the cap in a big file", big.String(), 1999, 2000, false},
		{"window past the cap with line numbers", big.String(), 1500, 1502, true},
		{"range past the end of a big file", big.String(), 3000, 6000, false},
		{"range past the end with line numbers", big.String(), 3000, 6000, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cwd := t.TempDir()
			writeTestFile(t, cwd, "f.txt", tc.content)
			s := newSet(t, cwd)

			res := call(t, s, cwd, "read_file", map[string]any{
				"path":       "f.txt",
				"start_line": float64(tc.startLine),
				"end_line":   float64(tc.endLine),
				"append_loc": tc.appendLoc,
			})
			want := readWholeFile(tc.content, tc.startLine, tc.endLine, tc.appendLoc)
			if res.Error != "" {
				t.Fatalf("unexpected error: %s", res.Error)
			}
			if res.PlainText != want {
				t.Errorf("got %d bytes starting %q, want %d bytes starting %q",
					len(res.PlainText), head(res.PlainText), len(want), head(want))
			}
		})
	}
}

func TestReadFileRefusesCharacterDevices(t *testing.T) {
	s := newSet(t, t.TempDir())

	// Each call used to reach os.ReadFile, because a device reports a size of 0
	// and so passes every limit. The /dev/zero ones ended in an out of memory
	// abort, which no recover() upstream can catch.
	cases := []struct {
		name     string
		path     string
		respType string
		params   map[string]any
	}{
		{"no range", "/dev/zero", "", map[string]any{"path": "/dev/zero"}},
		{"with a range", "/dev/zero", "", map[string]any{"path": "/dev/zero", "start_line": float64(1), "end_line": float64(10)}},
		{"base64", "/dev/urandom", "base64", map[string]any{"path": "/dev/urandom"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := os.Stat(tc.path); err != nil {
				t.Skipf("no %s on this machine", tc.path)
			}
			res := invoke(t, s, contracts.ToolRequest{
				Name:     "read_file",
				Params:   tc.params,
				RespType: tc.respType,
			}, nil)
			want := "cannot read file: " + tc.path
			if res.Error != want {
				t.Errorf("error = %q, want %q", res.Error, want)
			}
			if res.PlainText != "" || res.Body != nil {
				t.Errorf("got %+v, want nothing but the error", res)
			}
		})
	}
}

// TestReadFileRefusesFifoWithoutBlocking covers the second half of the device
// problem: a fifo has no size to argue about, and opening one for reading
// waits forever for a writer.
func TestReadFileRefusesFifoWithoutBlocking(t *testing.T) {
	cwd := t.TempDir()
	name := filepath.Join(cwd, "pipe")
	if err := syscall.Mkfifo(name, 0o666); err != nil {
		t.Skipf("cannot make a fifo here: %v", err)
	}
	s := newSet(t, cwd)

	res := callWithin(t, s, contracts.ToolRequest{
		Name:   "read_file",
		Params: map[string]any{"path": "pipe"},
		Cwd:    cwd,
	}, 5*time.Second)
	if res.Error != "cannot read file: pipe" {
		t.Errorf("error = %q, want %q", res.Error, "cannot read file: pipe")
	}
}

// TestReadFileRangeStaysInsideTheOutputCap reads the first line of a file far
// bigger than the 16 KiB limit. The old code loaded the whole thing first.
func TestReadFileRangeStaysInsideTheOutputCap(t *testing.T) {
	const sparseSize = 200 * 1024 * 1024

	t.Run("a range stops the read", func(t *testing.T) {
		cwd := t.TempDir()
		f, err := os.Create(filepath.Join(cwd, "sparse.txt"))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.WriteString("first\n"); err != nil {
			f.Close()
			t.Fatal(err)
		}
		if err := f.Truncate(sparseSize); err != nil {
			f.Close()
			t.Fatalf("cannot make a sparse file here: %v", err)
		}
		f.Close()
		s := newSet(t, cwd)

		var before, after runtime.MemStats
		runtime.GC()
		runtime.ReadMemStats(&before)
		start := time.Now()
		res := call(t, s, cwd, "read_file", map[string]any{"path": "sparse.txt", "end_line": float64(1)})
		elapsed := time.Since(start)
		runtime.ReadMemStats(&after)

		if res.Error != "" || res.PlainText != "first\n" {
			t.Fatalf("got %+v, want %q", res, "first\n")
		}
		if grew := after.TotalAlloc - before.TotalAlloc; grew > 16*1024*1024 {
			t.Errorf("the read allocated %d bytes for a 1 line request, want well under the %d byte file", grew, sparseSize)
		}
		if elapsed > 5*time.Second {
			t.Errorf("the read took %s, which means it walked the whole file", elapsed)
		}
	})

	t.Run("a hole wider than the cap is truncated", func(t *testing.T) {
		cwd := t.TempDir()
		f, err := os.Create(filepath.Join(cwd, "hole.txt"))
		if err != nil {
			t.Fatal(err)
		}
		if err := f.Truncate(sparseSize); err != nil {
			f.Close()
			t.Fatalf("cannot make a sparse file here: %v", err)
		}
		f.Close()
		s := newSet(t, cwd)

		res := call(t, s, cwd, "read_file", map[string]any{"path": "hole.txt", "end_line": float64(1)})
		if res.Error != "" {
			t.Fatalf("unexpected error: %s", res.Error)
		}
		// One line of NUL bytes can never fit, so the answer is only the marker,
		// exactly as the whole-file version wrote it.
		if res.PlainText != truncatedMarker {
			t.Errorf("plain text = %q, want the marker alone", head(res.PlainText))
		}
	})
}

// TestReadFileBase64BoundedRead covers the second limit on the base64 path.
// A regular file on a real filesystem reports its size, so os.Stat refuses it
// first; the bound on the read is what stands behind that gate.
func TestReadFileBase64BoundedRead(t *testing.T) {
	t.Run("the read stops one byte past the cap", func(t *testing.T) {
		content, over, err := readCapped(strings.NewReader(strings.Repeat("x", 4096)), 1024)
		if err != nil {
			t.Fatal(err)
		}
		if !over {
			t.Error("over = false, want true for a stream longer than the limit")
		}
		if len(content) != 1025 {
			t.Errorf("read %d bytes, want the limit plus the one that proves it", len(content))
		}

		content, over, err = readCapped(strings.NewReader("short"), 1024)
		if err != nil || over || string(content) != "short" {
			t.Errorf("got %q, over=%v, err=%v, want the whole string", content, over, err)
		}
	})

	t.Run("a file past the cap is refused either way", func(t *testing.T) {
		cwd := t.TempDir()
		// A procfs file claims 0 bytes and answers with whatever it likes, so
		// this is the shape of the lie the read limit is for. The tool only has
		// to refuse something too big, whichever gate found it.
		name := filepath.Join(cwd, "big.bin")
		f, err := os.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if err := f.Truncate(readFileMaxSizeB64 + 1); err != nil {
			f.Close()
			t.Fatalf("cannot make a sparse file here: %v", err)
		}
		f.Close()
		s := newSet(t, cwd)

		res := invoke(t, s, contracts.ToolRequest{
			Name:     "read_file",
			Params:   map[string]any{"path": "big.bin"},
			Cwd:      cwd,
			RespType: "base64",
		}, nil)
		want := fmt.Sprintf("file too large (%d bytes, max %d)", readFileMaxSizeB64+1, readFileMaxSizeB64)
		if res.Error != want {
			t.Errorf("error = %q, want %q", res.Error, want)
		}
	})
}

func TestReadFileHonoursContextCancellation(t *testing.T) {
	cwd := t.TempDir()
	content := strings.Repeat("a line of text\n", 2000)
	writeTestFile(t, cwd, "f.txt", content)
	s := newSet(t, cwd)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	res := invokeCtx(t, s, ctx, contracts.ToolRequest{
		Name:   "read_file",
		Params: map[string]any{"path": "f.txt", "end_line": float64(4000)},
		Cwd:    cwd,
	}, nil)
	if res.Error == "" {
		t.Errorf("got %q, want an error once the context is done", head(res.PlainText))
	}
}

func head(s string) string {
	if len(s) > 40 {
		return s[:40]
	}
	return s
}

// callWithin fails instead of hanging when a call does not come back.
func callWithin(t *testing.T, s *Set, req contracts.ToolRequest, within time.Duration) contracts.Result {
	t.Helper()
	tool, ok := s.Get(req.Name)
	if !ok {
		t.Fatalf("tool %q not registered", req.Name)
	}
	done := make(chan contracts.Result, 1)
	go func() { done <- tool.Invoke(context.Background(), req, nil) }()
	select {
	case res := <-done:
		return res
	case <-time.After(within):
		t.Fatalf("%q did not return within %s", req.Name, within)
		return contracts.Result{}
	}
}

func tail(s string) string {
	if len(s) > 40 {
		return s[len(s)-40:]
	}
	return s
}

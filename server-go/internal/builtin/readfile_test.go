package builtin

import (
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
		{"directory", "adir", "failed to open file: adir"},
		{"escape the cwd", "../outside.txt", "cannot stat file: ../outside.txt"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if st, err := os.Stat(filepath.Join(cwd, tc.path)); err == nil && st.IsDir() && st.Size() > readFileMaxSize {
				t.Skipf("directory size %d exceeds the text cap on this filesystem", st.Size())
			}
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

func tail(s string) string {
	if len(s) > 40 {
		return s[len(s)-40:]
	}
	return s
}

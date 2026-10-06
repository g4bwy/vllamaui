package builtin

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"llama-webui/server/internal/contracts"
)

// globTree builds a small directory layout under a fresh temp dir.
func globTree(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for _, name := range []string{
		"a.go", "b.txt", "src/inner.go", "pkg/x.go",
		"pkg/sub/keep.go", "pkg/sub/tmp/tmp.go",
		"node_modules/mod.js", ".git/config",
	} {
		writeTestFile(t, root, name, "x\n")
	}
	if err := os.Mkdir(filepath.Join(root, "emptydir"), 0o777); err != nil {
		t.Fatal(err)
	}
	return root
}

func entryPaths(res contracts.Result) []string {
	raw, _ := res.Body["entries"].([]any)
	out := make([]string, 0, len(raw))
	for _, e := range raw {
		m, _ := e.(map[string]any)
		out = append(out, fmt.Sprintf("%v:%v", m["type"], m["path"]))
	}
	return out
}

func TestFileGlobSearch(t *testing.T) {
	root := globTree(t)
	s := newSet(t, root)

	cases := []struct {
		name    string
		params  map[string]any
		entries []string
	}{
		{
			name:    "default include is every file",
			params:  map[string]any{"path": "."},
			entries: []string{"file:a.go", "file:b.txt", "file:pkg/sub/keep.go", "file:pkg/sub/tmp/tmp.go", "file:pkg/x.go", "file:src/inner.go"},
		},
		{
			name:    "no slash matches the basename at any depth",
			params:  map[string]any{"path": ".", "include": "*.go"},
			entries: []string{"file:a.go", "file:pkg/sub/keep.go", "file:pkg/sub/tmp/tmp.go", "file:pkg/x.go", "file:src/inner.go"},
		},
		{
			name:    "exclude drops matches",
			params:  map[string]any{"path": ".", "include": "*", "exclude": "*.txt"},
			entries: []string{"file:a.go", "file:pkg/sub/keep.go", "file:pkg/sub/tmp/tmp.go", "file:pkg/x.go", "file:src/inner.go"},
		},
		{
			name:    "a slashed pattern is anchored with **/",
			params:  map[string]any{"path": ".", "include": "sub/*"},
			entries: []string{"file:pkg/sub/keep.go"},
		},
		{
			name:    "dirs are listed but junk dirs are not walked",
			params:  map[string]any{"path": ".", "type": "dir"},
			entries: []string{"dir:.git", "dir:emptydir", "dir:node_modules", "dir:pkg", "dir:pkg/sub", "dir:pkg/sub/tmp", "dir:src"},
		},
		{
			name:    "all mixes both kinds",
			params:  map[string]any{"path": "pkg", "type": "all"},
			entries: []string{"dir:sub", "file:sub/keep.go", "dir:sub/tmp", "file:sub/tmp/tmp.go", "file:x.go"},
		},
		{
			name:    "a nested base lists from there",
			params:  map[string]any{"path": "src", "type": "all"},
			entries: []string{"file:inner.go"},
		},
		{
			name:    "max_depth 1 is the direct children",
			params:  map[string]any{"path": ".", "max_depth": float64(1)},
			entries: []string{"file:a.go", "file:b.txt"},
		},
		{
			name:    "max_depth 2 adds one more level",
			params:  map[string]any{"path": ".", "max_depth": 2.0},
			entries: []string{"file:a.go", "file:b.txt", "file:pkg/x.go", "file:src/inner.go"},
		},
		{
			name:    "a negative max_depth means unlimited",
			params:  map[string]any{"path": ".", "max_depth": float64(-3)},
			entries: []string{"file:a.go", "file:b.txt", "file:pkg/sub/keep.go", "file:pkg/sub/tmp/tmp.go", "file:pkg/x.go", "file:src/inner.go"},
		},
		{
			name:    "a bad type value falls back to the file default",
			params:  map[string]any{"path": ".", "type": 5.0},
			entries: []string{"file:a.go", "file:b.txt", "file:pkg/sub/keep.go", "file:pkg/sub/tmp/tmp.go", "file:pkg/x.go", "file:src/inner.go"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := call(t, s, root, "file_glob_search", tc.params)
			if res.Error != "" {
				t.Fatalf("unexpected error: %s", res.Error)
			}
			got := entryPaths(res)
			if strings.Join(got, " ") != strings.Join(tc.entries, " ") {
				t.Errorf("entries =\n%v\nwant\n%v", got, tc.entries)
			}
			if res.Body["base"] != resolvePath(root, tc.params["path"].(string)) {
				t.Errorf("base = %v, want %v", res.Body["base"], resolvePath(root, tc.params["path"].(string)))
			}
			text, _ := res.Body["plain_text_response"].(string)
			if text == "" {
				t.Error("plain_text_response must be in the body")
			}
			if res.PlainText != "" {
				t.Error("plain text must stay empty so the body is sent")
			}
		})
	}
}

func TestFileGlobSearchLimit(t *testing.T) {
	root := globTree(t)
	s := newSet(t, root)

	res := call(t, s, root, "file_glob_search", map[string]any{"path": ".", "include": "*.go", "limit": float64(2)})
	text, _ := res.Body["plain_text_response"].(string)
	wantText := "a.go\npkg/sub/keep.go\n" +
		"\n---\nTotal matches: 5\n" +
		"[2 results limit reached (5 total matches). Refine the glob pattern to narrow the search.]\n"
	if text != wantText {
		t.Errorf("plain text =\n%q\nwant\n%q", text, wantText)
	}
	if len(entryPaths(res)) != 2 {
		t.Errorf("entries = %v, want 2", entryPaths(res))
	}
}

func TestFileGlobSearchDirTrailingSlash(t *testing.T) {
	root := globTree(t)
	s := newSet(t, root)

	res := call(t, s, root, "file_glob_search", map[string]any{"path": ".", "type": "all", "include": "*", "max_depth": 1})
	text, _ := res.Body["plain_text_response"].(string)
	want := ".git/\na.go\nb.txt\nemptydir/\nnode_modules/\npkg/\nsrc/\n\n---\nTotal matches: 7\n"
	if text != want {
		t.Errorf("plain text =\n%q\nwant\n%q", text, want)
	}
	// entries keep the raw path, the slash is only in the text the model reads
	for _, e := range entryPaths(res) {
		if strings.HasSuffix(e, "/") {
			t.Errorf("entry %q must not carry a trailing slash", e)
		}
	}
}

func TestFileGlobSearchLimitCap(t *testing.T) {
	root := t.TempDir()
	for i := 0; i < 150; i++ {
		writeTestFile(t, root, fmt.Sprintf("f%03d.txt", i), "x\n")
	}
	s := newSet(t, root)

	res := call(t, s, root, "file_glob_search", map[string]any{"path": ".", "limit": 1000.0})
	if len(entryPaths(res)) != searchMaxResults {
		t.Errorf("got %d entries, want the cap of %d", len(entryPaths(res)), searchMaxResults)
	}
	text, _ := res.Body["plain_text_response"].(string)
	if !strings.Contains(text, "[100 results limit reached (150 total matches)") {
		t.Errorf("missing the limit tail in %q", tail(text))
	}
}

func TestFileGlobSearchErrors(t *testing.T) {
	root := globTree(t)
	s := newSet(t, root)

	cases := []struct {
		name   string
		params map[string]any
		want   string
	}{
		{"limit zero", map[string]any{"path": ".", "limit": float64(0)}, "invalid limit: 0 (expected 1 or more)"},
		{"limit negative", map[string]any{"path": ".", "limit": float64(-1)}, "invalid limit: -1 (expected 1 or more)"},
		{"bad type", map[string]any{"path": ".", "type": "both"}, `invalid type: both (expected "file", "dir" or "all")`},
		{"missing dir", map[string]any{"path": "nope"}, "path does not exist or is not a directory: nope"},
		{"a file is not a dir", map[string]any{"path": "a.go"}, "path does not exist or is not a directory: a.go"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := call(t, s, root, "file_glob_search", tc.params)
			if res.Error != tc.want {
				t.Errorf("error = %q, want %q", res.Error, tc.want)
			}
		})
	}
}

// TestFileGlobSearchBaseIsAbsolute covers what the webui needs from base: an
// absolute path, with "." and ".." folded out, and "~" expanded.
func TestFileGlobSearchBaseIsAbsolute(t *testing.T) {
	root := globTree(t)
	t.Setenv("HOME", root)
	s := newSet(t, "/nonexistent-default")

	res := call(t, s, filepath.Join(root, "pkg", "sub"), "file_glob_search", map[string]any{"path": "../.."})
	if res.Error != "" {
		t.Fatalf("unexpected error: %s", res.Error)
	}
	if res.Body["base"] != root {
		t.Errorf("base = %v, want %v", res.Body["base"], root)
	}

	res = call(t, s, root, "file_glob_search", map[string]any{"path": "~/pkg"})
	if res.Body["base"] != filepath.Join(root, "pkg") {
		t.Errorf("base = %v, want the expanded home path", res.Body["base"])
	}
}

func TestFileGlobSearchTruncated(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	root := globTree(t)
	blocked := filepath.Join(root, "locked")
	if err := os.Mkdir(blocked, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(blocked, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(blocked, 0o777) })

	s := newSet(t, root)
	res := call(t, s, root, "file_glob_search", map[string]any{"path": ".", "type": "all", "include": "locked"})
	text, _ := res.Body["plain_text_response"].(string)
	if !strings.Contains(text, "[results truncated: time budget or unreadable directory]") {
		t.Errorf("missing the truncated tail in %q", text)
	}
}

// TestFileGlobSearchSymlinks: a link is listed for what it points at, but the
// walker never descends through one, and a broken link is not listed at all.
func TestFileGlobSearchSymlinks(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, root, "a.go", "x\n")
	writeTestFile(t, root, "pkg/x.go", "x\n")
	for link, target := range map[string]string{
		"link_file": "a.go",
		"link_dir":  "pkg",
		"broken":    "nowhere",
	} {
		if err := os.Symlink(target, filepath.Join(root, link)); err != nil {
			t.Fatal(err)
		}
	}
	s := newSet(t, root)

	res := call(t, s, root, "file_glob_search", map[string]any{"path": ".", "type": "all"})
	got := entryPaths(res)
	want := []string{"file:a.go", "dir:link_dir", "file:link_file", "dir:pkg", "file:pkg/x.go"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("entries =\n%v\nwant\n%v", got, want)
	}
}

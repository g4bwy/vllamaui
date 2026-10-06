package builtin

import (
	"fmt"
	"strings"
	"testing"
)

func grepTree(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	writeTestFile(t, root, "code.go", "beta\n")
	writeTestFile(t, root, "one.txt", "alpha\nbeta\ngamma\ndelta\n")
	writeTestFile(t, root, "two.txt", "BETA upper\nnothing\nbeta tail\n")
	writeTestFile(t, root, "sub/three.txt", "x beta y\n")
	writeTestFile(t, root, "node_modules/junk.txt", "beta\n") // never walked
	return root
}

func TestGrepSearch(t *testing.T) {
	root := grepTree(t)
	s := newSet(t, root)

	cases := []struct {
		name   string
		params map[string]any
		want   string
	}{
		{
			name:   "matches under a directory",
			params: map[string]any{"path": root, "pattern": "beta"},
			want: "code.go:beta\none.txt:beta\nsub/three.txt:x beta y\ntwo.txt:beta tail\n" +
				"\n---\nTotal matches: 4\n",
		},
		{
			name:   "line numbers",
			params: map[string]any{"path": root, "pattern": "beta", "return_line_numbers": true},
			want: "code.go:1:beta\none.txt:2:beta\nsub/three.txt:1:x beta y\ntwo.txt:3:beta tail\n" +
				"\n---\nTotal matches: 4\n",
		},
		{
			name:   "ignore case",
			params: map[string]any{"path": root, "pattern": "beta", "ignore_case": true},
			want: "code.go:beta\none.txt:beta\nsub/three.txt:x beta y\ntwo.txt:BETA upper\ntwo.txt:beta tail\n" +
				"\n---\nTotal matches: 5\n",
		},
		{
			name:   "include filters by basename",
			params: map[string]any{"path": root, "pattern": "beta", "include": "*.go"},
			want:   "code.go:beta\n\n---\nTotal matches: 1\n",
		},
		{
			name:   "exclude drops files",
			params: map[string]any{"path": root, "pattern": "beta", "exclude": "*.go"},
			want: "one.txt:beta\nsub/three.txt:x beta y\ntwo.txt:beta tail\n" +
				"\n---\nTotal matches: 3\n",
		},
		{
			name:   "no match",
			params: map[string]any{"path": root, "pattern": "zzz"},
			want:   "\n---\nTotal matches: 0\n",
		},
		{
			name:   "single file keeps the path the caller sent",
			params: map[string]any{"path": "./one.txt", "pattern": "beta"},
			want:   "./one.txt:beta\n\n---\nTotal matches: 1\n",
		},
		{
			// a context line is joined with dashes: path-line-text
			name:   "context after a match",
			params: map[string]any{"path": "one.txt", "pattern": "beta", "context_lines": float64(1)},
			want:   "one.txt-1-alpha\none.txt:2:beta\none.txt-3-gamma\n--\n\n---\nTotal matches: 1\n",
		},
		{
			name:   "context is clamped at the top of the file",
			params: map[string]any{"path": "one.txt", "pattern": "alpha", "context_lines": float64(2)},
			want:   "one.txt:1:alpha\none.txt-2-beta\none.txt-3-gamma\n--\n\n---\nTotal matches: 1\n",
		},
		{
			name:   "context is clamped at the end of the file",
			params: map[string]any{"path": "one.txt", "pattern": "delta", "context_lines": float64(2)},
			want:   "one.txt-2-beta\none.txt-3-gamma\none.txt:4:delta\n--\n\n---\nTotal matches: 1\n",
		},
		{
			name:   "two context blocks, and a context line may repeat",
			params: map[string]any{"path": "one.txt", "pattern": "beta|delta", "context_lines": float64(1)},
			want: "one.txt-1-alpha\none.txt:2:beta\none.txt-3-gamma\n--\n" +
				"one.txt-3-gamma\none.txt:4:delta\n--\n" +
				"\n---\nTotal matches: 2\n",
		},
		{
			name:   "include can name a single file",
			params: map[string]any{"path": root, "pattern": "beta", "include": "one.txt", "context_lines": float64(0)},
			want:   "one.txt:beta\n\n---\nTotal matches: 1\n",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := call(t, s, root, "grep_search", tc.params)
			if res.Error != "" {
				t.Fatalf("unexpected error: %s", res.Error)
			}
			if res.PlainText != tc.want {
				t.Errorf("plain text =\n%q\nwant\n%q", res.PlainText, tc.want)
			}
		})
	}
}

func TestGrepSearchLiteral(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, root, "dots.txt", "a.b\naxb\n")
	s := newSet(t, root)

	res := call(t, s, root, "grep_search", map[string]any{"path": root, "pattern": "a.b"})
	want := "dots.txt:a.b\ndots.txt:axb\n\n---\nTotal matches: 2\n"
	if res.PlainText != want {
		t.Errorf("regex pass =\n%q\nwant\n%q", res.PlainText, want)
	}

	res = call(t, s, root, "grep_search", map[string]any{"path": root, "pattern": "a.b", "literal": true})
	want = "dots.txt:a.b\n\n---\nTotal matches: 1\n"
	if res.PlainText != want {
		t.Errorf("literal pass =\n%q\nwant\n%q", res.PlainText, want)
	}
}

// TestGrepSearchInvalidRegex: llama-server compiles ECMAScript regexes, Go
// compiles RE2. A pattern the C++ accepts can fail here, so the reason has to
// reach the caller.
func TestGrepSearchInvalidRegex(t *testing.T) {
	root := grepTree(t)
	s := newSet(t, root)

	for _, pattern := range []string{"(", "a{2,1}", "(?!beta)x"} {
		res := call(t, s, root, "grep_search", map[string]any{"path": root, "pattern": pattern})
		if !strings.HasPrefix(res.Error, "invalid regex: ") {
			t.Errorf("pattern %q: error = %q, want the invalid regex prefix", pattern, res.Error)
		}
		if res.PlainText != "" {
			t.Errorf("pattern %q: expected no output", pattern)
		}
	}
}

func TestGrepSearchMatchCap(t *testing.T) {
	root := t.TempDir()
	var sb strings.Builder
	for i := 0; i < 150; i++ {
		fmt.Fprintf(&sb, "hit %d\n", i)
	}
	writeTestFile(t, root, "many.txt", sb.String())
	s := newSet(t, root)

	res := call(t, s, root, "grep_search", map[string]any{"path": root, "pattern": "hit"})
	lines := strings.Split(res.PlainText, "\n")
	matches := 0
	for _, l := range lines {
		if strings.HasPrefix(l, "many.txt:") {
			matches++
		}
	}
	if matches != grepMaxResults {
		t.Errorf("printed %d match lines, want %d", matches, grepMaxResults)
	}
	want := "\n---\nTotal matches: 100\n[100 matches limit reached. Narrow the path/pattern/include to see more.]\n"
	if !strings.HasSuffix(res.PlainText, want) {
		t.Errorf("missing the limit tail:\n%q", tail(res.PlainText))
	}
}

func TestGrepSearchPathErrors(t *testing.T) {
	root := grepTree(t)
	s := newSet(t, root)

	res := call(t, s, root, "grep_search", map[string]any{"path": "nope", "pattern": "x"})
	if want := "path does not exist: nope"; res.Error != want {
		t.Errorf("error = %q, want %q", res.Error, want)
	}

	// the second message comes from the directory listing, which grep shares
	// with file_glob_search: it only shows up when the base stops being a
	// directory between the check and the walk
	blocked := root + "/one.txt"
	listing := listEntries(blocked, 0, kindFiles)
	if listing.errMsg != "path does not exist or is not a directory" {
		t.Fatalf("listing error = %q", listing.errMsg)
	}
	if got := listing.errMsg + ": " + blocked; got != "path does not exist or is not a directory: "+blocked {
		t.Errorf("composed error = %q", got)
	}
}

// TestGrepSearchWalksADirAndSkipsJunk checks the walker itself, including the
// message file_glob_search shows for a non-directory base.
func TestGrepSearchWalksADirAndSkipsJunk(t *testing.T) {
	root := grepTree(t)

	res := listEntries(root, 0, kindFiles)
	if res.errMsg != "" || res.truncated {
		t.Fatalf("listing failed: %+v", res)
	}
	for _, e := range res.entries {
		if strings.Contains(e.rel, "node_modules") {
			t.Errorf("junk directory was walked: %q", e.rel)
		}
	}

	res = listEntries(root+"/one.txt", 0, kindFiles)
	if got, want := res.errMsg, "path does not exist or is not a directory"; got != want {
		t.Errorf("errMsg = %q, want %q", got, want)
	}
}

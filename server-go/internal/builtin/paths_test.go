package builtin

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestResolvePath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	cases := []struct {
		name string
		dir  string
		path string
		want string
	}{
		{"empty", "/srv/app", "", "/srv/app"},
		{"relative", "/srv/app", "src/main.go", "/srv/app/src/main.go"},
		{"dot is folded", "/srv/app", "./x/./y", "/srv/app/x/y"},
		{"dotdot is folded", "/srv/app", "../conf/app.yaml", "/srv/conf/app.yaml"},
		{"absolute stays", "/srv/app", "/etc/hosts", "/etc/hosts"},
		{"absolute escapes cwd", "/srv/app", "/var/tmp/other", "/var/tmp/other"},
		{"home file", "/srv/app", "~/notes.txt", filepath.Join(home, "notes.txt")},
		{"home itself", "/srv/app", "~", home},
		{"user home is not expanded", "/srv/app", "~other/f", "/srv/app/~other/f"},
		{"trailing separator", "/srv/app", "logs/", "/srv/app/logs"},
		{"past the root", "/srv/app", "../../../etc/passwd", "/etc/passwd"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := resolvePath(tc.dir, tc.path)
			if got != tc.want {
				t.Errorf("resolvePath(%q, %q) = %q, want %q", tc.dir, tc.path, got, tc.want)
			}
			if escapedDotDot(got) {
				t.Errorf("result still holds a .. element: %q", got)
			}
		})
	}
}

// escapedDotDot reports whether a ".." element survived the resolve.
func escapedDotDot(p string) bool {
	for _, part := range strings.Split(p, "/") {
		if part == ".." {
			return true
		}
	}
	return false
}

func TestAbsDir(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	// a relative base is taken against the process working directory
	if got := absDir("sub"); got != filepath.Join(wd, "sub") {
		t.Errorf("absDir(sub) = %q, want %q", got, filepath.Join(wd, "sub"))
	}
	if got := absDir(""); got != filepath.Clean(wd) {
		t.Errorf("absDir(\"\") = %q, want the process directory %q", got, wd)
	}
}

func TestGlobMatch(t *testing.T) {
	cases := []struct {
		pattern string
		input   string
		want    bool
	}{
		{"*.cpp", "a.cpp", true},
		{"*.cpp", "dir/a.cpp", false},
		{"**", "any/thing/at/all.cpp", true},
		{"**", "", true},
		{"**/x", "a/b/x", true},
		{"**/x", "x", false}, // "**/" needs one separator, as in llama.cpp
		{"a?c", "abc", true},
		{"a?c", "a/c", false},
		{"[abc]", "b", true},
		{"[!abc]", "b", false},
		{"[a-c]", "c", true},
		{"[]]", "]", true},
		{"[", "[", true}, // an unterminated class is a literal "["
		{"[]", "]", false},
		{"src/*.go", "src/a.go", true},
		{"*.go", "a.go", true},
		{"**/*.go", "x.go", false}, // a top-level name needs the basename rule
		{"**/*.go", "a/x.go", true},
	}
	for _, tc := range cases {
		if got := globMatch(tc.pattern, tc.input); got != tc.want {
			t.Errorf("globMatch(%q, %q) = %v, want %v", tc.pattern, tc.input, got, tc.want)
		}
	}
}

// TestGlobMatchNoCatastrophicBacktracking: the matcher branches at every star,
// so a pile of "*a" pairs used to double its work per character of the name.
// Sixteen pairs against a 34 character name took over 20 s, and 40 never came
// back at all, which matters because the match runs after the walk deadline.
// Failed (pattern, name) states are remembered now, so the work is bounded.
// The time bound is loose on purpose: a timing assertion on a loaded CI runner
// is flaky, and the answer being correct is the real point.
func TestGlobMatchNoCatastrophicBacktracking(t *testing.T) {
	name := strings.Repeat("a", 34)

	cases := []struct {
		pattern string
		want    bool
	}{
		{strings.Repeat("*a", 16) + "b", false},  // no "b" to end on
		{strings.Repeat("*a", 40) + "b", false},  // and not enough "a" either
		{strings.Repeat("*a", 16), true},         // the accepted shape still works
		{strings.Repeat("*a", 34), true},         // one "a" per pair, exactly
		{strings.Repeat("**a", 16) + "b", false}, // the same pile with "**"
		{strings.Repeat("*?a", 16) + "b", false}, // and with "?" between
	}

	for _, tc := range cases {
		start := time.Now()
		got := globMatch(tc.pattern, name)
		elapsed := time.Since(start)
		if got != tc.want {
			t.Errorf("globMatch(%q, %q) = %v, want %v", tc.pattern, name, got, tc.want)
		}
		if elapsed > 2*time.Second {
			t.Errorf("globMatch with a %d byte pattern took %v", len(tc.pattern), elapsed)
		}
	}
}

func TestPathGlobMatchAnchoring(t *testing.T) {
	cases := []struct {
		pattern string
		rel     string
		want    bool
	}{
		{"*.go", "deep/a/b/x.go", true}, // no slash: basename at any depth
		{"*.go", "x.go", true},
		{"src/*.go", "pkg/src/x.go", true}, // auto-anchored with "**/"
		{"src/*.go", "src/x.go", false},    // so a top-level dir is missed, as in llama.cpp
		{"/abs/*.go", "abs/x.go", false},   // already anchored; rel paths have no leading "/"
		{"**/src/*.go", "pkg/src/x.go", true},
		{"pkg/**", "pkg/a/b.go", false}, // "**/" is prefixed, so a top-level dir never matches
		{"pkg/**", "x/pkg/a/b.go", true},
	}
	for _, tc := range cases {
		if got := pathGlobMatch(tc.pattern, tc.rel); got != tc.want {
			t.Errorf("pathGlobMatch(%q, %q) = %v, want %v", tc.pattern, tc.rel, got, tc.want)
		}
	}
}

func TestSplitLines(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"", nil},
		{"a", []string{"a"}},
		{"a\n", []string{"a"}},
		{"a\nb", []string{"a", "b"}},
		{"a\n\nb\n", []string{"a", "", "b"}},
		{"\r\n", []string{"\r"}},
	}
	for _, tc := range cases {
		got := splitLines(tc.in)
		if len(got) != len(tc.want) {
			t.Errorf("splitLines(%q) = %q, want %q", tc.in, got, tc.want)
			continue
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("splitLines(%q) = %q, want %q", tc.in, got, tc.want)
				break
			}
		}
	}
}

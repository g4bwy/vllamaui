package builtin

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"llama-webui/server/internal/contracts"
)

func TestWriteFile(t *testing.T) {
	root := t.TempDir()
	s := newSet(t, root)

	cases := []struct {
		name    string
		cwd     string
		params  map[string]any
		want    string // expected file content
		wantErr string
		abs     bool // path is absolute, so check it verbatim
	}{
		{
			name:   "create with parents",
			cwd:    root,
			params: map[string]any{"path": "a/b/c.txt", "content": "deep\n"},
			want:   "deep\n",
		},
		{
			name:   "overwrite",
			cwd:    root,
			params: map[string]any{"path": "over.txt", "content": "second"},
			want:   "second",
		},
		{
			name:   "bytes counts utf8 bytes",
			cwd:    root,
			params: map[string]any{"path": "u.txt", "content": "héllo"},
			want:   "héllo",
		},
		{
			name:   "empty content",
			cwd:    root,
			params: map[string]any{"path": "e.txt", "content": ""},
			want:   "",
		},
		{
			name:   "outside the cwd is allowed",
			cwd:    root,
			params: map[string]any{"path": filepath.Join(root, "..", "sibling", "s.txt"), "content": "out\n"},
			want:   "out\n",
			abs:    true,
		},
		{
			name:    "a parent that is a file fails",
			cwd:     root,
			params:  map[string]any{"path": "over.txt/nested.txt", "content": "x"},
			wantErr: "failed to write file: over.txt/nested.txt",
		},
	}

	writeTestFile(t, root, "over.txt", "first")

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := call(t, s, tc.cwd, "write_file", tc.params)
			if tc.wantErr != "" {
				if res.Error != tc.wantErr {
					t.Fatalf("error = %q, want %q", res.Error, tc.wantErr)
				}
				return
			}
			if res.Error != "" {
				t.Fatalf("unexpected error: %s", res.Error)
			}
			path, _ := tc.params["path"].(string)
			full := path
			if !tc.abs {
				full = filepath.Join(tc.cwd, path)
			}
			got, err := os.ReadFile(full)
			if err != nil {
				t.Fatalf("reading %s: %v", full, err)
			}
			if string(got) != tc.want {
				t.Errorf("content = %q, want %q", got, tc.want)
			}
			if res.Body["result"] != "file written successfully" || res.Body["path"] != path {
				t.Errorf("body = %v", res.Body)
			}
			if res.Body["bytes"] != len(tc.want) {
				t.Errorf("bytes = %v, want %d", res.Body["bytes"], len(tc.want))
			}
		})
	}
}

func TestEditFile(t *testing.T) {
	cases := []struct {
		name     string
		content  string
		edits    any
		want     string // expected content after the edit
		wantErr  string
		wantAppl int
	}{
		{
			name:     "one exact edit",
			content:  "hello world\n",
			edits:    edits("hello", "goodbye"),
			want:     "goodbye world\n",
			wantAppl: 1,
		},
		{
			name:     "two edits are read against the original content",
			content:  "foo bar\n",
			edits:    edits("foo", "bar", "bar", "baz"),
			want:     "bar baz\n",
			wantAppl: 2,
		},
		{
			name:     "an edit may delete text",
			content:  "keep drop keep\n",
			edits:    edits(" drop", ""),
			want:     "keep keep\n",
			wantAppl: 1,
		},
		{
			name:     "trailing blanks are folded away for matching",
			content:  "keep  \nfoo bar  \nend\n",
			edits:    edits("keep\nfoo bar\n", "REPLACED\n"),
			want:     "REPLACED\nend\n",
			wantAppl: 1,
		},
		{
			name:     "an untouched line keeps its original bytes",
			content:  "a  \nb   \nc  \n",
			edits:    edits("a\nb", "X\n"),
			want:     "X\n\nc  \n", // the touched line is replaced whole
			wantAppl: 1,
		},
		{
			name:     "typographic quotes fold to ascii",
			content:  "say \u201chello\u201d\n",
			edits:    edits(`say "hello"`, "say \"hi\""),
			want:     "say \"hi\"\n",
			wantAppl: 1,
		},
		{
			name:    "empty edits array",
			content: "x\n",
			edits:   []any{},
			wantErr: `"edits" must be a non-empty array`,
		},
		{
			name:    "edits is not an array",
			content: "x\n",
			edits:   "nope",
			wantErr: `"edits" must be a non-empty array`,
		},
		{
			name:    "edits element is not an object",
			content: "x\n",
			edits:   []any{"just a string"},
			wantErr: "type must be object, but is string",
		},
		{
			name:    "element without new_text",
			content: "x\n",
			edits:   []any{map[string]any{"old_text": "x"}},
			wantErr: "key 'new_text' not found",
		},
		{
			name:    "empty old_text",
			content: "x\n",
			edits:   append(edits("x", "y").([]any), map[string]any{"old_text": "", "new_text": "z"}),
			wantErr: "edits[1].old_text must not be empty",
		},
		{
			name:    "text is not in the file",
			content: "x\n",
			edits:   edits("absent", "y"),
			wantErr: "could not find edits[0].old_text in f.txt, it must match the file's current content exactly",
		},
		{
			name:    "text appears twice",
			content: "dup\ndup\n",
			edits:   edits("dup", "once"),
			wantErr: "found 2 occurrences of edits[0].old_text in f.txt, it must be unique",
		},
		{
			name:    "uniqueness is judged on normalized text",
			content: "dup \ndup\n",
			edits:   edits("dup", "once"),
			wantErr: "found 2 occurrences of edits[0].old_text in f.txt, it must be unique",
		},
		{
			name:    "overlapping edits",
			content: "abcdef\n",
			edits:   edits("abcd", "1", "cdef", "2"),
			wantErr: "edits[0] and edits[1] overlap in f.txt; merge them into one edit or target disjoint regions",
		},
		{
			name:    "replacement changes nothing",
			content: "same\n",
			edits:   edits("same", "same"),
			wantErr: "no changes made: the replacement(s) produced identical content",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			writeTestFile(t, root, "f.txt", tc.content)
			s := newSet(t, root)

			res := call(t, s, root, "edit_file", map[string]any{"path": "f.txt", "edits": tc.edits})
			if tc.wantErr != "" {
				if res.Error != tc.wantErr {
					t.Errorf("error = %q, want %q", res.Error, tc.wantErr)
				}
				got, _ := os.ReadFile(filepath.Join(root, "f.txt"))
				if string(got) != tc.content {
					t.Errorf("the file changed on an error: %q", got)
				}
				return
			}
			if res.Error != "" {
				t.Fatalf("unexpected error: %s", res.Error)
			}
			got, err := os.ReadFile(filepath.Join(root, "f.txt"))
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != tc.want {
				t.Errorf("content = %q, want %q", got, tc.want)
			}
			if res.Body["edits_applied"] != tc.wantAppl {
				t.Errorf("edits_applied = %v, want %d", res.Body["edits_applied"], tc.wantAppl)
			}
			if res.Body["result"] != "file edited successfully" || res.Body["path"] != "f.txt" {
				t.Errorf("body = %v", res.Body)
			}
		})
	}
}

// edits builds an edits array from old, new pairs.
func edits(pairs ...string) any {
	out := make([]any, 0, len(pairs)/2)
	for i := 0; i+1 < len(pairs); i += 2 {
		out = append(out, map[string]any{"old_text": pairs[i], "new_text": pairs[i+1]})
	}
	return out
}

func TestEditFileOpenAndWriteErrors(t *testing.T) {
	root := t.TempDir()
	s := newSet(t, root)

	res := call(t, s, root, "edit_file", map[string]any{"path": "gone.txt", "edits": edits("a", "b")})
	if want := "failed to open file: gone.txt"; res.Error != want {
		t.Errorf("error = %q, want %q", res.Error, want)
	}

	res = call(t, s, root, "edit_file", map[string]any{"path": "edits.txt"})
	if want := "key 'edits' not found"; res.Error != want {
		t.Errorf("error = %q, want %q", res.Error, want)
	}

	if err := os.Mkdir(filepath.Join(root, "adir"), 0o777); err != nil {
		t.Fatal(err)
	}
	res = call(t, s, root, "edit_file", map[string]any{"path": "adir", "edits": edits("a", "b")})
	if want := "failed to open file: adir"; res.Error != want {
		t.Errorf("error = %q, want %q", res.Error, want)
	}

	if os.Geteuid() != 0 {
		name := filepath.Join(root, "ro.txt")
		writeTestFile(t, root, "ro.txt", "target\n")
		if err := os.Chmod(name, 0o444); err != nil {
			t.Fatal(err)
		}
		res := call(t, s, root, "edit_file", map[string]any{"path": "ro.txt", "edits": edits("target", "changed")})
		if want := "failed to write file: ro.txt"; res.Error != want {
			t.Errorf("error = %q, want %q", res.Error, want)
		}
	}
}

func TestEditFileMultipleBlocks(t *testing.T) {
	root := t.TempDir()
	content := "alpha\nbravo charlie\ndelta\necho foxtrot\n"
	writeTestFile(t, root, "f.txt", content)
	s := newSet(t, root)

	res := call(t, s, root, "edit_file", map[string]any{
		"path":  "f.txt",
		"edits": edits("alpha", "ALPHA", "delta", "DELTA", "echo foxtrot", "ECHO"),
	})
	if res.Error != "" {
		t.Fatalf("unexpected error: %s", res.Error)
	}
	want := "ALPHA\nbravo charlie\nDELTA\nECHO\n"
	got, _ := os.ReadFile(filepath.Join(root, "f.txt"))
	if string(got) != want {
		t.Errorf("content = %q, want %q", got, want)
	}
	if res.Body["edits_applied"] != 3 {
		t.Errorf("edits_applied = %v, want 3", res.Body["edits_applied"])
	}
}

func TestGetInfo(t *testing.T) {
	root := t.TempDir()
	s := newSet(t, root)

	res := call(t, s, root, "get_info", map[string]any{})
	if res.Error != "" {
		t.Fatalf("unexpected error: %s", res.Error)
	}
	osInfo, _ := res.Body["os"].(string)
	if osInfo == "" {
		t.Error("os must not be empty")
	}
	if osInfo != "unknown" && !strings.Contains(osInfo, " ") {
		t.Errorf("os = %q, want the uname line or unknown", osInfo)
	}
	if res.Body["cwd"] != root {
		t.Errorf("cwd = %v, want %v", res.Body["cwd"], root)
	}
	if len(res.Body) != 2 {
		t.Errorf("body keys = %v, want os and cwd only", res.Body)
	}

	raw := res.JSON()
	for _, key := range []string{`"os":`, `"cwd":`} {
		if !strings.Contains(string(raw), key) {
			t.Errorf("wire json %s misses %s", raw, key)
		}
	}
}

// TestGetInfoWithoutUname covers the "unknown" fallback.
func TestGetInfoWithoutUname(t *testing.T) {
	root := t.TempDir()
	t.Setenv("PATH", filepath.Join(root, "no-such-bin"))
	s, err := New([]string{"get_info"}, root)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	tool, _ := mustTool(t, s, "get_info")
	res := tool.Invoke(t.Context(), contracts.ToolRequest{Name: "get_info", Cwd: root}, nil)
	if res.Body["os"] != "unknown" {
		t.Errorf("os = %v, want unknown", res.Body["os"])
	}
}

func mustTool(t *testing.T, s *Set, name string) (contracts.Tool, bool) {
	t.Helper()
	tool, ok := s.Get(name)
	if !ok {
		t.Fatalf("tool %q is not registered", name)
	}
	return tool, ok
}

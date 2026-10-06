package builtin

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type listKind int

const (
	kindFiles listKind = iota
	kindDirs
	kindAll
)

type listEntry struct {
	rel   string
	isDir bool
}

type listing struct {
	entries   []listEntry
	errMsg    string // set when base is not a directory
	truncated bool   // the walk could not see everything
}

// isJunkDir names the directories a listing reports but never descends into:
// they can be enormous. Same set as SERVER_TOOL_JUNK_DIR_NAMES, matched
// case-sensitively as on POSIX.
func isJunkDir(name string) bool {
	switch name {
	case ".git", ".svn", ".hg", "node_modules", "__pycache__",
		".venv", "venv", "dist", "build", "target", ".cache", ".idea", ".vscode":
		return true
	}
	return false
}

// listEntries walks base and returns entries relative to it. maxDepth 0 means
// unlimited, 1 means the direct children only.
//
// The C++ has a "git ls-files" fast path and a stack walker with no ordering
// guarantee. Only the walker is implemented here, and its result is sorted, so
// a listing and the limit cut are stable across calls.
func listEntries(base string, maxDepth int, kind listKind) listing {
	var res listing

	if st, err := os.Stat(base); err != nil || !st.IsDir() {
		res.errMsg = "path does not exist or is not a directory"
		return res
	}

	deadline := time.Now().Add(listTimeoutSecs * time.Second)

	type frame struct {
		dir   string
		rel   string
		depth int
	}
	stack := []frame{{dir: base}}

	for len(stack) > 0 {
		f := stack[len(stack)-1]
		stack = stack[:len(stack)-1]

		dir, err := os.Open(f.dir)
		if err != nil {
			res.truncated = true
			continue
		}
		for {
			if time.Now().After(deadline) {
				dir.Close()
				res.truncated = true
				sortEntries(res.entries)
				return res
			}
			names, err := dir.ReadDir(1)
			for _, e := range names {
				rel := f.rel + e.Name()
				full := filepath.Join(f.dir, e.Name())
				isDir, regular := classify(e, full)
				switch {
				case isDir:
					if kind == kindDirs || kind == kindAll {
						res.entries = append(res.entries, listEntry{rel: rel, isDir: true})
					}
					if isJunkDir(e.Name()) {
						continue
					}
					if e.Type()&os.ModeSymlink != 0 {
						continue // a link can point back to an ancestor
					}
					if maxDepth == 0 || f.depth+1 < maxDepth {
						stack = append(stack, frame{dir: full, rel: rel + "/", depth: f.depth + 1})
					}
				case regular:
					if kind == kindFiles || kind == kindAll {
						res.entries = append(res.entries, listEntry{rel: rel})
					}
				}
			}
			if err != nil {
				if !errors.Is(err, io.EOF) {
					res.truncated = true
				}
				break
			}
		}
		dir.Close()
	}

	sortEntries(res.entries)
	return res
}

func sortEntries(entries []listEntry) {
	sort.Slice(entries, func(i, j int) bool { return entries[i].rel < entries[j].rel })
}

// classify reports what an entry is, following a symlink to a target that
// exists. An entry whose target is gone is skipped, like the C++.
func classify(e os.DirEntry, path string) (isDir bool, regular bool) {
	if e.Type()&os.ModeSymlink == 0 {
		return e.IsDir(), e.Type().IsRegular()
	}
	st, err := os.Stat(path)
	if err != nil {
		return false, false
	}
	return st.IsDir(), st.Mode().IsRegular()
}

// splitLines mirrors std::getline over content: "a\nb" gives two lines, and a
// trailing newline does not add an empty one.
func splitLines(content string) []string {
	if content == "" {
		return nil
	}
	lines := strings.Split(content, "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

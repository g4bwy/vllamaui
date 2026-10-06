package builtin

import (
	"os"
	"path/filepath"
	"strings"
)

// resolvePath mirrors tools_io_basic::resolve: a leading "~" expands to the
// home directory, a relative path is joined onto dir, and the result is
// cleaned so "." and ".." never reach the filesystem or the response.
//
// The path is not confined to dir: llama-server happily reads outside the
// working directory, and a rewrite that refused would surprise clients.
func resolvePath(dir, path string) string {
	p := expandHome(path)
	if !filepath.IsAbs(p) {
		p = filepath.Join(dir, p)
	}
	return filepath.Clean(p)
}

// expandHome replaces a lone "~" or a "~/..." prefix. "~user" is left alone.
func expandHome(path string) string {
	if path == "" || path[0] != '~' {
		return path
	}
	if len(path) > 1 && path[1] != '/' && path[1] != '\\' {
		return path
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return path
	}
	return home + path[1:]
}

// absDir turns a working directory into a clean absolute path. An empty string
// means "the process working directory", like llama-server.
func absDir(dir string) string {
	p := expandHome(dir)
	if !filepath.IsAbs(p) {
		wd, err := os.Getwd()
		if err != nil {
			return filepath.Clean(p)
		}
		p = filepath.Join(wd, p)
	}
	return filepath.Clean(p)
}

// baseName is fs::path::filename for '/'-separated paths: everything after the
// last separator, empty when the path ends with one.
func baseName(path string) string {
	if i := strings.LastIndexByte(path, '/'); i >= 0 {
		return path[i+1:]
	}
	return path
}

// globMatch is a port of glob_match() in common/common.cpp: "*" matches within
// one path segment, "**" matches anything including "/", "?" is one character
// that is not "/", and "[...]" is a character class with ranges and "!".
//
// The matcher branches at every star, so a pile of "*a" pairs before a trailing
// "b" nearly doubles its work every two characters of the name: 16 pairs
// against a 34 character name took over 20 s, and 40 never came back. That is
// far beyond any walk deadline, because the match runs once the listing is
// done. Each state depends on (pi, si) alone, so a state that failed once is
// remembered and fails at once on revisit. The work is then bounded by
// len(pattern) x len(name), with the same accept and reject results.
func globMatch(pattern, s string) bool {
	m := globMatcher{p: []byte(pattern), s: []byte(s), cols: len(s) + 1}
	return m.at(0, 0)
}

// globMatcher is one pattern and name paired up, plus what has already failed.
type globMatcher struct {
	p, s []byte
	cols int    // width of the failed table, one row per pattern index
	bad  []bool // failed (pi, si) pairs at pi*cols+si, allocated on the first failure
}

// at is the memo shell around step: it answers a repeat of a failed state
// without walking into it again.
func (m *globMatcher) at(pi, si int) bool {
	if pi == len(m.p) {
		return si == len(m.s)
	}
	i := pi*m.cols + si
	if m.bad != nil && m.bad[i] {
		return false
	}
	if m.step(pi, si) {
		return true
	}
	if m.bad == nil {
		m.bad = make([]bool, (len(m.p)+1)*m.cols)
	}
	m.bad[i] = true
	return false
}

// step matches p[pi:] against s[si:]. Every recursive call moves forward on at
// least one side, so the depth is bounded by the two lengths added together.
func (m *globMatcher) step(pi, si int) bool {
	p, s := m.p, m.s
	if p[pi] == '*' && pi+1 < len(p) && p[pi+1] == '*' {
		if m.at(pi+2, si) {
			return true
		}
		if si < len(s) {
			return m.at(pi, si+1)
		}
		return false
	}
	if p[pi] == '*' {
		for si < len(s) && s[si] != '/' {
			if m.at(pi+1, si) {
				return true
			}
			si++
		}
		return m.at(pi+1, si)
	}
	if p[pi] == '?' && si < len(s) && s[si] != '/' {
		return m.at(pi+1, si+1)
	}
	if p[pi] == '[' {
		end := pi + 1
		if end < len(p) && (p[end] == ']' || p[end] == '-') {
			end++
		}
		for end < len(p) && p[end] != ']' {
			end++
		}
		if end < len(p) {
			if si == len(s) {
				return false
			}
			return globClass(p[pi+1:end], s[si]) && m.at(end+1, si+1)
		}
		if si < len(s) && s[si] == '[' {
			return m.at(pi+1, si+1)
		}
		return false
	}
	if si < len(s) && p[pi] == s[si] {
		return m.at(pi+1, si+1)
	}
	return false
}

// globClass reports whether c matches the body of a [...] class, which may
// start with "!" to negate it.
func globClass(class []byte, c byte) bool {
	negated := false
	if len(class) > 0 && class[0] == '!' {
		negated = true
		class = class[1:]
	}
	if len(class) > 0 && (class[0] == ']' || class[0] == '-') {
		if class[0] == c {
			return !negated
		}
		class = class[1:]
	}
	matched := false
	for len(class) > 0 {
		if len(class) > 2 && class[1] == '-' && class[2] != ']' {
			if c >= class[0] && c <= class[2] {
				matched = true
				break
			}
			class = class[3:]
			continue
		}
		if class[0] == c {
			matched = true
			break
		}
		class = class[1:]
	}
	return matched != negated
}

// pathGlobMatch is path_glob_match(): a pattern without "/" is matched against
// the basename at any depth, otherwise against the relative path, which gets a
// leading "**/" unless it is already anchored.
func pathGlobMatch(pattern, rel string) bool {
	if !strings.Contains(pattern, "/") {
		return globMatch(pattern, baseName(rel))
	}
	if pattern == "**" || strings.HasPrefix(pattern, "**/") || strings.HasPrefix(pattern, "/") {
		return globMatch(pattern, rel)
	}
	return globMatch("**/"+pattern, rel)
}

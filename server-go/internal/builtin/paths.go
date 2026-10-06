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
func globMatch(pattern, s string) bool {
	return globMatchAt([]byte(pattern), 0, []byte(s), 0)
}

func globMatchAt(p []byte, pi int, s []byte, si int) bool {
	if pi == len(p) {
		return si == len(s)
	}
	if p[pi] == '*' && pi+1 < len(p) && p[pi+1] == '*' {
		if globMatchAt(p, pi+2, s, si) {
			return true
		}
		if si < len(s) {
			return globMatchAt(p, pi, s, si+1)
		}
		return false
	}
	if p[pi] == '*' {
		for si < len(s) && s[si] != '/' {
			if globMatchAt(p, pi+1, s, si) {
				return true
			}
			si++
		}
		return globMatchAt(p, pi+1, s, si)
	}
	if p[pi] == '?' && si < len(s) && s[si] != '/' {
		return globMatchAt(p, pi+1, s, si+1)
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
			return globClass(p[pi+1:end], s[si]) && globMatchAt(p, end+1, s, si+1)
		}
		if si < len(s) && s[si] == '[' {
			return globMatchAt(p, pi+1, s, si+1)
		}
		return false
	}
	if si < len(s) && p[pi] == s[si] {
		return globMatchAt(p, pi+1, s, si+1)
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

package builtin

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"llama-webui/server/internal/contracts"
)

func grepSearch(_ context.Context, s *Set, req contracts.ToolRequest, _ contracts.Sink) contracts.Result {
	params := req.Params
	path, bad := reqString(params, "path")
	if bad != "" {
		return fail(bad)
	}
	pattern, bad := reqString(params, "pattern")
	if bad != "" {
		return fail(bad)
	}
	include := optString(params, "include", "**")
	exclude := optString(params, "exclude", "")
	showLineNo := optBool(params, "return_line_numbers", false)
	literal := optBool(params, "literal", false)
	ignoreCase := optBool(params, "ignore_case", false)
	ctxLines := optInt(params, "context_lines", 0)
	if ctxLines < 0 {
		ctxLines = 0
	}

	src := pattern
	if literal {
		src = regexp.QuoteMeta(pattern)
	}
	if ignoreCase {
		src = "(?i)" + src
	}
	re, err := regexp.Compile(src)
	if err != nil {
		// Go's RE2 is not the ECMAScript dialect llama-server uses, so a
		// pattern can fail here and work there. Say why.
		return fail("invalid regex: " + err.Error())
	}

	dir := s.dir(req)
	absPath := resolvePath(dir, path)

	type fileEntry struct {
		full    string
		display string
	}
	var files []fileEntry

	if isRegularFile(absPath) {
		files = append(files, fileEntry{full: absPath, display: path})
	} else if isDirectory(absPath) {
		res := listEntries(absPath, 0, kindFiles)
		if res.errMsg != "" {
			return fail(res.errMsg + ": " + path)
		}
		for _, e := range res.entries {
			if !pathGlobMatch(include, e.rel) {
				continue
			}
			if exclude != "" && pathGlobMatch(exclude, e.rel) {
				continue
			}
			files = append(files, fileEntry{full: filepath.Join(absPath, e.rel), display: e.rel})
		}
	} else {
		return fail("path does not exist: " + path)
	}

	var sb strings.Builder
	total := 0
	limitReached := false
	showNum := showLineNo || ctxLines > 0

	for _, f := range files {
		if limitReached {
			break
		}
		content, err := os.ReadFile(f.full)
		if err != nil {
			continue
		}
		lines := splitLines(string(content))
		for i, line := range lines {
			if total >= grepMaxResults {
				limitReached = true
				break
			}
			if !re.MatchString(line) {
				continue
			}
			start, end := i, i
			if ctxLines > 0 {
				start = i - ctxLines
				if start < 0 {
					start = 0
				}
				end = i + ctxLines
				if end > len(lines)-1 {
					end = len(lines) - 1
				}
			}
			for j := start; j <= end; j++ {
				isMatch := j == i
				sep := "-"
				if isMatch {
					sep = ":"
				}
				sb.WriteString(f.display)
				sb.WriteString(sep)
				if showNum {
					fmt.Fprintf(&sb, "%d%s", j+1, sep)
				}
				sb.WriteString(lines[j])
				sb.WriteString("\n")
			}
			if ctxLines > 0 {
				sb.WriteString("--\n")
			}
			total++
		}
	}

	fmt.Fprintf(&sb, "\n---\nTotal matches: %d\n", total)
	if limitReached {
		fmt.Fprintf(&sb, "[%d matches limit reached. Narrow the path/pattern/include to see more.]\n",
			grepMaxResults)
	}

	return contracts.Result{PlainText: sb.String()}
}

func isRegularFile(path string) bool {
	st, err := os.Stat(path)
	return err == nil && st.Mode().IsRegular()
}

func isDirectory(path string) bool {
	st, err := os.Stat(path)
	return err == nil && st.IsDir()
}

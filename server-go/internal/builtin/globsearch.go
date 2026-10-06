package builtin

import (
	"context"
	"fmt"
	"strings"

	"llama-webui/server/internal/contracts"
)

func fileGlobSearch(_ context.Context, s *Set, req contracts.ToolRequest, _ contracts.Sink) contracts.Result {
	params := req.Params
	path, bad := reqString(params, "path")
	if bad != "" {
		return fail(bad)
	}
	include := optString(params, "include", "**")
	exclude := optString(params, "exclude", "")
	kind := optString(params, "type", "file")
	maxDepth := optInt(params, "max_depth", 0)
	if maxDepth < 0 {
		maxDepth = 0
	}
	limit := optInt(params, "limit", searchMaxResults)
	if limit < 1 {
		return fail(fmt.Sprintf("invalid limit: %d (expected 1 or more)", limit))
	}
	if limit > searchMaxResults {
		limit = searchMaxResults
	}

	switch kind {
	case "file", "dir", "all":
	default:
		return fail(fmt.Sprintf("invalid type: %s (expected \"file\", \"dir\" or \"all\")", kind))
	}

	base := resolvePath(s.dir(req), path)
	var listKind listKind
	switch kind {
	case "file":
		listKind = kindFiles
	case "dir":
		listKind = kindDirs
	default:
		listKind = kindAll
	}

	res := listEntries(base, maxDepth, listKind)
	if res.errMsg != "" {
		return fail(res.errMsg + ": " + path)
	}

	var matches []listEntry
	for _, e := range res.entries {
		if !pathGlobMatch(include, e.rel) {
			continue
		}
		if exclude != "" && pathGlobMatch(exclude, e.rel) {
			continue
		}
		matches = append(matches, e)
	}
	// the listing comes back sorted, so the filter keeps that order

	total := len(matches)
	shown := total
	if shown > limit {
		shown = limit
	}

	var sb strings.Builder
	entries := make([]any, 0, shown)
	for _, e := range matches[:shown] {
		sb.WriteString(e.rel)
		if e.isDir {
			sb.WriteString("/")
		}
		sb.WriteString("\n")
		entryType := "file"
		if e.isDir {
			entryType = "dir"
		}
		entries = append(entries, map[string]any{"path": e.rel, "type": entryType})
	}

	fmt.Fprintf(&sb, "\n---\nTotal matches: %d\n", total)
	if total > shown {
		fmt.Fprintf(&sb, "[%d results limit reached (%d total matches). Refine the glob pattern to narrow the search.]\n",
			shown, total)
	}
	if res.truncated {
		sb.WriteString("[results truncated: time budget or unreadable directory]\n")
	}

	// The body carries everything, including the text the model sees, so the
	// UI can read entries and base without re-parsing the text.
	return contracts.Result{Body: map[string]any{
		"plain_text_response": sb.String(),
		"entries":             entries,
		"base":                base,
	}}
}

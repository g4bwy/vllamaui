package builtin

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"

	"llama-webui/server/internal/contracts"
)

type editReq struct {
	oldText string
	newText string
}

type matchedEdit struct {
	editIndex   int
	matchIndex  int // offset into the base content
	matchLength int
	newText     string
}

func editFile(_ context.Context, s *Set, req contracts.ToolRequest, _ contracts.Sink) contracts.Result {
	params := req.Params
	path, bad := reqString(params, "path")
	if bad != "" {
		return fail(bad)
	}

	v, ok := params["edits"]
	if !ok {
		return fail(keyNotFound("edits"))
	}
	list, _ := v.([]any)
	if len(list) == 0 {
		return fail(`"edits" must be a non-empty array`)
	}

	edits := make([]editReq, 0, len(list))
	for i, item := range list {
		m, ok := item.(map[string]any)
		if !ok {
			return fail(wrongType("object", item))
		}
		oldText, bad := reqString(m, "old_text")
		if bad != "" {
			return fail(bad)
		}
		newText, bad := reqString(m, "new_text")
		if bad != "" {
			return fail(bad)
		}
		if oldText == "" {
			return fail(fmt.Sprintf("edits[%d].old_text must not be empty", i))
		}
		edits = append(edits, editReq{oldText, newText})
	}

	full := resolvePath(s.dir(req), path)
	content, err := os.ReadFile(full)
	if err != nil {
		return fail("failed to open file: " + path)
	}
	original := string(content)
	normOriginal := normalizeForFuzzy(original)

	// an old_text with no exact hit still matches when the file and the edit
	// differ only in trailing blanks or typographic quotes
	anyFuzzy := false
	for i := range edits {
		if strings.Contains(original, edits[i].oldText) {
			continue
		}
		if !strings.Contains(normOriginal, normalizeForFuzzy(edits[i].oldText)) {
			return fail(fmt.Sprintf("could not find edits[%d].old_text in %s, it must match the file's current content exactly", i, path))
		}
		anyFuzzy = true
	}

	baseContent := original
	if anyFuzzy {
		baseContent = normOriginal
	}

	// uniqueness is always judged on the normalized text, so a whitespace-only
	// duplicate still counts
	matched := make([]matchedEdit, 0, len(edits))
	for i := range edits {
		normOld := normalizeForFuzzy(edits[i].oldText)
		if occ := countOccurrences(normOriginal, normOld); occ > 1 {
			return fail(fmt.Sprintf("found %d occurrences of edits[%d].old_text in %s, it must be unique", occ, i, path))
		}
		needle := edits[i].oldText
		if anyFuzzy {
			needle = normOld
		}
		idx := strings.Index(baseContent, needle)
		if idx < 0 {
			return fail(fmt.Sprintf("could not find edits[%d].old_text in %s, it must match the file's current content exactly", i, path))
		}
		matched = append(matched, matchedEdit{editIndex: i, matchIndex: idx, matchLength: len(needle), newText: edits[i].newText})
	}

	sort.Slice(matched, func(i, j int) bool { return matched[i].matchIndex < matched[j].matchIndex })
	for i := 1; i < len(matched); i++ {
		prev, cur := matched[i-1], matched[i]
		if prev.matchIndex+prev.matchLength > cur.matchIndex {
			return fail(fmt.Sprintf("edits[%d] and edits[%d] overlap in %s; merge them into one edit or target disjoint regions",
				prev.editIndex, cur.editIndex, path))
		}
	}

	// every edit is cut out of the original content, never out of the result of
	// the previous one
	newContent := applyReplacements(baseContent, matched, 0)
	if anyFuzzy {
		newContent = applyPreservingUnchangedLines(original, baseContent, matched)
	}

	if newContent == original {
		return fail("no changes made: the replacement(s) produced identical content")
	}

	if err := writeThrough(full, newContent); err != nil {
		return fail("failed to write file: " + path)
	}

	return contracts.Result{Body: map[string]any{
		"result":        "file edited successfully",
		"path":          path,
		"edits_applied": len(matched),
	}}
}

func countOccurrences(content, needle string) int {
	if needle == "" {
		return 0
	}
	count := 0
	pos := 0
	for {
		i := strings.Index(content[pos:], needle)
		if i < 0 {
			return count
		}
		count++
		pos += i + len(needle)
	}
}

type replacement struct {
	from string
	to   string
}

// fuzzyReplacements is the ordered table normalizeLine applies: smart quotes,
// dashes and exotic spaces fold to ASCII.
func fuzzyReplacements() []replacement {
	var reps []replacement
	seq := func(b byte) string { return string([]byte{0xE2, 0x80, b}) }
	for _, b := range []byte{0x98, 0x99, 0x9A, 0x9B} {
		reps = append(reps, replacement{seq(b), "'"})
	}
	for _, b := range []byte{0x9C, 0x9D, 0x9E, 0x9F} {
		reps = append(reps, replacement{seq(b), `"`})
	}
	for b := byte(0x90); b <= 0x95; b++ {
		reps = append(reps, replacement{seq(b), "-"})
	}
	reps = append(reps, replacement{string([]byte{0xE2, 0x88, 0x92}), "-"})
	reps = append(reps, replacement{string([]byte{0xC2, 0xA0}), " "})
	for b := byte(0x82); b <= 0x8A; b++ {
		reps = append(reps, replacement{seq(b), " "})
	}
	reps = append(reps,
		replacement{string([]byte{0xE2, 0x80, 0xAF}), " "},
		replacement{string([]byte{0xE2, 0x81, 0x9F}), " "},
		replacement{string([]byte{0xE3, 0x80, 0x80}), " "})
	return reps
}

// normalizeForFuzzy folds each line and keeps the line count, so offsets in
// the normalized text still point at the right line.
func normalizeForFuzzy(content string) string {
	reps := fuzzyReplacements()
	var sb strings.Builder
	for {
		line := content
		isLast := true
		if nl := strings.IndexByte(content, '\n'); nl >= 0 {
			line = content[:nl]
			content = content[nl+1:]
			isLast = false
		}
		sb.WriteString(normalizeLine(line, reps))
		if isLast {
			break
		}
		sb.WriteByte('\n')
	}
	return sb.String()
}

func normalizeLine(line string, reps []replacement) string {
	line = strings.TrimRight(line, " \t\r")
	for _, r := range reps {
		line = strings.ReplaceAll(line, r.from, r.to)
	}
	return line
}

// splitLinesWithEndings keeps the '\n' on each line, so untouched lines can be
// put back verbatim.
func splitLinesWithEndings(content string) []string {
	var lines []string
	for start := 0; start < len(content); {
		nl := strings.IndexByte(content[start:], '\n')
		if nl < 0 {
			return append(lines, content[start:])
		}
		lines = append(lines, content[start:start+nl+1])
		start += nl + 1
	}
	return lines
}

type lineSpan struct {
	start int
	end   int
}

func getLineSpans(content string) []lineSpan {
	var spans []lineSpan
	offset := 0
	for _, line := range splitLinesWithEndings(content) {
		spans = append(spans, lineSpan{start: offset, end: offset + len(line)})
		offset += len(line)
	}
	return spans
}

// replacementLineRange widens a replacement to the lines of spans it touches.
func replacementLineRange(spans []lineSpan, matchIndex, matchLength int) (int, int, bool) {
	replacementEnd := matchIndex + matchLength

	startLine := -1
	for i, sp := range spans {
		if matchIndex >= sp.start && matchIndex < sp.end {
			startLine = i
			break
		}
	}
	if startLine < 0 {
		return 0, 0, false
	}

	endLine := startLine
	for endLine < len(spans) && spans[endLine].end < replacementEnd {
		endLine++
	}
	if endLine >= len(spans) {
		return 0, 0, false
	}
	return startLine, endLine + 1, true
}

// applyReplacements puts the edits into content, back to front, so the offsets
// of the earlier ones stay valid.
func applyReplacements(content string, reps []matchedEdit, offset int) string {
	result := content
	for i := len(reps) - 1; i >= 0; i-- {
		local := reps[i].matchIndex - offset
		result = result[:local] + reps[i].newText + result[local+reps[i].matchLength:]
	}
	return result
}

// applyPreservingUnchangedLines edits the normalized text but copies the lines
// it did not touch from the original, so only the edited lines lose their
// trailing blanks or typographic quotes.
func applyPreservingUnchangedLines(original, base string, reps []matchedEdit) string {
	originalLines := splitLinesWithEndings(original)
	baseLines := getLineSpans(base)

	type group struct {
		startLine int
		endLine   int // exclusive
		reps      []matchedEdit
	}
	var groups []group

	for _, rep := range reps {
		startLine, endLine, ok := replacementLineRange(baseLines, rep.matchIndex, rep.matchLength)
		if !ok {
			continue // only reachable for an empty file, where no edit can match
		}
		if n := len(groups); n > 0 && startLine < groups[n-1].endLine {
			if endLine > groups[n-1].endLine {
				groups[n-1].endLine = endLine
			}
			groups[n-1].reps = append(groups[n-1].reps, rep)
		} else {
			groups = append(groups, group{startLine: startLine, endLine: endLine, reps: []matchedEdit{rep}})
		}
	}

	lineIndex := 0
	var sb strings.Builder
	for _, g := range groups {
		for i := lineIndex; i < g.startLine; i++ {
			sb.WriteString(originalLines[i])
		}
		start := baseLines[g.startLine].start
		end := baseLines[g.endLine-1].end
		sb.WriteString(applyReplacements(base[start:end], g.reps, start))
		lineIndex = g.endLine
	}
	for i := lineIndex; i < len(originalLines); i++ {
		sb.WriteString(originalLines[i])
	}
	return sb.String()
}

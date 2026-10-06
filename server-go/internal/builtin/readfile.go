package builtin

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"strings"

	"llama-webui/server/internal/contracts"
)

func readFile(_ context.Context, s *Set, req contracts.ToolRequest, _ contracts.Sink) contracts.Result {
	params := req.Params
	path, bad := reqString(params, "path")
	if bad != "" {
		return fail(bad)
	}
	startLine := optInt(params, "start_line", 1)
	endLine := optInt(params, "end_line", -1)
	appendLoc := optBool(params, "append_loc", false)
	asBase64 := req.RespType == "base64"

	full := resolvePath(s.dir(req), path)

	info, err := os.Stat(full)
	if err != nil {
		return fail("cannot stat file: " + path)
	}

	if asBase64 {
		if info.Size() > readFileMaxSizeB64 {
			return fail(fmt.Sprintf("file too large (%d bytes, max %d)", info.Size(), readFileMaxSizeB64))
		}
		content, err := os.ReadFile(full)
		if err != nil {
			return fail("failed to open file: " + path)
		}
		return contracts.Result{Body: map[string]any{
			"base64":     base64.StdEncoding.EncodeToString(content),
			"size_bytes": len(content),
		}}
	}

	if info.Size() > readFileMaxSize && endLine == -1 {
		return fail(fmt.Sprintf("file too large (%d bytes, max %d). Use start_line/end_line to read a portion.",
			info.Size(), readFileMaxSize))
	}

	content, err := os.ReadFile(full)
	if err != nil {
		return fail("failed to open file: " + path)
	}

	var sb strings.Builder
	for i, line := range splitLines(string(content)) {
		lineno := i + 1
		if lineno < startLine {
			continue
		}
		if endLine != -1 && lineno > endLine {
			break
		}
		out := line + "\n"
		if appendLoc {
			out = fmt.Sprintf("%d\u2192%s\n", lineno, line)
		}
		if sb.Len()+len(out) > readFileMaxSize {
			sb.WriteString(truncatedMarker)
			break
		}
		sb.WriteString(out)
	}

	return contracts.Result{PlainText: sb.String()}
}

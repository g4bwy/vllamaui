package builtin

import (
	"context"
	"os"
	"path/filepath"
	"strings"

	"llama-webui/server/internal/contracts"
)

func writeFile(_ context.Context, s *Set, req contracts.ToolRequest, _ contracts.Sink) contracts.Result {
	params := req.Params
	path, bad := reqString(params, "path")
	if bad != "" {
		return fail(bad)
	}
	content, bad := reqString(params, "content")
	if bad != "" {
		return fail(bad)
	}

	full := resolvePath(s.dir(req), path)
	if err := writeThrough(full, content); err != nil {
		return fail("failed to write file: " + path)
	}

	return contracts.Result{Body: map[string]any{
		"result": "file written successfully",
		"path":   path,
		"bytes":  len(content),
	}}
}

// writeThrough creates the parent directories and replaces the file content.
// 0666 and 0777 before the umask are the modes ofstream and
// create_directories would pick.
func writeThrough(full, content string) error {
	if dir := filepath.Dir(full); dir != "" {
		if err := os.MkdirAll(dir, 0o777); err != nil {
			return err
		}
	}
	return os.WriteFile(full, []byte(content), 0o666)
}

func getInfo(ctx context.Context, s *Set, req contracts.ToolRequest, _ contracts.Sink) contracts.Result {
	res := runProc(ctx, s.dir(req), []string{"uname", "-a"}, getInfoMaxOutput, getInfoTimeout, nil)

	osInfo := "unknown"
	if res.exitCode == 0 && !res.timedOut {
		osInfo = strings.TrimSpace(res.output)
	}

	return contracts.Result{Body: map[string]any{
		"os":  osInfo,
		"cwd": s.dir(req),
	}}
}

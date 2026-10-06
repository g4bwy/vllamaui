// Package builtin implements the server-side tools that llama-server answers
// on GET /tools and POST /tools. The behaviour, the tool descriptions and the
// error wording follow tools/server/server-tools.cpp.
package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"llama-webui/server/internal/contracts"
)

// Limits copied from server-tools.cpp.
const (
	readFileMaxSize    = 16 * 1024
	readFileMaxSizeB64 = 32 * 1024 * 1024
	searchMaxResults   = 100
	grepMaxResults     = 100
	execMaxOutputSize  = 16 * 1024
	execMaxTimeout     = 60
	execDefaultTimeout = 10
	getInfoMaxOutput   = 4096
	getInfoTimeout     = 5
	listTimeoutSecs    = 15

	truncatedMarker = "[output truncated]"
)

// allToolNames lists the tools in the order build_tools() creates them. That
// order is what GET /tools returns and what the "available tools" message
// prints.
func allToolNames() []string {
	return []string{
		"read_file",
		"file_glob_search",
		"grep_search",
		"exec_shell_command",
		"write_file",
		"edit_file",
		"get_info",
	}
}

// meta is the static part of one tool, as server_tool::to_json builds it.
type meta struct {
	name        string
	displayName string
	write       bool
	definition  map[string]any
}

// Set holds the enabled tools. It implements contracts.Registry.
type Set struct {
	tools      []contracts.Tool
	byName     map[string]contracts.Tool
	defaultCwd string
}

// dir is the base directory of one call: the x-tool-cwd header, else the
// default given to New, else the process working directory.
func (s *Set) dir(req contracts.ToolRequest) string {
	cwd := req.Cwd
	if cwd == "" {
		cwd = s.defaultCwd
	}
	return absDir(cwd)
}

var _ contracts.Registry = (*Set)(nil)

// New builds the registry for a parsed --tools list. "all" enables every tool.
// An empty spec yields an empty set: llama-server then answers 403 on /tools,
// which is the HTTP layer's decision to make.
//
// defaultCwd is the base directory for a relative path when a request carries
// no x-tool-cwd header. When it is empty too, the process working directory is
// used, like llama-server.
func New(spec []string, defaultCwd string) (*Set, error) {
	s := &Set{defaultCwd: defaultCwd, byName: map[string]contracts.Tool{}}

	wanted := make(map[string]bool, len(spec))
	for _, name := range spec {
		if name != "all" && !known(name) {
			return nil, fmt.Errorf("unknown tool %q. available tools: %s",
				name, strings.Join(allToolNames(), ", "))
		}
		wanted[name] = true
	}

	for _, m := range metas() {
		if !wanted["all"] && !wanted[m.name] {
			continue
		}
		fn := dispatch(m.name)
		if fn == nil {
			return nil, fmt.Errorf("tool %q has no implementation", m.name)
		}
		t := &tool{set: s, info: infoOf(m), fn: fn}
		s.tools = append(s.tools, t)
		s.byName[m.name] = t
	}
	return s, nil
}

// SupportsStream reports whether a tool can push output while it runs. Only
// exec_shell_command can, and llama-server rejects a stream request for any
// other tool with: tool "x" does not support stream = true.
func (s *Set) SupportsStream(name string) bool {
	return name == "exec_shell_command"
}

// List returns the tool descriptions in build_tools() order.
func (s *Set) List() []contracts.ToolInfo {
	out := make([]contracts.ToolInfo, 0, len(s.tools))
	for _, t := range s.tools {
		out = append(out, t.Info())
	}
	return out
}

// Get looks a tool up by the name the model sees.
func (s *Set) Get(name string) (contracts.Tool, bool) {
	t, ok := s.byName[name]
	return t, ok
}

func known(name string) bool {
	for _, n := range allToolNames() {
		if n == name {
			return true
		}
	}
	return false
}

// tool adapts one invoke function to the contracts.Tool interface.
type tool struct {
	set  *Set
	info contracts.ToolInfo
	fn   func(context.Context, *Set, contracts.ToolRequest, contracts.Sink) contracts.Result
}

func (t *tool) Info() contracts.ToolInfo { return t.info }

func (t *tool) Invoke(ctx context.Context, req contracts.ToolRequest, out contracts.Sink) contracts.Result {
	if out == nil {
		out = func(string) {}
	}
	return t.fn(ctx, t.set, req, out)
}

func dispatch(name string) func(context.Context, *Set, contracts.ToolRequest, contracts.Sink) contracts.Result {
	switch name {
	case "read_file":
		return readFile
	case "file_glob_search":
		return fileGlobSearch
	case "grep_search":
		return grepSearch
	case "exec_shell_command":
		return execShellCommand
	case "write_file":
		return writeFile
	case "edit_file":
		return editFile
	case "get_info":
		return getInfo
	}
	return nil
}

func infoOf(m meta) contracts.ToolInfo {
	var info contracts.ToolInfo
	info.DisplayName = m.displayName
	info.Tool = m.name
	info.Type = "server"
	info.Permissions.Write = m.write
	info.UsesCwd = true
	info.Definition = marshalJSON(m.definition)
	return info
}

// metas returns every tool with its OpenAI function definition. Names, types,
// descriptions and required arrays are copied from build_tools().
func metas() []meta {
	return []meta{
		{
			name:        "read_file",
			displayName: "Read file",
			definition: funcDef("read_file",
				"Read the contents of a file. Optionally specify a 1-based line range. "+
					"If append_loc is true, each line is prefixed with its line number (e.g. \"1\u2192...\").",
				map[string]any{
					"path":       param("string", "Path to the file"),
					"start_line": param("integer", "First line to read, 1-based (default: 1)"),
					"end_line":   param("integer", "Last line to read, 1-based inclusive (default: end of file)"),
					"append_loc": param("boolean", "Prefix each line with its line number"),
				},
				"path"),
		},
		{
			name:        "file_glob_search",
			displayName: "File search",
			definition: funcDef("file_glob_search",
				"Recursively search for files matching a glob pattern under a directory. "+
					"Automatically skips files ignored by .gitignore (when the directory is inside a git repo) "+
					"and common junk directories (.git, node_modules, build, dist, etc.) otherwise. "+
					"A pattern with no '/' (e.g. \"*.cpp\") matches the file's basename at any depth. "+
					"A pattern containing '/' matches the full relative path; unless already anchored with "+
					"\"**/\" or a leading '/', it is automatically prefixed with \"**/\". "+
					"Use type=\"dir\" or \"all\" to also list directories; directory entries are suffixed with '/' in the output. "+
					"Note: directory listings do not apply .gitignore filtering.",
				map[string]any{
					"path":      param("string", "Base directory to search in"),
					"include":   param("string", "Glob pattern for files to include (e.g. \"*.cpp\" or \"src/**/*.cpp\"). Default: **"),
					"exclude":   param("string", "Glob pattern for files to exclude"),
					"type":      param("string", "Entry type to return: \"file\" (default), \"dir\" or \"all\""),
					"max_depth": param("integer", "Maximum depth to descend into subdirectories (default: 0 = unlimited; 1 = direct children only)"),
					"limit":     param("integer", fmt.Sprintf("Maximum number of results to return, capped at %d (default %d)", searchMaxResults, searchMaxResults)),
				},
				"path"),
		},
		{
			name:        "grep_search",
			displayName: "Grep search",
			definition: funcDef("grep_search",
				"Search for a pattern in files under a path. Returns matching lines with file paths "+
					"(and, unless searching a single file, paths relative to the given directory). "+
					"Automatically skips files ignored by .gitignore (when the directory is inside a git repo) "+
					"and common junk directories (.git, node_modules, build, dist, etc.) otherwise. "+
					"include/exclude: a pattern with no '/' matches the basename at any depth; a pattern "+
					"containing '/' matches the full relative path (auto-anchored with \"**/\" unless already anchored).",
				map[string]any{
					"path":                param("string", "File or directory to search in"),
					"pattern":             param("string", "Pattern to search for (regular expression unless literal is true)"),
					"include":             param("string", "Glob pattern to filter files (default: **)"),
					"exclude":             param("string", "Glob pattern to exclude files"),
					"return_line_numbers": param("boolean", "If true, include line numbers in results"),
					"literal":             param("boolean", "Treat pattern as a literal string instead of a regular expression (default: false)"),
					"ignore_case":         param("boolean", "Case-insensitive search (default: false)"),
					"context_lines":       param("integer", "Number of lines of context to show before and after each match (default: 0)"),
				},
				"path", "pattern"),
		},
		{
			name:        "exec_shell_command",
			displayName: "Execute shell command",
			write:       true,
			definition: funcDef("exec_shell_command",
				"Execute a shell command and return its output (stdout and stderr combined).",
				map[string]any{
					"command":         param("string", "Shell command to execute"),
					"timeout":         param("integer", fmt.Sprintf("Timeout in seconds (default %d, max %d)", execDefaultTimeout, execMaxTimeout)),
					"max_output_size": param("integer", fmt.Sprintf("Maximum output size in bytes (default %d)", execMaxOutputSize)),
				},
				"command"),
		},
		{
			name:        "write_file",
			displayName: "Write file",
			write:       true,
			definition: funcDef("write_file",
				"Write content to a file, creating it (including parent directories) if it does not exist. May use with edit_file for more complex edits.",
				map[string]any{
					"path":    param("string", "Path of the file to write"),
					"content": param("string", "Content to write"),
				},
				"path", "content"),
		},
		{
			name:        "edit_file",
			displayName: "Edit file",
			write:       true,
			definition: funcDef("edit_file",
				"Edit a file using exact text replacement. Each edits[].old_text must be unique in the file "+
					"and is matched against the original content, not incrementally. Merge nearby changes into "+
					"one edit instead of overlapping edits. Use write_file to replace the whole file.",
				map[string]any{
					"path": param("string", "Path to the file to edit"),
					"edits": map[string]any{
						"type":        "array",
						"description": "One or more exact text replacements to apply",
						"items": map[string]any{
							"type": "object",
							"properties": map[string]any{
								"old_text": param("string", "Exact text to find; must be unique in the file and must not overlap with other edits"),
								"new_text": param("string", "Text to replace old_text with"),
							},
							"required": []string{"old_text", "new_text"},
						},
					},
				},
				"path", "edits"),
		},
		{
			name:        "get_info",
			displayName: "Get Runtime Info",
			definition: map[string]any{
				"type": "function",
				"function": map[string]any{
					"name":        "get_info",
					"description": "Returns runtime info: the OS name/version and the current working directory",
					"parameters": map[string]any{
						"type":       "object",
						"properties": map[string]any{},
					},
				},
			},
		},
	}
}

func param(kind, desc string) map[string]any {
	return map[string]any{"type": kind, "description": desc}
}

// funcDef wraps one tool as an OpenAI function object. A tool with no
// parameters keeps an empty required array, as get_info does in the C++.
func funcDef(name, desc string, props map[string]any, required ...string) map[string]any {
	return map[string]any{
		"type": "function",
		"function": map[string]any{
			"name":        name,
			"description": desc,
			"parameters": map[string]any{
				"type":       "object",
				"properties": props,
				"required":   required,
			},
		},
	}
}

// marshalJSON renders a value without the HTML escaping that encoding/json
// adds by default, so the bytes match what nlohmann writes.
func marshalJSON(v any) json.RawMessage {
	var sb strings.Builder
	enc := json.NewEncoder(&sb)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return json.RawMessage(`{"error":"serialization failed"}`)
	}
	return json.RawMessage(strings.TrimSuffix(sb.String(), "\n"))
}

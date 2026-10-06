// Package mcpx is the MCP client of the webui server. It mirrors the C++ client
// in llama.cpp, tools/server/server-mcp.cpp, on top of the official Go MCP SDK.
//
// The config format is the Cursor compatible "mcpServers" object. The tool
// names, the definition object and the result wording follow llama-server, so
// the webui reads an MCP tool and a built-in tool the same way.
package mcpx

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sort"
)

// DefaultTimeoutMS is the per-call timeout for a server that does not set
// timeout_ms. It matches server_mcp_server_config in server-mcp.h.
const DefaultTimeoutMS = 30000

// ServerConfig is one entry of the "mcpServers" object.
type ServerConfig struct {
	Name    string // the config key, for example "filesystem"
	Command string // program to spawn for the stdio transport
	Args    []string
	Env     map[string]string // overrides on top of the parent environment
	Cwd     string
	// TimeoutMS bounds one tools/call, and the (re)spawn that a call needs.
	TimeoutMS int
	// InheritEnv is not part of the C++ client. A stdio child normally starts
	// from an allowlist of the parent's variables, so a third-party package
	// cannot read the secrets the web server runs with. Setting it to true asks
	// for the whole parent environment instead.
	InheritEnv bool
	// URL is not part of the C++ client. When it is set the entry talks to a
	// streamable HTTP endpoint and Command may stay empty.
	URL string
}

// Transport names the transport this entry uses: "stdio" or "http".
func (s ServerConfig) Transport() string {
	if s.URL != "" {
		return "http"
	}
	return "stdio"
}

// Config holds the servers from both config sources.
type Config struct {
	Servers []ServerConfig
}

// Empty is true when no server was configured. It matches server_mcp::empty.
func (c Config) Empty() bool { return len(c.Servers) == 0 }

// Load reads server definitions from a config file and from an inline JSON
// string, in that order. Either may be empty. A name that both sources define
// is taken from the file, and the inline copy is dropped. This is
// server_mcp::start.
//
// A missing "mcpServers" key is not an error. An unreadable file and broken
// JSON are, and the caller must treat them as fatal.
func Load(file, inline string, log *slog.Logger) (Config, error) {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	var out Config
	seen := make(map[string]bool)

	add := func(data string) error {
		servers, err := parseCursorJSON([]byte(data), log)
		if err != nil {
			return fmt.Errorf("%s%w", prefixParseConfig, err)
		}
		if len(servers) == 0 {
			log.Warn(msgNoServers)
			return nil
		}
		for _, s := range servers {
			if seen[s.Name] {
				log.Warn(fmt.Sprintf(msgDuplicate, s.Name))
				continue
			}
			seen[s.Name] = true
			out.Servers = append(out.Servers, s)
		}
		return nil
	}

	if file != "" {
		data, err := os.ReadFile(file)
		if err != nil {
			return Config{}, fmt.Errorf("%s%s: %w", prefixOpenConfig, file, err)
		}
		if err := add(string(data)); err != nil {
			return Config{}, err
		}
	}
	if inline != "" {
		if err := add(inline); err != nil {
			return Config{}, err
		}
	}
	return out, nil
}

// serverJSON is the wire shape of one entry. Wrong types are fatal, like the
// nlohmann get<T>() calls in parse_cursor_format.
type serverJSON struct {
	Command   string          `json:"command"`
	Args      json.RawMessage `json:"args"`
	Env       json.RawMessage `json:"env"`
	Cwd       string          `json:"cwd"`
	TimeoutMS *float64        `json:"timeout_ms"`
	URL       string          `json:"url"`
	// InheritEnv asks for the parent environment for a stdio child. Absent is
	// false, which is the safe default.
	InheritEnv bool `json:"inherit_env"`
}

func parseCursorJSON(data []byte, log *slog.Logger) ([]ServerConfig, error) {
	if !json.Valid(data) {
		return nil, errors.New("not valid JSON")
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(data, &top); err != nil {
		// Valid JSON that is not an object has no "mcpServers" key.
		return nil, nil
	}
	group, ok := top["mcpServers"]
	if !ok {
		return nil, nil
	}
	var entries map[string]json.RawMessage
	if err := json.Unmarshal(group, &entries); err != nil {
		return nil, nil // "mcpServers" is not an object
	}

	names := make([]string, 0, len(entries))
	for name := range entries {
		names = append(names, name)
	}
	sort.Strings(names) // the C++ walks a std::map, so order is by name

	var out []ServerConfig
	for _, name := range names {
		var e serverJSON
		if err := json.Unmarshal(entries[name], &e); err != nil {
			return nil, fmt.Errorf("server %q: %w", name, err)
		}
		s := ServerConfig{
			Name:       name,
			Command:    e.Command,
			Cwd:        e.Cwd,
			TimeoutMS:  DefaultTimeoutMS,
			URL:        e.URL,
			InheritEnv: e.InheritEnv,
		}
		if e.TimeoutMS != nil {
			s.TimeoutMS = int(*e.TimeoutMS)
		}
		args, err := stringList(e.Args)
		if err != nil {
			return nil, fmt.Errorf("server %q: args: %w", name, err)
		}
		s.Args = args
		env, err := stringMap(e.Env)
		if err != nil {
			return nil, fmt.Errorf("server %q: env: %w", name, err)
		}
		s.Env = env

		if s.Command == "" && s.URL == "" {
			log.Warn(fmt.Sprintf(msgNoCommand, name))
			continue
		}
		out = append(out, s)
	}
	return out, nil
}

// stringList reads "args". A value that is not an array is ignored, which is
// what parse_cursor_format does. A non-string element is fatal.
func stringList(raw json.RawMessage) ([]string, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, nil
	}
	out := make([]string, 0, len(items))
	for _, item := range items {
		var s string
		if err := json.Unmarshal(item, &s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, nil
}

// stringMap reads "env", with the same lax shape rules as stringList.
func stringMap(raw json.RawMessage) (map[string]string, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var items map[string]json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, nil
	}
	out := make(map[string]string, len(items))
	for k, v := range items {
		var s string
		if err := json.Unmarshal(v, &s); err != nil {
			return nil, err
		}
		out[k] = s
	}
	return out, nil
}

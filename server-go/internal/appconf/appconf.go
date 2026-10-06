// Package appconf reads the adapter configuration. A .env file in the project
// root carries things a deployment knows and the repo must not: the backend
// host, the api key. Real environment variables win over the file, and command
// line flags win over both.
package appconf

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// BackendNames are the engines the adapter can drive. Unknown names stop it.
var BackendNames = []string{"vllm", "strata"}

// Config is the resolved settings, after .env, environment and flags.
type Config struct {
	// Backend is auto, vllm or strata. auto asks the upstream what it is.
	Backend  string
	Upstream string // base URL, never with a trailing slash
	APIKey   string // sent as Authorization: Bearer
	Model    string // id to send when the UI leaves it out
	NCtx     int    // 0 means ask the backend
	// Vision is "auto", "1" or "0": ask the engine, force on, force off.
	Vision          string
	EngineTimings   bool
	KeepUnsupported bool
	ProbeVision     bool
	ProbeThinking   bool
	Host            string // listen address; loopback by default, see load
	Port            int
	Dist            string

	// Tools is the --tools list: names from builtin.Spec, or "all". Empty means
	// no server tools, and then /tools answers 403 like llama-server does.
	Tools []string
	// ToolRuntime is accepted but unsupported here, kept so a configured
	// operator learns why rather than getting host execution by surprise.
	ToolRuntime string
	// MCPServersConfig and MCPServersJSON are the two llama-server sources,
	// usable together: a Cursor-compatible file and an inline object.
	MCPServersConfig string
	MCPServersJSON   string
	// UIMCPPROXY serves /cors-proxy so the browser can reach remote MCP servers.
	UIMCPPROXY bool
	// CORSOrigins limits who may call /cors-proxy. Empty means the UI's own
	// localhost origin, which is the only page that needs it.
	CORSOrigins []string
	// InboundKey guards /tools and /cors-proxy. Distinct from APIKey, which is
	// what this server sends upstream.
	InboundKey string
	// AllowUnauthenticatedTools starts the server anyway when a tool that can
	// change this machine is enabled without an InboundKey. The default is to
	// stop, because such a tool answers any process that can reach the port.
	AllowUnauthenticatedTools bool
	// UsingDotEnv says a .env was read, which the boot log reports.
	UsingDotEnv bool
}

// VisionForced reports whether VLLM_MODALITY_VISION decided for us, and what
// it decided.
func (c *Config) VisionForced() (bool, bool) {
	switch c.Vision {
	case "1":
		return true, true
	case "0":
		return true, false
	}
	return false, false
}

// UpstreamHost is the host part of the backend URL, used to keep it out of the
// responses a browser can read.
func (c *Config) UpstreamHost() string {
	u := c.Upstream
	if i := strings.Index(u, "://"); i >= 0 {
		u = u[i+3:]
	}
	if i := strings.IndexAny(u, "/?#"); i >= 0 {
		u = u[:i]
	}
	return u
}

var envLine = regexp.MustCompile(`^\s*([A-Z0-9_]+)\s*=\s*(.*?)\s*$`)

// LoadDotEnv applies KEY=value lines from file to the environment. An existing
// variable is left alone. It reports whether the file was there.
func LoadDotEnv(file string) bool {
	text, err := os.ReadFile(file)
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(text), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		m := envLine.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		if _, ok := os.LookupEnv(m[1]); ok {
			continue
		}
		os.Setenv(m[1], unquote(m[2]))
	}
	return true
}

func unquote(v string) string {
	if len(v) >= 1 && (v[0] == '"' || v[0] == '\'') {
		v = v[1:]
	}
	if len(v) >= 1 && (v[len(v)-1] == '"' || v[len(v)-1] == '\'') {
		v = v[:len(v)-1]
	}
	return v
}

// BinaryDir is where the running executable lives. The Node version anchors its
// default paths on the adapter file, so this plays the same part.
func BinaryDir() string {
	exe, err := os.Executable()
	if err != nil {
		wd, err := os.Getwd()
		if err != nil {
			return "."
		}
		return wd
	}
	return filepath.Dir(exe)
}

// RootDir finds the project root. The rule is the Node one: the root is the
// parent of the directory the code sits in, which holds dist and .env. A binary
// may sit deeper, and `go run` builds into a temp dir, so a few parents and the
// working directory are tried before falling back to the plain rule.
func RootDir(anchor string) string {
	var tries []string
	dir := anchor
	for i := 0; i < 4; i++ {
		dir = filepath.Dir(dir)
		tries = append(tries, dir)
	}
	if wd, err := os.Getwd(); err == nil {
		tries = append(tries, wd, filepath.Dir(wd))
	}
	for _, c := range tries {
		if c == "" {
			continue
		}
		if isPath(filepath.Join(c, "dist")) || isPath(filepath.Join(c, ".env")) {
			return c
		}
	}
	return filepath.Dir(anchor)
}

func isPath(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func firstOf(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func intOf(v string, def int) int {
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil {
		return def
	}
	return n
}

// Load resolves the configuration from os.Args[1:], the environment and .env.
func Load(args []string) (*Config, error) {
	return load(args, BinaryDir())
}

func load(args []string, binDir string) (*Config, error) {
	root := RootDir(binDir)
	envFile := filepath.Join(root, ".env")
	cfg := &Config{}
	cfg.UsingDotEnv = LoadDotEnv(envFile)

	env := os.Getenv
	cfg.Backend = strings.ToLower(firstOf(env("BACKEND"), "auto"))
	cfg.Upstream = strings.TrimRight(firstOf(env("UPSTREAM_URL"), env("VLLM_UPSTREAM"), "http://localhost:8000"), "/")
	cfg.APIKey = firstOf(env("UPSTREAM_API_KEY"), env("VLLM_API_KEY"), env("OPENAI_API_KEY"))
	cfg.Model = env("VLLM_MODEL")
	cfg.NCtx = intOf(env("VLLM_N_CTX"), 0)
	cfg.Vision = "auto"
	switch env("VLLM_MODALITY_VISION") {
	case "1":
		cfg.Vision = "1"
	case "0":
		cfg.Vision = "0"
	}
	cfg.EngineTimings = env("VLLM_ENGINE_TIMINGS") != "0"
	cfg.KeepUnsupported = env("VLLM_KEEP_UNSUPPORTED") == "1"
	cfg.ProbeVision = env("VLLM_PROBE_VISION") != "0"
	cfg.ProbeThinking = env("VLLM_PROBE_THINKING") != "0"
	// Loopback, not 0.0.0.0: the port serves the UI and, with --tools, the shell
	// and file tools. A LAN broadcast has to be something the operator asks for.
	cfg.Host = firstOf(env("HOST"), "127.0.0.1")
	cfg.Port = intOf(env("PORT"), 8080)
	cfg.Dist = firstOf(env("UI_DIST"), filepath.Join(root, "dist"))
	cfg.Tools = parseToolList(firstOf(env("TOOLS"), env("LLAMA_TOOLS")))
	cfg.ToolRuntime = env("TOOLS_RUNTIME")
	cfg.MCPServersConfig = firstOf(env("MCP_SERVERS_CONFIG"), env("LLAMA_MCP_SERVERS_CONFIG"))
	cfg.MCPServersJSON = firstOf(env("MCP_SERVERS_JSON"), env("LLAMA_MCP_SERVERS_JSON"))
	cfg.UIMCPPROXY = oneOf(env("UI_MCP_PROXY"), env("WEBUI_MCP_PROXY"))
	cfg.InboundKey = firstOf(env("API_KEY"), env("WEBUI_API_KEY"))
	cfg.AllowUnauthenticatedTools = oneOf(env("ALLOW_UNAUTHENTICATED_TOOLS"))
	cfg.CORSOrigins = splitList(firstOf(env("CORS_ORIGINS"), env("LLAMA_CORS_ORIGINS")))

	fs := flag.NewFlagSet("webui", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	var vision, keep, engine string
	fs.StringVar(&cfg.Backend, "backend", cfg.Backend, "backend: auto, vllm or strata")
	fs.StringVar(&cfg.Upstream, "upstream", cfg.Upstream, "backend base URL")
	fs.StringVar(&cfg.APIKey, "api-key", cfg.APIKey, "bearer token for the backend")
	fs.StringVar(&cfg.Model, "model", cfg.Model, "model id to send when the UI omits it")
	fs.IntVar(&cfg.NCtx, "n-ctx", cfg.NCtx, "context size to report, 0 asks the backend")
	fs.StringVar(&vision, "vision", cfg.Vision, "vision: auto, 1 or 0")
	fs.StringVar(&engine, "engine-timings", boolToFlag(cfg.EngineTimings), "diff engine metrics around a request")
	fs.StringVar(&keep, "keep-unsupported", boolToFlag(cfg.KeepUnsupported), "send params the backend rejects")
	fs.BoolVar(&cfg.ProbeThinking, "probe-thinking", cfg.ProbeThinking, "ask at startup whether the model thinks")
	fs.BoolVar(&cfg.ProbeVision, "probe-vision", cfg.ProbeVision, "ask at startup whether the model sees")
	fs.StringVar(&cfg.Host, "host", cfg.Host, "listen address (default 127.0.0.1, loopback only, so the port is not reachable from the LAN)")
	fs.IntVar(&cfg.Port, "port", cfg.Port, "listen port")
	fs.StringVar(&cfg.Dist, "dist", cfg.Dist, "built UI to serve")
	tools := fs.String("tools", strings.Join(cfg.Tools, ","), "server tools: comma list, or all")
	fs.StringVar(&cfg.ToolRuntime, "tools-runtime", cfg.ToolRuntime, "tool runtime (accepted, not supported)")
	fs.StringVar(&cfg.MCPServersConfig, "mcp-servers-config", cfg.MCPServersConfig, "path to an mcpServers JSON file")
	fs.StringVar(&cfg.MCPServersJSON, "mcp-servers-json", cfg.MCPServersJSON, "inline mcpServers JSON")
	fs.BoolVar(&cfg.UIMCPPROXY, "ui-mcp-proxy", cfg.UIMCPPROXY, "serve /cors-proxy for browser MCP servers")
	fs.StringVar(&cfg.InboundKey, "require-api-key", cfg.InboundKey, "key callers must present for /tools and /cors-proxy")
	fs.BoolVar(&cfg.AllowUnauthenticatedTools, "allow-unauthenticated-tools", cfg.AllowUnauthenticatedTools, "start even when a write or exec tool has no --require-api-key")
	cors := fs.String("cors-origins", strings.Join(cfg.CORSOrigins, ","), "origins allowed on /cors-proxy")
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	cfg.Vision = vision
	cfg.Tools = parseToolList(*tools)
	cfg.CORSOrigins = splitList(*cors)
	cfg.EngineTimings = flagToBool(engine, true)
	cfg.KeepUnsupported = flagToBool(keep, false)
	cfg.Upstream = strings.TrimRight(cfg.Upstream, "/")
	cfg.Backend = strings.ToLower(cfg.Backend)

	if !known(cfg.Backend) {
		return nil, fmt.Errorf("BACKEND must be auto, %s. Got %q.", strings.Join(BackendNames, ", "), cfg.Backend)
	}
	return cfg, nil
}

func known(name string) bool {
	if name == "auto" {
		return true
	}
	for _, b := range BackendNames {
		if b == name {
			return true
		}
	}
	return false
}

func boolToFlag(v bool) string {
	if v {
		return "1"
	}
	return "0"
}

func flagToBool(v string, def bool) bool {
	switch strings.ToLower(v) {
	case "1", "true", "yes":
		return true
	case "0", "false", "no":
		return false
	}
	return def
}

// parseToolList reads a --tools value: comma separated, "all" expanded by the
// caller, blanks and duplicates dropped the way the C++ csv parser effectively
// does for a hand-typed list.
func parseToolList(v string) []string {
	if strings.TrimSpace(v) == "" {
		return nil
	}
	var out []string
	seen := map[string]bool{}
	for _, item := range strings.Split(v, ",") {
		item = strings.TrimSpace(item)
		if item == "" || seen[item] {
			continue
		}
		seen[item] = true
		out = append(out, item)
	}
	return out
}

// oneOf reads a value used as a switch.
func oneOf(values ...string) bool {
	for _, v := range values {
		if v == "1" || strings.EqualFold(v, "true") {
			return true
		}
	}
	return false
}

// splitList is parseToolList under its real name for other comma values.
func splitList(v string) []string { return parseToolList(v) }

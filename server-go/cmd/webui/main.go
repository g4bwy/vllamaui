// Command webui serves the extracted llama.cpp webui in front of an
// OpenAI-compatible inference server.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"llama-webui/server/internal/appconf"
	"llama-webui/server/internal/backend"
	"llama-webui/server/internal/builtin"
	"llama-webui/server/internal/contracts"
	"llama-webui/server/internal/core"
	"llama-webui/server/internal/mcpx"
	"llama-webui/server/internal/toolsapi"
)

func main() {
	cfg, err := appconf.Load(os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	// SIGINT and SIGTERM release the port instead of killing streams mid-write.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	log := newLog()
	d := backend.NewDeps(cfg, log)

	// Pick the backend first: everything else depends on what it can tell us.
	// BACKEND names it outright, auto asks the upstream.
	var b backend.Backend
	if cfg.Backend == "auto" {
		b = backend.AutoDetect(ctx, d)
	} else {
		b, err = backend.New(cfg.Backend, d)
		if err != nil {
			log("%s", err)
			os.Exit(1)
		}
	}
	s := core.New(b, d)

	// Tools and MCP servers, both opt-in the way llama-server has them. A
	// shell tool reachable from a browser is not something to enable by
	// accident, so an empty --tools list leaves /tools answering 403.
	tooling := buildTools(ctx, cfg, log)
	s.Attach(tooling.api, tooling.proxy)
	defer tooling.shutdown()

	// Capability probes only run for a backend that has nothing better to say,
	// and they have to be underway before the port binds.
	s.StartProbes(ctx)

	if err := s.Bind(); err != nil {
		log("cannot listen on %s:%d: %s", cfg.Host, cfg.Port, err)
		os.Exit(1)
	}
	bootLog(ctx, s, d, b, tooling.notes)

	if err := s.Serve(ctx); err != nil {
		log("server stopped: %s", err)
		os.Exit(1)
	}
}

// bootLog is the banner the operator reads to know what the adapter found.
func bootLog(ctx context.Context, s *core.Server, d *backend.Deps, b backend.Backend, notes []string) {
	cfg := d.Cfg
	models := d.Models.List(ctx, true)
	backend.SafeSnapshot(ctx, b, d, backend.SnapshotOpts{Force: true})

	d.Log("llama.cpp webui -> %s adapter", b.ID())
	d.Log("  ui        http://%s:%d/", cfg.Host, cfg.Port)
	d.Log("  backend   %s", cfg.Upstream)
	if cfg.UsingDotEnv {
		d.Log("  loaded    .env (gitignored)")
	}
	d.Log("  model     %s", modelIDs(models))
	d.Log("  timings   %s", timingsText(b, cfg))
	for _, n := range notes {
		d.Log("%s", n)
	}
	// The probe lines land after the banner, bounded so a dead engine cannot
	// hold the process there.
	s.WaitProbes(ctx, 2*time.Second)
}

func modelIDs(models map[string]any) string {
	var names []string
	for _, e := range backend.Entries(models) {
		if id := backend.Str(e, "id"); id != "" {
			names = append(names, id)
		}
	}
	if len(names) == 0 {
		return "none reported"
	}
	return strings.Join(names, ", ")
}

func timingsText(b backend.Backend, cfg *appconf.Config) string {
	if !b.WindowsTimings() {
		return "reported by the engine"
	}
	if cfg.EngineTimings {
		return "metrics window, wall-clock fallback"
	}
	return "wall clock only"
}

// newLog returns the one-line logger every part of the adapter writes through.
func newLog() backend.Log {
	return func(format string, v ...any) {
		fmt.Println(time.Now().UTC().Format("15:04:05"), fmt.Sprintf(format, v...))
	}
}

// tooling bundles what startup worked out, so main stays short.
type tooling struct {
	api      *toolsapi.API
	proxy    *toolsapi.Proxy
	shutdown func()
	// notes are the lines startup learned, printed after the banner so the log
	// reads in one block instead of arriving before the port is bound.
	notes []string
}

// buildTools makes the built-in set, then the MCP client, merges them so a
// built-in name always wins, and wraps the result in the HTTP handlers.
func buildTools(ctx context.Context, cfg *appconf.Config, log backend.Log) tooling {
	out := tooling{shutdown: func() {}}

	var regs []contracts.Registry
	set, err := builtin.New(cfg.Tools, "")
	if err != nil {
		log("%s", err)
		os.Exit(1)
	}
	if listed := set.List(); len(listed) > 0 {
		regs = append(regs, set)
		out.notes = append(out.notes, "  tools     "+names(set))
	}

	if client := buildMCP(ctx, cfg, log, set); client != nil {
		regs = append(regs, client.Registry())
		out.notes = append(out.notes, "  mcp tools "+names(client))
		out.shutdown = client.Shutdown
	}

	if len(regs) > 0 {
		api := toolsapi.New(toolsapi.Merge(regs...), toolsapi.Log(log))
		if cfg.InboundKey != "" {
			api.Keys(authorized(cfg.InboundKey))
			out.notes = append(out.notes, "  /tools    guarded by an api key")
		}
		out.api = api
	}

	if cfg.UIMCPPROXY {
		origins := cfg.CORSOrigins
		if len(origins) == 0 {
			origins = defaultOrigins(cfg)
		}
		out.proxy = toolsapi.NewProxy(origins, toolsapi.Log(log))
		out.notes = append(out.notes, "  proxy     /cors-proxy for "+strings.Join(origins, " "))
	}
	return out
}

// names joins the tool names a registry reports.
func names(reg contracts.Registry) string {
	listed := reg.List()
	out := make([]string, 0, len(listed))
	for _, t := range listed {
		out = append(out, t.Tool)
	}
	return strings.Join(out, ", ")
}

// buildMCP starts the MCP client from the two llama-server sources. A server
// that will not start is an operator mistake, so it stops startup rather than
// leaving a half-populated tool list behind.
func buildMCP(ctx context.Context, cfg *appconf.Config, log backend.Log, reserved contracts.Registry) *mcpx.Client {
	if cfg.MCPServersConfig == "" && cfg.MCPServersJSON == "" {
		return nil
	}
	mcfg, err := mcpx.Load(cfg.MCPServersConfig, cfg.MCPServersJSON, mcpLogger(log))
	if err != nil {
		log("%s", err)
		os.Exit(1)
	}
	if len(mcfg.Servers) == 0 {
		log("  mcp       configured, but no server in it")
		return nil
	}
	var taken []string
	if reserved != nil {
		for _, t := range reserved.List() {
			taken = append(taken, t.Tool)
		}
	}
	client, err := mcpx.New(ctx, mcfg, &mcpx.Options{Logger: mcpLogger(log), ReservedNames: taken})
	if err != nil {
		log("mcp: %s", err)
		os.Exit(1)
	}
	return client
}

// mcpLogger bridges the SDK logger the MCP package wants to the one-line log
// every other part writes through, so all output keeps one format: the message
// first, then any attributes as key=value.
func mcpLogger(log backend.Log) *slog.Logger {
	return slog.New(mcpHandler{log: log})
}

type mcpHandler struct {
	log   backend.Log
	attrs []slog.Attr
}

func (h mcpHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h mcpHandler) Handle(_ context.Context, r slog.Record) error {
	parts := make([]string, 0, len(h.attrs)+1)
	for _, a := range h.attrs {
		parts = append(parts, a.String())
	}
	r.Attrs(func(a slog.Attr) bool {
		parts = append(parts, a.String())
		return true
	})
	line := r.Message
	if len(parts) > 0 {
		line += " " + strings.Join(parts, " ")
	}
	h.log("%s", line)
	return nil
}

func (h mcpHandler) WithAttrs(as []slog.Attr) slog.Handler {
	h.attrs = append(append([]slog.Attr{}, h.attrs...), as...)
	return h
}

func (h mcpHandler) WithGroup(string) slog.Handler { return h }

// authorized accepts the two header forms llama-server reads.
func authorized(key string) func(*http.Request) bool {
	return func(r *http.Request) bool {
		if k := r.Header.Get("X-Api-Key"); k != "" {
			return k == key
		}
		auth := r.Header.Get("Authorization")
		return len(auth) > 7 && strings.EqualFold(auth[:7], "Bearer ") && auth[7:] == key
	}
}

// defaultOrigins allows only the machine the UI is served from. A browser needs
// no CORS entry at all for same-origin calls, so this list is about the pages an
// operator opens next to the server, not about the server itself.
func defaultOrigins(cfg *appconf.Config) []string {
	host := cfg.Host
	if host == "0.0.0.0" || host == "" {
		host = "localhost"
	}
	return []string{fmt.Sprintf("http://%s:%d", host, cfg.Port), fmt.Sprintf("http://127.0.0.1:%d", cfg.Port)}
}

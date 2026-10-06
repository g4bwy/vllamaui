// Package core is the HTTP surface: the llama.cpp routes the webui calls, the
// engine stats, and the built UI in front of them. One port answers both, so the
// browser talks same-origin only.
package core

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"llama-webui/server/internal/backend"
	"llama-webui/server/internal/chat"
	"llama-webui/server/internal/toolsapi"
)

// notFoundStream is what the UI gets for the one llama.cpp feature neither
// engine can offer.
const notFoundStream = "stream replay is a llama.cpp feature, not available on this backend"

// probeGrace bounds how long /props waits for the capability probes. A dead
// backend must not stall the first paint: after this the page loads with what we
// know, and the UI retries /props anyway.
const probeGrace = 2 * time.Second

// statsTTL is how long one engine scrape answers for, so several browser tabs
// cost one upstream read.
const statsTTL = 1500 * time.Millisecond

// Server answers the webui contract for one backend.
type Server struct {
	d  *backend.Deps
	b  backend.Backend
	pr *chat.Proxy

	// tools and proxy are attached at startup; nil means the feature is off.
	tools *toolsapi.API
	proxy *toolsapi.Proxy

	probeMu   sync.Mutex
	probeDone chan struct{}

	mu        sync.Mutex
	statsAt   time.Time
	statsSnap *backend.Snapshot
	rates     *backend.RateWindow

	httpSrv  *http.Server
	listener net.Listener
}

// New builds the server around a backend and its dependencies.
func New(b backend.Backend, d *backend.Deps) *Server {
	return &Server{d: d, b: b, pr: chat.New(b, d)}
}

// Attach wires the tool endpoints in. Either may be nil, and then that route
// answers the way llama-server does when the feature was not enabled.
func (s *Server) Attach(tools *toolsapi.API, proxy *toolsapi.Proxy) {
	s.tools, s.proxy = tools, proxy
}

// Backend exposes the engine, for the boot log.
func (s *Server) Backend() backend.Backend { return s.b }

// Handler is the router.
func (s *Server) Handler() http.Handler {
	return http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		w := &tracker{ResponseWriter: rw}
		defer func() {
			if rec := recover(); rec != nil {
				s.d.Log("handler error: %s", toText(rec))
				if !w.wrote {
					chat.WriteError(w, http.StatusInternalServerError, toText(rec))
				}
			}
		}()
		s.route(w, r)
	})
}

// Bind opens the port, so a bad address fails before anything is logged.
func (s *Server) Bind() error {
	cfg := s.d.Cfg
	addr := net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port))
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.listener = ln
	// The timeouts stop a slow or dead client from holding a connection and its
	// goroutine forever. ReadTimeout bounds reading the request only, so the
	// long-lived SSE responses are unaffected.
	s.httpSrv = &http.Server{
		Handler:           s.Handler(),
		ReadHeaderTimeout: 15 * time.Second,
		ReadTimeout:       60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	s.mu.Unlock()
	return nil
}

// Serve runs until ctx is cancelled, then releases the port.
func (s *Server) Serve(ctx context.Context) error {
	s.mu.Lock()
	srv, ln := s.httpSrv, s.listener
	s.mu.Unlock()
	if srv == nil {
		if err := s.Bind(); err != nil {
			return err
		}
		s.mu.Lock()
		srv, ln = s.httpSrv, s.listener
		s.mu.Unlock()
	}

	shutdown := make(chan struct{})
	go func() {
		defer close(shutdown)
		<-ctx.Done()
		// Give the browser a moment to finish, then let go: the port must come
		// back even when a stream is still open.
		closing, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(closing)
	}()

	err := srv.Serve(ln)
	<-shutdown
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// route dispatches on the path with any trailing slashes removed.
func (s *Server) route(w http.ResponseWriter, r *http.Request) {
	cfg := s.d.Cfg
	full := urlPath(r)
	p := strings.TrimRight(full, "/")
	if p == "" {
		p = "/"
	}

	// A wildcard here would let any page the operator visits read the answers of
	// every route, /tools included. The UI is same-origin and needs no CORS
	// headers, so the only route that hands any out is the proxy, and it decides
	// from its own allow list.
	if r.Method == http.MethodOptions {
		if p == "/cors-proxy" && s.proxy != nil {
			s.callProxy(w, r)
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}

	switch {
	case p == "/props":
		s.handleProps(w, r)
		return
	case p == "/v1/models" && r.Method == http.MethodGet:
		models := s.d.Models.List(r.Context(), true)
		if models == nil {
			chat.WriteMessage(w, http.StatusBadGateway, "cannot reach the backend at "+cfg.Upstream)
			return
		}
		chat.WriteJSON(w, http.StatusOK, models)
		return
	case p == "/v1/chat/completions" && r.Method == http.MethodPost:
		s.pr.Complete(w, r)
		return
	case p == "/v1/chat/completions/control" && r.Method == http.MethodPost:
		chat.WriteUnavailable(w, http.StatusOK, "not supported by "+s.b.ID())
		return
	case p == "/slots":
		chat.WriteJSON(w, http.StatusOK, []any{})
		return
	case p == "/tools" && r.Method == http.MethodGet:
		s.listTools(w, r)
		return
	case p == "/tools" && r.Method == http.MethodPost:
		s.callTool(w, r)
		return
	case p == "/cors-proxy":
		s.callProxy(w, r)
		return
	case p == "/v1/streams/lookup" && r.Method == http.MethodPost:
		chat.WriteJSON(w, http.StatusOK, []any{})
		return
	case p == "/v1/stream":
		if r.Method == http.MethodDelete {
			chat.WriteJSON(w, http.StatusOK, map[string]any{"success": true})
			return
		}
		chat.WriteMessage(w, http.StatusNotFound, notFoundStream)
		return
	case p == "/build.json":
		chat.WriteJSON(w, http.StatusOK, map[string]any{"version": s.buildInfo(r.Context())})
		return
	case p == "/vllm/stats" || p == "/engine/stats":
		s.handleStats(w, r)
		return
	case strings.HasPrefix(p, "/models") || p == "/completion" || p == "/tokenize":
		chat.WriteUnavailable(w, http.StatusNotImplemented, "not available on a "+s.b.ID()+" backend ("+p+")")
		return
	}

	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		chat.WriteError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	s.serveStatic(w, r, full)
}

// urlPath reads the path the way `new URL(req.url)` does in the Node router:
// ".." segments are folded away, while escapes stay encoded until the static
// handler decodes them. The two differ, and both are guarded.
func urlPath(r *http.Request) string {
	raw := r.URL.EscapedPath()
	cleaned := path.Clean("/" + raw)
	if strings.HasSuffix(raw, "/") && !strings.HasSuffix(cleaned, "/") {
		cleaned += "/"
	}
	return cleaned
}

// handleProps answers the call the UI makes first.
func (s *Server) handleProps(w http.ResponseWriter, r *http.Request) {
	s.WaitProbes(r.Context(), probeGrace)
	models := s.d.Models.List(r.Context(), false)
	if models == nil {
		// 503 is the code the UI reads as "backend not ready": it shows a spinner
		// and retries once a second, so a restarting backend heals itself.
		chat.WriteMessage(w, http.StatusServiceUnavailable, "cannot reach the backend at "+s.d.Cfg.Upstream)
		return
	}
	d, err := s.b.Describe(r.Context())
	if err != nil {
		s.d.Log("handler error: %s", err)
		chat.WriteMessage(w, http.StatusInternalServerError, err.Error())
		return
	}

	entry := d.Entry
	var modelPath any
	switch {
	case d.HasModelPath:
		modelPath = d.ModelPath
	case entry != nil && entry["id"] != nil:
		modelPath = entry["id"]
	case s.d.Cfg.Model != "":
		modelPath = s.d.Cfg.Model
	default:
		modelPath = "unknown"
	}
	eos := any("")
	if entry != nil && entry["eos_token"] != nil {
		eos = entry["eos_token"]
	}
	totalSlots := any(1.0)
	if d.HasTotalSlots {
		totalSlots = d.TotalSlots
	}
	params := d.Params
	if params == nil {
		params = map[string]any{}
	}

	chat.WriteJSON(w, http.StatusOK, map[string]any{
		"role":        "model",
		"model_path":  modelPath,
		"total_slots": totalSlots,
		// vLLM never serves its chat template, and the UI decides whether the
		// model can think by scanning that template. Strata ships a real one, so
		// its backend passes the original text and the UI reads it directly.
		"chat_template": d.ChatTemplate,
		"bos_token":     "",
		"eos_token":     eos,
		// no backend address here: this response goes to every browser that loads
		// the page, and the host is a private deployment detail.
		"build_info":         s.safeBuildInfo(d.BuildInfo),
		"cors_proxy_enabled": s.proxy != nil,
		"modalities":         d.Modalities,
		"default_generation_settings": map[string]any{
			"id":            0,
			"id_task":       0,
			"n_ctx":         d.NCtx,
			"speculative":   d.Speculative,
			"is_processing": false,
			"prompt":        "",
			"next_token": map[string]any{
				"has_next_token": false,
				"has_new_line":   false,
				"n_remain":       0,
				"n_decoded":      0,
				"stopping_word":  "",
			},
			// vLLM reports nothing usable here, so the UI sends no sampling params
			// and the server defaults rule. Strata answers this from its /props.
			"params": params,
		},
	})
}

// safeBuildInfo keeps the backend host out of a string a browser can read.
func (s *Server) safeBuildInfo(v string) string {
	host := s.d.Cfg.UpstreamHost()
	if host != "" {
		v = strings.ReplaceAll(v, host, "backend")
	}
	return v
}

// buildInfo is the one short label /build.json shows in the About dialog.
func (s *Server) buildInfo(ctx context.Context) string {
	d, err := s.b.Describe(ctx)
	info := ""
	if err == nil && d != nil {
		info = d.BuildInfo
	}
	if info == "" {
		info = s.b.ID()
	}
	return s.safeBuildInfo(backend.Flatten(info, 60))
}

// statsSnapshot reads the engine once per short window and keeps the result.
func (s *Server) statsSnapshot(ctx context.Context) *backend.Snapshot {
	s.mu.Lock()
	cached := s.statsSnap
	fresh := time.Since(s.statsAt) < statsTTL
	s.mu.Unlock()
	if cached != nil && fresh {
		return cached
	}
	// Never forced: the cache is the point here. The timing window around a chat
	// request takes its own fresh reads instead.
	snap := backend.SafeSnapshot(ctx, s.b, s.d, backend.SnapshotOpts{})
	s.mu.Lock()
	s.statsAt = time.Now()
	s.statsSnap = snap
	s.mu.Unlock()
	return snap
}

func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	snap := s.statsSnapshot(r.Context())
	if snap == nil {
		chat.WriteError(w, http.StatusBadGateway, "cannot read stats from "+s.b.ID())
		return
	}
	// The window comes from the scrape, not from this handler's clock.
	s.mu.Lock()
	rates, window := backend.RatesFrom(snap, snap.At, s.b.RateKeys(), s.rates)
	s.rates = window
	s.mu.Unlock()

	chat.WriteJSON(w, http.StatusOK, map[string]any{
		"title":      s.b.Title(),
		"sampled_at": snap.At.UTC().Format(isoMillis),
		"gauges":     chat.OmitMissing(s.b.Gauges(snap)),
		"labels":     s.b.Labels(),
		"rates":      rates,
		"counters":   chat.OmitMissing(s.b.Counters(snap)),
		"engine":     chat.OmitMissing(s.b.Engine(snap)),
	})
}

// StartProbes asks the engine about the capabilities it does not report, before
// the port binds.
func (s *Server) StartProbes(ctx context.Context) {
	s.probeMu.Lock()
	if s.probeDone != nil {
		s.probeMu.Unlock()
		return
	}
	s.probeDone = make(chan struct{})
	s.probeMu.Unlock()

	go func() {
		defer close(s.probeDone)
		defer func() {
			if rec := recover(); rec != nil {
				s.d.Log("probe crashed: %v", rec)
			}
		}()
		wants := s.b.Probes()
		prober, ok := s.b.(backend.Prober)
		if wants.Thinking && ok {
			prober.ProbeThinking(ctx)
		} else {
			s.d.Log("thinking support: from /props")
		}
		if wants.Vision && ok {
			prober.ProbeVision(ctx)
		} else {
			s.d.Log("vision support: from /props")
		}
	}()
}

// WaitProbes blocks until the capability probes are done, or until grace runs
// out.
func (s *Server) WaitProbes(ctx context.Context, grace time.Duration) {
	s.probeMu.Lock()
	done := s.probeDone
	s.probeMu.Unlock()
	if done == nil {
		return
	}
	select {
	case <-done:
	case <-time.After(grace):
	case <-ctx.Done():
	}
}

func toText(v any) string {
	if err, ok := v.(error); ok {
		return err.Error()
	}
	if s, ok := v.(string); ok {
		return s
	}
	return "handler failed"
}

// isoMillis is the shape a browser produces with Date.toISOString().
const isoMillis = "2006-01-02T15:04:05.000Z"

// tracker notes whether a response has started, so a late panic is logged
// instead of turning into a second status line.
type tracker struct {
	http.ResponseWriter
	wrote bool
}

func (t *tracker) WriteHeader(code int) {
	t.wrote = true
	t.ResponseWriter.WriteHeader(code)
}

func (t *tracker) Write(b []byte) (int, error) {
	t.wrote = true
	return t.ResponseWriter.Write(b)
}

func (t *tracker) Flush() {
	if f, ok := t.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// ReadFrom keeps the copy path io.Copy would take on the unwrapped writer.
func (t *tracker) ReadFrom(r io.Reader) (int64, error) {
	t.wrote = true
	return io.Copy(t.ResponseWriter, r)
}

func (t *tracker) Unwrap() http.ResponseWriter { return t.ResponseWriter }

// listTools answers GET /tools. With no tools enabled it says so the way
// llama-server does, which is what the webui turns into its "start the server
// with --tools" hint.
func (s *Server) listTools(w http.ResponseWriter, r *http.Request) {
	if s.tools == nil {
		chat.WriteDisabled(w)
		return
	}
	s.tools.HandleList(w, r)
}

func (s *Server) callTool(w http.ResponseWriter, r *http.Request) {
	if s.tools == nil {
		chat.WriteDisabled(w)
		return
	}
	s.tools.HandleCall(w, r)
}

// callProxy relays browser MCP traffic. The route exists only when the operator
// turned the proxy on, so a disabled one says the same thing /tools does.
func (s *Server) callProxy(w http.ResponseWriter, r *http.Request) {
	if s.proxy == nil {
		chat.WriteDisabled(w)
		return
	}
	s.proxy.Handle(w, r)
}

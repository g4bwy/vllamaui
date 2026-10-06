package core

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"llama-webui/server/internal/appconf"
	"llama-webui/server/internal/backend"
)

// fakeEngine is the inference server these tests talk to. It answers a fixed
// body per path and counts the hits, so a test can see how often the adapter
// called.
type fakeEngine struct {
	mu     sync.Mutex
	routes map[string]string
	status map[string]int
	calls  map[string]int
	chats  []map[string]any
	delay  time.Duration
	chat   func(http.ResponseWriter, *http.Request, map[string]any)
	body   map[string]any // last chat request body
}

func newEngine(routes map[string]string) *fakeEngine {
	return &fakeEngine{routes: routes, calls: map[string]int{}}
}

func (f *fakeEngine) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	f.mu.Lock()
	f.calls[path]++
	body := f.routes[path]
	status := f.status[path]
	delay := f.delay
	f.mu.Unlock()

	if path == "/v1/chat/completions" {
		var sent map[string]any
		json.NewDecoder(r.Body).Decode(&sent)
		f.mu.Lock()
		f.body = sent
		f.chats = append(f.chats, sent)
		chat := f.chat
		f.mu.Unlock()
		if chat != nil {
			if delay > 0 {
				select {
				case <-time.After(delay):
				case <-r.Context().Done():
					return
				}
			}
			chat(w, r, sent)
			return
		}
	}
	if status != 0 {
		http.Error(w, body, status)
		return
	}
	if body == "" {
		http.Error(w, "no route "+path, http.StatusNotFound)
		return
	}
	fmt.Fprint(w, body)
}

func (f *fakeEngine) count(path string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[path]
}

func (f *fakeEngine) sent() map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.body
}

// sentChat returns the nth request the engine saw on /v1/chat/completions.
func (f *fakeEngine) sentChat(n int) map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	if n >= len(f.chats) {
		return nil
	}
	return f.chats[n]
}

// harness is one server over one fake engine, with the log it wrote.
type harness struct {
	s      *Server
	d      *backend.Deps
	eng    *fakeEngine
	url    string
	host   string
	logs   func() []string
	hasLog func(string) bool
}

func newHarness(t *testing.T, id string, eng *fakeEngine, tweak func(*appconf.Config)) *harness {
	t.Helper()
	srv := httptest.NewServer(eng)
	t.Cleanup(srv.Close)

	cfg := &appconf.Config{
		Backend:       id,
		Upstream:      srv.URL,
		Vision:        "auto",
		EngineTimings: true,
		ProbeVision:   true,
		ProbeThinking: true,
		Dist:          t.TempDir(),
		Port:          0,
	}
	if tweak != nil {
		tweak(cfg)
	}
	var mu sync.Mutex
	var lines []string
	log := func(format string, v ...any) {
		mu.Lock()
		defer mu.Unlock()
		lines = append(lines, fmt.Sprintf(format, v...))
	}
	d := backend.NewDeps(cfg, log)
	b, err := backend.New(id, d)
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{s: New(b, d), d: d, eng: eng, url: srv.URL, host: cfg.UpstreamHost()}
	h.logs = func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), lines...)
	}
	h.hasLog = func(part string) bool {
		for _, l := range h.logs() {
			if strings.Contains(l, part) {
				return true
			}
		}
		return false
	}
	return h
}

func (h *harness) do(method, target string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, nil)
	rec := httptest.NewRecorder()
	h.s.Handler().ServeHTTP(rec, req)
	return rec
}

func (h *harness) get(target string) *httptest.ResponseRecorder { return h.do(http.MethodGet, target) }

func decode(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("body is not JSON: %v (%s)", err, rec.Body)
	}
	return out
}

const vllmModels = `{"object":"list","data":[{"id":"qwen-test","object":"model","max_model_len":32768,"eos_token":3}]}`

func vllmEngine() *fakeEngine {
	return newEngine(map[string]string{
		"/v1/models": vllmModels,
		"/version":   `{"version":"0.11.0"}`,
	})
}

// TestPropsUnavailable: the UI reads 503 as "not ready" and retries.
func TestPropsUnavailable(t *testing.T) {
	h := newHarness(t, "vllm", newEngine(map[string]string{}), nil)
	rec := h.get("/props")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	body := decode(t, rec)
	msg := backend.Str(backend.Obj(body, "error"), "message")
	if !strings.Contains(msg, "cannot reach the backend at") || !strings.Contains(msg, h.url) {
		t.Errorf("the message must name the unreachable URL: %v", msg)
	}
}

func TestPropsVLLM(t *testing.T) {
	h := newHarness(t, "vllm", vllmEngine(), nil)
	rec := h.get("/props")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body %s", rec.Code, rec.Body)
	}
	body := decode(t, rec)
	if got := backend.Str(body, "role"); got != "model" {
		t.Errorf("role = %q", got)
	}
	if got := backend.Str(body, "model_path"); got != "qwen-test" {
		t.Errorf("model_path = %q", got)
	}
	if got := backend.NumOr(body, "total_slots", 0); got != 1 {
		t.Errorf("total_slots = %v, an OpenAI server serves one UI slot", got)
	}
	if got := backend.Str(body, "chat_template"); got != "" {
		t.Errorf("without a thinking probe the template stays empty, got %q", got)
	}
	if got := backend.Str(body, "bos_token"); got != "" {
		t.Errorf("bos_token = %q", got)
	}
	if got := backend.NumOr(body, "eos_token", -1); got != 3 {
		t.Errorf("eos_token = %v, want the id the model list reported", got)
	}
	if got := backend.Str(body, "build_info"); got != "vllm 0.11.0" {
		t.Errorf("build_info = %q", got)
	}
	if backend.Bool(body, "cors_proxy_enabled") {
		t.Error("there is no cors proxy")
	}
	mod := backend.Obj(body, "modalities")
	if backend.Bool(mod, "vision") {
		t.Errorf("vision must be off until the engine says otherwise: %v", mod)
	}
	dgs := backend.Obj(body, "default_generation_settings")
	if got := backend.NumOr(dgs, "n_ctx", 0); got != 32768 {
		t.Errorf("n_ctx = %v, want max_model_len", got)
	}
	if params := backend.Obj(dgs, "params"); len(params) != 0 {
		t.Errorf("vLLM has no params to report, got %v", params)
	}
	next := backend.Obj(dgs, "next_token")
	for _, k := range []string{"has_next_token", "has_new_line", "stopping_word"} {
		if _, ok := next[k]; !ok {
			t.Errorf("next_token.%s missing", k)
		}
	}
	if backend.Bool(dgs, "is_processing") {
		t.Error("nothing is processing before the first request")
	}
}

// TestPropsNeverLeaksHost: /props and /build.json go to every browser, and the
// backend host is a private deployment detail.
func TestPropsNeverLeaksHost(t *testing.T) {
	h := newHarness(t, "vllm", vllmEngine(), nil)
	h.eng.set("/version", `{"version":"0.11.0 at `+h.host+`"}`)
	for _, target := range []string{"/props", "/build.json"} {
		rec := h.get(target)
		if strings.Contains(rec.Body.String(), h.host) {
			t.Errorf("%s leaked the backend host: %s", target, rec.Body)
		}
	}
	build := decode(t, h.get("/build.json"))
	if got := backend.Str(build, "version"); got == "" {
		t.Errorf("build.json must still say something: %q", got)
	}
}

// TestPropsStrata: Strata serves a real template and its own params, which come
// through with n_predict removed.
func TestPropsStrata(t *testing.T) {
	eng := newEngine(map[string]string{
		"/v1/models": `{"object":"list","data":[{"id":"strata-model","object":"model","meta":{"n_ctx":8192}}]}`,
		"/health":    `{"service":"strata","model":"strata-model","max_context":8192,"images":true}`,
		"/props": `{"default_generation_settings":{"n_ctx":16384,"params":{"n_predict":-1,"temperature":0.7,"mirostat":2}},` +
			`"total_slots":3,"model_alias":"alias-name","build_info":"strata 1.2.3",` +
			`"chat_template":"{% if enable_thinking %}x{% endif %}","modalities":{"vision":true}}`,
		"/v1/status": `{"engine":"cuda"}`,
	})
	h := newHarness(t, "strata", eng, nil)
	rec := h.get("/props")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body %s", rec.Code, rec.Body)
	}
	body := decode(t, rec)
	if got := backend.Str(body, "model_path"); got != "alias-name" {
		t.Errorf("model_path = %q, want the alias the engine reported", got)
	}
	if got := backend.NumOr(body, "total_slots", 0); got != 3 {
		t.Errorf("total_slots = %v", got)
	}
	if got := backend.Str(body, "chat_template"); !strings.Contains(got, "enable_thinking") {
		t.Errorf("Strata passes its real template through, got %q", got)
	}
	if got := backend.Str(body, "build_info"); got != "strata 1.2.3" {
		t.Errorf("build_info = %q", got)
	}
	if !backend.Bool(backend.Obj(body, "modalities"), "vision") {
		t.Error("the engine said it sees")
	}
	dgs := backend.Obj(body, "default_generation_settings")
	if got := backend.NumOr(dgs, "n_ctx", 0); got != 16384 {
		t.Errorf("n_ctx = %v, want the value from /props", got)
	}
	params := backend.Obj(dgs, "params")
	if _, ok := params["n_predict"]; ok {
		t.Error("n_predict must not reach the UI: the proxy maps it to max_tokens")
	}
	if params["mirostat"] == nil {
		t.Error("the params Strata does read must come through")
	}
	if !h.d.Flags.Reasoning.Get() {
		t.Error("a template that mentions enable_thinking proves thinking support")
	}
}

// TestStrataNeedsNoProbes: the boot log says where the answer came from.
func TestStrataNeedsNoProbes(t *testing.T) {
	eng := newEngine(map[string]string{
		"/v1/models": `{"object":"list","data":[{"id":"m","object":"model"}]}`,
		"/health":    `{"service":"strata","model":"m","images":true}`,
		"/props":     `{"total_slots":1,"chat_template":"enable_thinking","model_path":"m","build_info":"strata 1","modalities":{"vision":true},"default_generation_settings":{"n_ctx":4096,"params":{}}}`,
	})
	h := newHarness(t, "strata", eng, nil)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	h.s.StartProbes(ctx)
	h.s.WaitProbes(ctx, 5*time.Second)
	if !h.hasLog("thinking support: from /props") || !h.hasLog("vision support: from /props") {
		t.Errorf("log = %v", h.logs())
	}
	if eng.count("/v1/chat/completions") != 0 {
		t.Error("probing an engine that reports its capabilities is waste")
	}
	// Strata reports both through /props, so the flags land when the UI asks.
	body := decode(t, h.get("/props"))
	if !backend.Bool(backend.Obj(body, "modalities"), "vision") {
		t.Error("/props said it sees")
	}
	if !h.d.Flags.Vision.Get() {
		t.Error("the engine answer must set the flag the next /props reports")
	}
}

func TestModelsPassthrough(t *testing.T) {
	h := newHarness(t, "vllm", vllmEngine(), nil)
	rec := h.get("/v1/models")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	body := decode(t, rec)
	if len(backend.Entries(body)) != 1 {
		t.Errorf("body = %v", body)
	}

	down := newHarness(t, "vllm", newEngine(map[string]string{}), nil)
	rec = down.get("/v1/models")
	if rec.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502 when the engine is unreachable", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "cannot reach the backend at") {
		t.Errorf("body = %s", rec.Body)
	}
}

// TestRoutes covers the llama.cpp surface the UI polls, including the trailing
// slash the Node router ignores.
func TestRoutes(t *testing.T) {
	eng := vllmEngine()
	eng.routes["/metrics"] = "vllm:prompt_tokens_total 12\n"
	h := newHarness(t, "vllm", eng, nil)

	cases := []struct {
		method string
		target string
		code   int
		expect func(map[string]any) string
	}{
		{http.MethodGet, "/slots", 200, nil},
		{http.MethodGet, "/slots/", 200, nil},
		// Nothing was attached, so these answer like a server started without
		// --tools or --ui-mcp-proxy.
		{http.MethodGet, "/tools", 403, disabledReason},
		{http.MethodPost, "/v1/streams/lookup", 200, nil},
		{http.MethodDelete, "/v1/stream", 200, func(b map[string]any) string {
			if !backend.Bool(b, "success") {
				return "success must be true"
			}
			return ""
		}},
		{http.MethodGet, "/v1/stream", 404, func(b map[string]any) string {
			if !strings.Contains(bodyText(b), "llama.cpp feature") {
				return "the 404 must explain the missing feature: " + bodyText(b)
			}
			return ""
		}},
		{http.MethodPost, "/v1/chat/completions/control", 200, func(b map[string]any) string {
			if backend.Bool(b, "success") {
				return "control must report failure"
			}
			if got := backend.Str(b, "error"); !strings.Contains(got, "not supported by vllm") {
				return "error = " + got
			}
			return ""
		}},
		{http.MethodGet, "/models", 501, nil},
		{http.MethodPost, "/models/load", 501, nil},
		{http.MethodPost, "/models/unload", 501, nil},
		{http.MethodGet, "/completion", 501, nil},
		{http.MethodGet, "/tokenize", 501, nil},
		{http.MethodGet, "/cors-proxy", 403, disabledReason},
		{http.MethodGet, "/build.json", 200, func(b map[string]any) string {
			if got := backend.Str(b, "version"); got != "vllm 0.11.0" {
				return "version = " + got
			}
			return ""
		}},
		{http.MethodGet, "/vllm/stats", 200, nil},
		{http.MethodGet, "/engine/stats", 200, nil},
		{http.MethodPost, "/some/client/route", 405, nil},
	}
	for _, tc := range cases {
		rec := h.do(tc.method, tc.target)
		if rec.Code != tc.code {
			t.Errorf("%s %s = %d, want %d (%s)", tc.method, tc.target, rec.Code, tc.code, rec.Body)
			continue
		}
		if tc.expect != nil {
			if problem := tc.expect(decode(t, rec)); problem != "" {
				t.Errorf("%s %s: %s", tc.method, tc.target, problem)
			}
		}
	}
}

// bodyText renders a decoded body back to text for a failure message.
func bodyText(b map[string]any) string {
	out, _ := json.Marshal(b)
	return string(out)
}

func TestOptionsPreflight(t *testing.T) {
	h := newHarness(t, "vllm", vllmEngine(), nil)
	rec := h.do(http.MethodOptions, "/v1/chat/completions")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d", rec.Code)
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "*" {
		t.Errorf("allow-origin = %q", got)
	}
	for _, want := range []string{"Content-Type", "Authorization", "X-Conversation-Id", "api-key"} {
		if !strings.Contains(rec.Header().Get("Access-Control-Allow-Headers"), want) {
			t.Errorf("allow-headers must list %s: %q", want, rec.Header().Get("Access-Control-Allow-Headers"))
		}
	}
	if got := rec.Header().Get("Access-Control-Allow-Methods"); !strings.Contains(got, "DELETE") {
		t.Errorf("allow-methods = %q", got)
	}
	// The UI is same-origin, so this header is only there for convenience; every
	// other answer carries it too.
	if got := h.get("/props").Header().Get("Access-Control-Allow-Origin"); got != "*" {
		t.Errorf("props allow-origin = %q", got)
	}
}

// writeDist lays out a fake build output: one page, one hashed bundle under the
// immutable segment, and one file that must never be reachable.
func writeDist(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	files := map[string]string{
		"index.html":                    "<!doctype html><title>webui</title>",
		"favicon.ico":                   "icon",
		"_app/immutable/chunks/app.js":  "console.log(1)",
		"_app/immutable/assets/app.css": "body{}",
		"_app/version.json":             `{"version":"x"}`,
		"notbuilt":                      "no extension, no file",
	}
	for name, text := range files {
		full := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// a sibling of dist that a prefix-only check would hand out
	if err := os.WriteFile(filepath.Join(filepath.Dir(root), "dist-secret"), []byte("private"), 0o644); err != nil {
		t.Logf("cannot place a sibling file here: %v", err)
	}
	return root
}

func TestStatic(t *testing.T) {
	root := writeDist(t)
	h := newHarness(t, "vllm", vllmEngine(), func(c *appconf.Config) { c.Dist = root })

	cases := []struct {
		target string
		code   int
		body   string
		ctype  string
		cache  string
	}{
		{"/", 200, "<!doctype html>", "text/html", "no-cache"},
		{"/index.html", 200, "<!doctype html>", "text/html", "no-cache"},
		{"/new/chat", 200, "<!doctype html>", "text/html", "no-cache"},
		{"/_app/immutable/chunks/app.js", 200, "console.log(1)", "text/javascript", "immutable"},
		{"/_app/immutable/assets/app.css", 200, "body{}", "text/css", "immutable"},
		{"/_app/version.json", 200, `{"version":"x"}`, "application/json", "no-cache"},
		{"/favicon.ico", 200, "icon", "image/x-icon", "no-cache"},
		{"/missing.js", 404, "", "", ""},
		{"/deep/missing.js", 404, "", "", ""},
		{"/missing.png", 404, "", "", ""},
		// A leading dot is not an extension, so this is a route path: the page
		// answers, exactly as the Node router does.
		{"/.hidden", 200, "<!doctype html>", "text/html", "no-cache"},
		{"/_app/immutable/chunks/gone.js", 404, "", "", ""},
	}
	for _, tc := range cases {
		rec := h.get(tc.target)
		if rec.Code != tc.code {
			t.Errorf("GET %s = %d, want %d", tc.target, rec.Code, tc.code)
			continue
		}
		if tc.code == 404 {
			// A missing module must not answer with the page, or the browser
			// reports a syntax error instead of a 404.
			if strings.Contains(rec.Body.String(), "<!doctype") {
				t.Errorf("GET %s fell back to the SPA", tc.target)
			}
			continue
		}
		if !strings.Contains(rec.Body.String(), tc.body) {
			t.Errorf("GET %s body = %q, want %q", tc.target, rec.Body, tc.body)
		}
		if got := rec.Header().Get("Content-Type"); !strings.HasPrefix(got, tc.ctype) {
			t.Errorf("GET %s type = %q, want %q", tc.target, got, tc.ctype)
		}
		if got := rec.Header().Get("Cache-Control"); !strings.Contains(got, tc.cache) {
			t.Errorf("GET %s cache = %q, want %q", tc.target, got, tc.cache)
		}
	}
}

// TestStaticStaysInDist: encoded escapes are refused by the guard, folded
// escapes cannot leave the root either, and nothing outside the build output is
// ever handed out.
func TestStaticStaysInDist(t *testing.T) {
	root := writeDist(t)
	h := newHarness(t, "vllm", vllmEngine(), func(c *appconf.Config) { c.Dist = root })

	// A browser sends ".." folded away, so these only arrive on a hand-built
	// request. They decode to a path above the root, which the guard refuses.
	for _, target := range []string{
		"/%2e%2e/dist-secret",
		"/%2e%2e/%2e%2e/etc/passwd",
		"/..%2fdist-secret",
		"/%2e%2e%2f%2e%2e%2fetc/passwd",
	} {
		rec := h.get(target)
		if rec.Body.String() == "private" || strings.Contains(rec.Body.String(), "root:") {
			t.Errorf("GET %s escaped the dist root: %s", target, rec.Body)
		}
		if rec.Code != http.StatusForbidden && rec.Code != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 403 or 404", target, rec.Code)
		}
	}
	if got := h.get("/%2e%2e/dist-secret").Code; got != http.StatusForbidden {
		t.Errorf("an escape out of the root = %d, want 403", got)
	}

	// A literal ".." is normalised before the file is looked up, exactly as the
	// Node router does, so it lands inside the root and never reads the sibling.
	for _, target := range []string{"/../dist-secret", "/../../etc/passwd", "/./../dist-secret"} {
		rec := h.get(target)
		if rec.Body.String() == "private" || strings.Contains(rec.Body.String(), "root:") {
			t.Errorf("GET %s read outside dist: %s", target, rec.Body)
		}
	}

	// A directory with no page in it answers 404 rather than a listing.
	if got := h.get("/_app/"); got.Code != http.StatusNotFound || strings.Contains(got.Body.String(), "Index of") {
		t.Errorf("/_app/ = %d %s, want a 404 and no listing", got.Code, got.Body)
	}
}

func TestStaticHead(t *testing.T) {
	root := writeDist(t)
	h := newHarness(t, "vllm", vllmEngine(), func(c *appconf.Config) { c.Dist = root })
	rec := h.do(http.MethodHead, "/_app/immutable/chunks/app.js")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if rec.Body.Len() != 0 {
		t.Errorf("HEAD must not carry a body, got %q", rec.Body)
	}
	if rec.Header().Get("Content-Length") == "" {
		t.Error("HEAD still reports the length the GET would send")
	}
}

func TestStaticServesTheRealBuild(t *testing.T) {
	// The repo ships ./dist, so these guards run against the real output too.
	root := repoDist(t)
	if root == "" {
		t.Skip("no built UI in this checkout")
	}
	h := newHarness(t, "vllm", vllmEngine(), func(c *appconf.Config) { c.Dist = root })
	rec := h.get("/")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "<") {
		t.Fatalf("GET / = %d %s", rec.Code, rec.Body)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-cache" {
		t.Errorf("the page must not be cached: %q", got)
	}
	assets, err := filepath.Glob(filepath.Join(root, "_app", "immutable", "*.js"))
	if err != nil || len(assets) == 0 {
		t.Skip("this build has no hashed bundle to check")
	}
	target := "/_app/immutable/" + filepath.Base(assets[0])
	bundle := h.get(target)
	if bundle.Code != http.StatusOK {
		t.Fatalf("GET %s = %d", target, bundle.Code)
	}
	if got := bundle.Header().Get("Cache-Control"); !strings.Contains(got, "immutable") {
		t.Errorf("a hashed asset must be cached for a year: %q", got)
	}
	if got := bundle.Header().Get("Content-Type"); !strings.HasPrefix(got, "text/javascript") {
		t.Errorf("bundle type = %q", got)
	}
}

// repoDist walks up from the test's working directory to the dist the repo
// ships, so the guards above are also tried against a real build.
func repoDist(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		return ""
	}
	for i := 0; i < 6; i++ {
		candidate := filepath.Join(dir, "dist")
		if _, err := os.Stat(filepath.Join(candidate, "index.html")); err == nil {
			return candidate
		}
		dir = filepath.Dir(dir)
	}
	return ""
}

const statsMetrics = `vllm:request_prefill_time_seconds_count 3
vllm:request_prefill_time_seconds_sum 0.09
vllm:request_decode_time_seconds_count 3
vllm:request_decode_time_seconds_sum 0.6
vllm:request_queue_time_seconds_sum 0.03
vllm:time_to_first_token_seconds_count 3
vllm:time_to_first_token_seconds_sum 0.12
vllm:inter_token_latency_seconds_count 30
vllm:inter_token_latency_seconds_sum 0.02
vllm:request_prefill_kv_computed_tokens_count 3
vllm:request_prefill_kv_computed_tokens_sum 300
vllm:prefix_cache_hits_total 60
vllm:prefix_cache_queries_total 300
vllm:prompt_tokens_total 300
vllm:generation_tokens_total 150
vllm:num_requests_running 0
vllm:num_requests_waiting 0
vllm:kv_cache_usage_perc 0.25
vllm:spec_decode_num_drafts_total 10
vllm:spec_decode_num_draft_tokens_total 100
vllm:spec_decode_num_accepted_tokens_total 62
vllm:num_preemptions_total 1
vllm:cache_config_info{block_size="16",num_gpu_blocks="100",kv_cache_size_tokens="1600",enable_prefix_caching="True",gpu_memory_utilization="0.9"} 1
`

// statsMetricsMore is the same engine a little later: 200 more generated tokens
// and 100 more drafts.
func statsMetricsMore() string {
	out := strings.Replace(statsMetrics, "vllm:generation_tokens_total 150", "vllm:generation_tokens_total 350", 1)
	out = strings.Replace(out, "vllm:spec_decode_num_draft_tokens_total 100", "vllm:spec_decode_num_draft_tokens_total 200", 1)
	return strings.Replace(out, "vllm:spec_decode_num_accepted_tokens_total 62", "vllm:spec_decode_num_accepted_tokens_total 124", 1)
}

func (f *fakeEngine) set(path, body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.routes[path] = body
}

func TestStatsShape(t *testing.T) {
	eng := vllmEngine()
	eng.routes["/metrics"] = statsMetrics
	h := newHarness(t, "vllm", eng, nil)
	rec := h.get("/vllm/stats")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body %s", rec.Code, rec.Body)
	}
	body := decode(t, rec)
	if got := backend.Str(body, "title"); got != "vLLM engine" {
		t.Errorf("title = %q", got)
	}
	at := backend.Str(body, "sampled_at")
	if _, err := time.Parse(time.RFC3339, at); err != nil {
		t.Errorf("sampled_at %q is not RFC3339: %v", at, err)
	}
	gauges := backend.Obj(body, "gauges")
	for k, want := range map[string]float64{
		"kv_cache_usage_percent":     25,
		"prefix_cache_hit_percent":   20,
		"spec_decode_accept_percent": 62,
		"tokens_per_step":            7.2,
		"preemptions":                1,
		"requests_running":           0,
	} {
		if got := backend.NumOr(gauges, k, -1); got != want {
			t.Errorf("gauges.%s = %v, want %v", k, got, want)
		}
	}
	counters := backend.Obj(body, "counters")
	for k, want := range map[string]float64{
		"prompt_tokens":      300,
		"generation_tokens":  150,
		"requests":           3,
		"avg_ttft_ms":        40,
		"avg_prefill_ms":     30,
		"avg_decode_ms":      200,
		"avg_queue_ms":       10,
		"avg_prefill_tokens": 100,
	} {
		if got := backend.NumOr(counters, k, -1); got != want {
			t.Errorf("counters.%s = %v, want %v", k, got, want)
		}
	}
	engine := backend.Obj(body, "engine")
	if backend.Str(engine, "block_size") != "16" || backend.Str(engine, "kv_cache_size_tokens") != "1600" {
		t.Errorf("engine block came from the labelled metric: %v", engine)
	}
	if _, ok := engine["model"]; ok {
		t.Error("an absent config label must not appear as null")
	}
	labels := backend.Obj(body, "labels")
	if backend.Str(labels, "kv") != "KV cache" || backend.Str(labels, "ttft") != "First token" {
		t.Errorf("labels = %v", labels)
	}
	if rates := backend.Obj(body, "rates"); len(rates) != 0 {
		t.Errorf("the first sample has no window to divide: %v", rates)
	}
}

// TestStatsCacheAndWindow: several tabs cost one upstream read, and the rate
// window is measured between two scrapes.
func TestStatsCacheAndWindow(t *testing.T) {
	eng := vllmEngine()
	eng.set("/metrics", statsMetrics)
	h := newHarness(t, "vllm", eng, nil)

	rec := h.get("/vllm/stats")
	scrapes := eng.count("/metrics")
	if scrapes != 1 {
		t.Fatalf("want one scrape, got %d", scrapes)
	}
	body := decode(t, rec)
	firstAt := backend.Str(body, "sampled_at")

	// A second tab, still inside the short cache.
	again := decode(t, h.get("/vllm/stats"))
	if got := eng.count("/metrics"); got != scrapes {
		t.Errorf("a second poll must be served from the cache, scrapes = %d", got)
	}
	if got := backend.Str(again, "sampled_at"); got != firstAt {
		t.Errorf("the cached scrape keeps its own sample time: %s then %s", firstAt, got)
	}
	// Same scrape twice in a row is not a window: the gap is zero.
	if rates := backend.Obj(again, "rates"); len(rates) != 1 || rates["window_s"] == nil {
		t.Errorf("a zero-length window must only report its length: %v", rates)
	}

	eng.set("/metrics", statsMetricsMore())
	time.Sleep(statsTTL + 100*time.Millisecond)
	third := decode(t, h.get("/vllm/stats"))
	if got := eng.count("/metrics"); got != scrapes+1 {
		t.Errorf("after the cache aged out the engine must be read again, scrapes = %d", got)
	}
	rates := backend.Obj(third, "rates")
	window := backend.NumOr(rates, "window_s", 0)
	if window < 0.5 {
		t.Fatalf("window_s = %v, want a real gap", window)
	}
	// 200 generated tokens over the window.
	rate := backend.NumOr(rates, "generation_tokens_per_second", 0)
	if want := 200 / window; rate < want*0.5 || rate > want*2 {
		t.Errorf("generation_tokens_per_second = %v, want about %v", rate, want)
	}
	if _, ok := rates["steps_per_second"]; !ok {
		t.Error("vLLM accumulates drafts, so its rate block carries steps_per_second")
	}
	if _, ok := rates["window_accept_percent"]; !ok {
		t.Error("draft tokens moved, so the window reports acceptance")
	} else if got := rates["window_accept_percent"]; got != 62.0 {
		t.Errorf("window_accept_percent = %v, want 62", got)
	}
}

func TestStatsUnavailable(t *testing.T) {
	h := newHarness(t, "vllm", vllmEngine(), nil)
	rec := h.get("/vllm/stats")
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 when the snapshot fails", rec.Code)
	}
	if got := backend.Str(decode(t, rec), "error"); !strings.Contains(got, "cannot read stats from vllm") {
		t.Errorf("body = %s", rec.Body)
	}
	if !h.hasLog("snapshot failed") {
		t.Errorf("the failed read must be logged, got %v", h.logs())
	}
}

func TestStatsStrataLabels(t *testing.T) {
	eng := newEngine(map[string]string{
		"/v1/models": `{"object":"list","data":[{"id":"m","object":"model"}]}`,
		"/health":    `{"service":"strata","model":"m"}`,
		"/props":     `{"total_slots":1,"chat_template":"t","default_generation_settings":{"n_ctx":4096,"params":{}}}`,
		"/metrics":   `{"engine":{"model":"m","max_context":4096},"totals":{"requests":2,"prompt_tokens":200,"reused":50,"output_tokens":80,"prompt_ms":100,"decode_ms":2000},"live":{"state":"idle","queued":0},"requests":[{"prompt_ms":40,"decode_ms":900,"decode_tok_s":40},{"prompt_ms":60,"decode_ms":1100,"decode_tok_s":45}]}`,
		"/v1/status": `{"service":"strata","engine":"mock","last_timings":{"draft_n":40,"draft_n_accepted":25},"machine":{"gpu":{"used_mib":6144,"total_mib":12288}}}`,
	})
	h := newHarness(t, "strata", eng, nil)
	body := decode(t, h.get("/engine/stats"))
	if got := backend.Str(body, "title"); got != "Strata engine" {
		t.Errorf("title = %q", got)
	}
	labels := backend.Obj(body, "labels")
	if backend.Str(labels, "kv") != "VRAM" || backend.Str(labels, "ttft") != "Prompt read" {
		t.Errorf("Strata names its rows differently: %v", labels)
	}
	gauges := backend.Obj(body, "gauges")
	if got := backend.NumOr(gauges, "kv_cache_usage_percent", 0); got != 50 {
		t.Errorf("kv row must show VRAM percent, got %v", got)
	}
	if got := backend.Str(gauges, "kv_cache_label"); got != "VRAM" {
		t.Errorf("kv_cache_label = %q", got)
	}
	if got := backend.NumOr(gauges, "spec_decode_accept_percent", 0); got != 62.5 {
		t.Errorf("accept percent from the last timings = %v, want 62.5", got)
	}
	counters := backend.Obj(body, "counters")
	if got := backend.NumOr(counters, "avg_ttft_ms", 0); got != 50 {
		t.Errorf("avg_ttft_ms = %v, want the mean of the two requests", got)
	}
	engine := backend.Obj(body, "engine")
	if backend.Str(engine, "serving") != "one request at a time" {
		t.Errorf("engine = %v", engine)
	}
	if got := backend.NumOr(gauges, "requests_running", -1); got != 0 {
		t.Errorf("an idle engine runs nothing, got %v", got)
	}
}

// probeEngine answers the two startup probes the way an engine that thinks but
// cannot see does.
func probeEngine(t *testing.T) *fakeEngine {
	eng := vllmEngine()
	eng.chat = func(w http.ResponseWriter, _ *http.Request, body map[string]any) {
		if isVisionProbe(body) {
			http.Error(w, `{"error":{"message":"This model does not support image inputs"}}`, http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, `data: {"choices":[{"index":0,"delta":{"reasoning":"because"}}]}`+"\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}
	return eng
}

func isVisionProbe(body map[string]any) bool {
	for _, m := range backend.Arr(body, "messages") {
		mm, ok := m.(map[string]any)
		if !ok {
			continue
		}
		for _, p := range backend.Arr(mm, "content") {
			pm, ok := p.(map[string]any)
			if ok && backend.Str(pm, "type") == "image_url" {
				return true
			}
		}
	}
	return false
}

// runProbes starts the probes and waits for them, releasing the fake engine on
// the way out.
func runProbes(h *harness) {
	ctx, cancel := context.WithCancel(context.Background())
	h.s.StartProbes(ctx)
	h.s.WaitProbes(ctx, 10*time.Second)
	cancel()
}

func TestCapabilityProbes(t *testing.T) {
	eng := probeEngine(t)
	h := newHarness(t, "vllm", eng, nil)
	runProbes(h)

	if !h.hasLog("thinking support: yes (engine emitted reasoning deltas)") {
		t.Errorf("the thinking probe must report what it saw, log: %v", h.logs())
	}
	if !h.hasLog("vision support: no (engine rejects image inputs, HTTP 400)") {
		t.Errorf("the vision probe must report the reason, log: %v", h.logs())
	}
	if eng.count("/v1/chat/completions") != 2 {
		t.Errorf("two probes, got %d calls", eng.count("/v1/chat/completions"))
	}
	body := decode(t, h.get("/props"))
	if got := backend.Str(body, "chat_template"); got != "enable_thinking" {
		t.Errorf("the probe result stands in for the template vLLM does not serve, got %q", got)
	}
	if backend.Bool(backend.Obj(body, "modalities"), "vision") {
		t.Error("a rejected image probe must keep vision off")
	}
	think := eng.sentChat(0)
	if !backend.Bool(think, "stream") || backend.NumOr(think, "max_tokens", 0) != 16 {
		t.Errorf("the thinking probe is one short streamed completion: %v", think)
	}
	if backend.Str(think, "model") != "qwen-test" {
		t.Errorf("the probe must name a model: %v", think["model"])
	}
}

func TestVisionProbeIsNonStreamingAndShort(t *testing.T) {
	eng := vllmEngine()
	eng.chat = func(w http.ResponseWriter, _ *http.Request, _ map[string]any) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"choices":[{"index":0,"message":{"content":"A"}}]}`)
	}
	h := newHarness(t, "vllm", eng, nil)
	runProbes(h)

	var vision map[string]any
	for i := 0; i < 4; i++ {
		if b := eng.sentChat(i); b != nil && isVisionProbe(b) {
			vision = b
		}
	}
	if vision == nil {
		t.Fatal("no image request reached the engine")
	}
	if backend.Bool(vision, "stream") {
		t.Error("the vision probe is one non-streaming call")
	}
	if got := backend.NumOr(vision, "max_tokens", 0); got != 1 {
		t.Errorf("max_tokens = %v, want 1: the probe only needs the encoder to run", got)
	}
	if !h.d.Flags.Vision.Get() {
		t.Error("an accepted image turns vision on")
	}
	if !h.hasLog("vision support: yes (engine accepted an image)") {
		t.Errorf("log = %v", h.logs())
	}
}

func TestProbeSwitches(t *testing.T) {
	eng := probeEngine(t)
	h := newHarness(t, "vllm", eng, func(c *appconf.Config) {
		c.ProbeThinking = false
		c.Vision = "0"
	})
	runProbes(h)
	if eng.count("/v1/chat/completions") != 0 {
		t.Errorf("both probes are off, got %d calls", eng.count("/v1/chat/completions"))
	}
	if !h.hasLog("vision: off (VLLM_MODALITY_VISION)") {
		t.Errorf("a forced value must be reported, log: %v", h.logs())
	}
	body := decode(t, h.get("/props"))
	if got := backend.Str(body, "chat_template"); got != "" {
		t.Errorf("with no probe nothing proves thinking, got %q", got)
	}

	forced := newHarness(t, "vllm", probeEngine(t), func(c *appconf.Config) {
		c.Vision = "1"
		c.ProbeThinking = false
	})
	runProbes(forced)
	if !forced.d.Flags.Vision.Get() {
		t.Error("VLLM_MODALITY_VISION=1 forces vision on")
	}
	if !backend.Bool(backend.Obj(decode(t, forced.get("/props")), "modalities"), "vision") {
		t.Error("/props must tell the UI it can take images")
	}
}

// TestProbeSkipVisionSwitch covers VLLM_PROBE_VISION=0 on its own.
func TestProbeSkipVisionSwitch(t *testing.T) {
	eng := probeEngine(t)
	h := newHarness(t, "vllm", eng, func(c *appconf.Config) {
		c.ProbeVision = false
		c.ProbeThinking = false
	})
	runProbes(h)
	if !h.hasLog("vision: off (probe skipped, set VLLM_MODALITY_VISION=1 to force on)") {
		t.Errorf("log = %v", h.logs())
	}
}

// TestPropsProbeGrace: /props waits for the probes, but only briefly, so a dead
// engine cannot stall the first paint.
func TestPropsProbeGrace(t *testing.T) {
	if probeGrace > 2*time.Second {
		t.Errorf("props must not wait longer than 2 s, grace = %s", probeGrace)
	}
	eng := vllmEngine()
	eng.chat = func(w http.ResponseWriter, r *http.Request, _ map[string]any) {
		<-r.Context().Done() // a hung engine
	}
	h := newHarness(t, "vllm", eng, nil)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	h.s.StartProbes(ctx)
	start := time.Now()
	h.s.WaitProbes(ctx, 150*time.Millisecond)
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("the wait must be bounded, took %s", elapsed)
	}
	// The page still loads while the probe is stuck.
	if rec := h.get("/props"); rec.Code != http.StatusOK {
		t.Errorf("props = %d while a probe is running", rec.Code)
	}
}

// disabledReason accepts the llama-server body a route sends when the feature
// was not turned on at startup.
func disabledReason(b map[string]any) string {
	err, _ := b["error"].(map[string]any)
	if err == nil || err["type"] != "feature_disabled" {
		return "want feature_disabled"
	}
	if err["message"] != "this feature is disabled" {
		return "message = " + fmt.Sprint(err["message"])
	}
	return ""
}

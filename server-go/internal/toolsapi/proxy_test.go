package toolsapi

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// upstream records what the proxy sent and answers with a fixed reply.
type upstream struct {
	server *httptest.Server

	mu      sync.Mutex
	hits    int
	method  string
	path    string
	query   string
	body    string
	headers http.Header
}

func newUpstream(t *testing.T, status int, reply, contentType string) *upstream {
	return newUpstreamFunc(t, func(w http.ResponseWriter, r *http.Request) {
		if contentType != "" {
			w.Header().Set("Content-Type", contentType)
		}
		w.WriteHeader(status)
		_, _ = io.WriteString(w, reply)
	})
}

// newUpstreamFunc builds an upstream that records every request it is given.
func newUpstreamFunc(t *testing.T, serve func(http.ResponseWriter, *http.Request)) *upstream {
	t.Helper()
	u := &upstream{}
	u.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		u.mu.Lock()
		u.hits++
		u.method = r.Method
		u.path = r.URL.Path
		u.query = r.URL.RawQuery
		u.body = string(body)
		u.headers = r.Header.Clone()
		u.mu.Unlock()
		serve(w, r)
	}))
	t.Cleanup(u.server.Close)
	return u
}

func (u *upstream) snapshot() (string, http.Header, string, string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.method, u.headers, u.body, u.path
}

func (u *upstream) nHits() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.hits
}

func proxyReq(t *testing.T, p *Proxy, method, target, body string, headers map[string]string) *flushRecorder {
	t.Helper()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, "/cors-proxy?url="+target, nil)
	} else {
		r = httptest.NewRequest(method, "/cors-proxy?url="+target, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	rec := newFlushRecorder()
	p.Handle(rec, r)
	return rec
}

func TestProxyDisabled(t *testing.T) {
	if p := NewProxy(nil, testLog(t)); p != nil {
		t.Error("NewProxy must return nil with no allow list, so the caller can drop the route")
	}
	rec := newFlushRecorder()
	(*Proxy)(nil).Handle(rec, httptest.NewRequest(http.MethodGet, "/cors-proxy?url=http://x/y", nil))
	wantBody(t, rec, http.StatusForbidden,
		`{"error":{"message":"this feature is disabled","type":"feature_disabled"}}`)

	rec = newFlushRecorder()
	(*Proxy)(nil).Handle(rec, httptest.NewRequest(http.MethodPost, "/cors-proxy", nil))
	if rec.Code != http.StatusForbidden {
		t.Errorf("a nil proxy must refuse POST too, got %d", rec.Code)
	}
}

func TestProxyForwardsTarget(t *testing.T) {
	u := newUpstream(t, http.StatusTeapot, "hello from upstream", "text/plain; charset=utf-8")
	p := NewProxy([]string{"http://ui.test"}, testLog(t))

	rec := proxyReq(t, p, http.MethodPost, "http://"+strings.TrimPrefix(u.server.URL, "http://")+"/mcp?session=7",
		`{"jsonrpc":"2.0"}`, nil)

	if rec.Code != http.StatusTeapot {
		t.Fatalf("status = %d, want the upstream status", rec.Code)
	}
	if got := rec.Body.String(); got != "hello from upstream" {
		t.Errorf("body = %q", got)
	}
	if got := rec.Header().Get("Content-Type"); got != "text/plain; charset=utf-8" {
		t.Errorf("content type = %q, want the upstream one", got)
	}
	method, _, body, path := u.snapshot()
	if method != http.MethodPost || path != "/mcp" || body != `{"jsonrpc":"2.0"}` {
		t.Errorf("upstream saw %s %s body %q", method, path, body)
	}
	if u.query != "session=7" {
		t.Errorf("query = %q, want it forwarded with the target", u.query)
	}
	// The caller's own Content-Type is not copied: only a header under the proxy
	// prefix reaches the upstream. See TestProxyHeaderRenameAndLeak.
	if got := u.headers.Get("Content-Type"); got != "" {
		t.Errorf("upstream content type = %q, want it dropped", got)
	}
}

func TestProxyGetForwardsWithoutBody(t *testing.T) {
	u := newUpstream(t, http.StatusOK, "ok", "application/json")
	p := NewProxy([]string{"*"}, testLog(t))

	rec := proxyReq(t, p, http.MethodGet, "http://"+strings.TrimPrefix(u.server.URL, "http://")+"/sse", "", nil)
	if rec.Code != http.StatusOK || rec.Body.String() != "ok" {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body)
	}
	method, headers, body, _ := u.snapshot()
	if method != http.MethodGet || body != "" {
		t.Errorf("upstream saw %s with body %q", method, body)
	}
	if headers.Get("Content-Type") != "" {
		t.Errorf("a GET must not carry a content type upstream: %q", headers.Get("Content-Type"))
	}
	// "*" lets any origin through, so the response is usable from a browser.
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "*" {
		t.Errorf("allow origin = %q", got)
	}
}

func TestProxyHeaderRenameAndLeak(t *testing.T) {
	u := newUpstream(t, http.StatusOK, "ok", "text/plain")
	p := NewProxy([]string{"http://ui.test"}, testLog(t))

	rec := proxyReq(t, p, http.MethodPost, "http://"+strings.TrimPrefix(u.server.URL, "http://")+"/mcp", `{}`,
		map[string]string{
			// Renamed and forwarded: what an MCP server needs.
			"x-llama-server-proxy-header-authorization":  "Bearer mcp-token",
			"x-llama-server-proxy-header-mcp-session-id": "abc",
			// The webui prefixes every MCP header, body type included. The plain
			// Content-Type of the /cors-proxy request must not overwrite it.
			"x-llama-server-proxy-header-content-type": "application/json",
			"Content-Type": "text/plain",
			// Must never reach a third-party host.
			"Cookie":        "session=secret",
			"Authorization": "Bearer local-llama-server-key",
			"X-Api-Key":     "local-key",
			"Host":          "elsewhere.test",
			"Referer":       "http://elsewhere.test/page",
		})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body %s", rec.Code, rec.Body)
	}

	_, headers, _, _ := u.snapshot()
	if got := headers.Get("Authorization"); got != "Bearer mcp-token" {
		t.Errorf("renamed authorization = %q, want the prefixed value", got)
	}
	if got := headers.Get("Mcp-Session-Id"); got != "abc" {
		t.Errorf("renamed mcp-session-id = %q", got)
	}
	// The prefixed body type arrives, and the caller's own text/plain on the
	// /cors-proxy request does not overwrite it.
	if got := headers.Values("Content-Type"); len(got) != 1 || got[0] != "application/json" {
		t.Errorf("renamed content-type = %v, want one application/json", got)
	}
	if got := headers.Get("Cookie"); got != "" {
		t.Errorf("the client cookie leaked upstream: %q", got)
	}
	if got := headers.Values("Authorization"); len(got) != 1 {
		t.Errorf("authorization must appear once, got %v", got)
	}
	for _, name := range []string{"X-Api-Key", "Referer"} {
		if got := headers.Get(name); got != "" {
			t.Errorf("%s leaked upstream: %q", name, got)
		}
	}
	// The local API key must not be reachable through the error path either.
	if strings.Contains(rec.Body.String(), "local-key") {
		t.Errorf("proxy body echoed the key: %s", rec.Body)
	}
}

func TestProxyRejectsBadTargets(t *testing.T) {
	p := NewProxy([]string{"http://ui.test"}, testLog(t))

	cases := []struct {
		name string
		url  string
		want string
	}{
		{"missing", "", `{"error":{"message":"invalid URL: no scheme","type":"invalid_request_error","code":400}}`},
		{"no scheme", "example.test/tools", `{"error":{"message":"invalid URL: no scheme","type":"invalid_request_error","code":400}}`},
		{"ftp", "ftp://example.test/tools", `{"error":{"message":"unsupported URL scheme: ftp","type":"invalid_request_error","code":400}}`},
		{"file", "file:///etc/passwd", `{"error":{"message":"unsupported URL scheme: file","type":"invalid_request_error","code":400}}`},
		{"no host", "http:///tools", `{"error":{"message":"invalid target URL: missing host","type":"invalid_request_error","code":400}}`},
		{"credentials", "http://user:pw@example.test/tools", `{"error":{"message":"authentication in target URL is not supported","type":"invalid_request_error","code":400}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := proxyReq(t, p, http.MethodGet, tc.url, "", nil)
			wantBody(t, rec, http.StatusBadRequest, tc.want)
		})
	}
}

func TestProxyUpstreamUnreachable(t *testing.T) {
	// 127.0.0.1 on a closed port: nothing is listening.
	u := newUpstream(t, http.StatusOK, "x", "text/plain")
	addr := strings.TrimPrefix(u.server.URL, "http://")
	dead := "http://" + addr[:strings.LastIndex(addr, ":")] + ":1/mcp"
	p := NewProxy([]string{"http://ui.test"}, testLog(t))

	rec := proxyReq(t, p, http.MethodGet, dead, "", nil)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body)
	}
	if !strings.HasPrefix(rec.Body.String(), `{"error":{"message":"proxy request failed:`) {
		t.Errorf("body = %s", rec.Body)
	}
	if !strings.Contains(rec.Body.String(), `"type":"server_error","code":500}`) {
		t.Errorf("body = %s", rec.Body)
	}
}

func TestProxyCORSScope(t *testing.T) {
	u := newUpstream(t, http.StatusOK, "ok", "text/plain")
	target := "http://" + strings.TrimPrefix(u.server.URL, "http://") + "/mcp"
	p := NewProxy([]string{"http://ui.test"}, testLog(t))

	rec := proxyReq(t, p, http.MethodGet, target, "", map[string]string{"Origin": "http://ui.test"})
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "http://ui.test" {
		t.Errorf("allow origin = %q, want the allowed one echoed", got)
	}

	rec = proxyReq(t, p, http.MethodGet, target, "", map[string]string{"Origin": "http://evil.test"})
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("a foreign origin must get no CORS header, got %q", got)
	}

	// A preflight must be answerable without touching the upstream.
	before := u.nHits()
	rec = proxyReq(t, p, http.MethodOptions, target, "", map[string]string{
		"Origin":                         "http://ui.test",
		"Access-Control-Request-Method":  "POST",
		"Access-Control-Request-Headers": "content-type, x-llama-server-proxy-header-authorization",
	})
	if rec.Code != http.StatusNoContent {
		t.Fatalf("preflight status = %d body %s", rec.Code, rec.Body)
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "http://ui.test" {
		t.Errorf("preflight allow origin = %q", got)
	}
	if got := rec.Header().Get("Access-Control-Allow-Methods"); !strings.Contains(got, "POST") {
		t.Errorf("preflight allow methods = %q", got)
	}
	if got := rec.Header().Get("Access-Control-Allow-Headers"); !strings.Contains(got, "content-type") {
		t.Errorf("preflight allow headers = %q", got)
	}
	if got := u.nHits(); got != before {
		t.Errorf("a preflight must not reach the upstream, hits went %d -> %d", before, got)
	}

	// An unsupported method is refused before any upstream work.
	rec = proxyReq(t, p, http.MethodDelete, target, "", nil)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", rec.Code)
	}
}

// TestProxyClientLeavesCancelsUpstream proves the upstream request dies with the
// caller: the relay builds it from r.Context(), so a closed client connection
// shows up as a cancelled context on the other side.
func TestProxyClientLeavesCancelsUpstream(t *testing.T) {
	failed := make(chan error, 1)
	up := newUpstreamFunc(t, func(w http.ResponseWriter, r *http.Request) {
		// An MCP server that keeps a stream open: headers and one chunk, then it
		// waits. Without a first byte the client would never see a response, and
		// it could not leave.
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "data: hello\n\n")
		if fl, ok := w.(http.Flusher); ok {
			fl.Flush()
		}
		<-r.Context().Done()
		failed <- r.Context().Err()
	})

	p := NewProxy([]string{"http://ui.test"}, testLog(t))
	srv := httptest.NewServer(http.HandlerFunc(p.Handle))
	defer srv.Close()

	req, err := http.NewRequest(http.MethodGet, srv.URL+"/cors-proxy?url="+up.server.URL+"/slow", nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 64)
	if n, err := res.Body.Read(buf); err != nil || n == 0 {
		res.Body.Close()
		t.Fatalf("the first relayed chunk never arrived: n=%d err=%v", n, err)
	}
	res.Body.Close() // the browser tab goes away

	select {
	case err := <-failed:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("upstream context ended with %v, want cancellation", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the upstream request was not cancelled after the client left")
	}
}

// TestProxyStripsHopByHop keeps the local server's answer to the browser, not
// the upstream copy of it.
func TestProxyStripsHopByHop(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "http://upstream.test")
		w.Header().Set("X-Upstream-Note", "keep me")
		w.Header().Set("Server", "upstream-software")
		w.WriteHeader(http.StatusAccepted)
		_, _ = io.WriteString(w, "body")
	}))
	defer up.Close()

	p := NewProxy([]string{"http://ui.test"}, testLog(t))
	rec := proxyReq(t, p, http.MethodGet, up.URL+"/x", "", map[string]string{"Origin": "http://ui.test"})

	if rec.Code != http.StatusAccepted {
		t.Errorf("status = %d", rec.Code)
	}
	if got := rec.Header().Get("X-Upstream-Note"); got != "keep me" {
		t.Errorf("a normal upstream header must be copied, got %q", got)
	}
	if got := rec.Header().Get("Server"); got == "upstream-software" {
		t.Error("Server must be stripped like llama-server does")
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "http://ui.test" {
		t.Errorf("allow origin = %q, want the local answer only", got)
	}
	if got := rec.Body.String(); got != "body" {
		t.Errorf("body = %q", got)
	}
}

// roundTripFunc lets a test watch the request the relay builds.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// TestProxyExchangeIsBounded: llama-server gives the relay a 600 s read timeout
// and a 600 s write timeout. Here one context bounds the whole exchange.
func TestProxyExchangeIsBounded(t *testing.T) {
	var deadline time.Time
	p := NewProxy([]string{"http://ui.test"}, testLog(t))
	p.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if d, ok := r.Context().Deadline(); ok {
			deadline = d
		}
		return nil, errors.New("no route to host")
	})

	rec := proxyReq(t, p, http.MethodGet, "http://target.test/mcp", "", nil)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body)
	}
	if deadline.IsZero() {
		t.Fatal("the upstream request carries no deadline")
	}
	left := time.Until(deadline)
	if left <= 590*time.Second || left > proxyTimeout {
		t.Errorf("the exchange is bounded by %v, want about %v", left.Round(time.Millisecond), proxyTimeout)
	}
}

// TestProxyTargetTable drives the target rules without touching the network.
func TestProxyTargetTable(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		wantURL string
		wantErr string
	}{
		{"path kept", "http://h.test:8080/mcp?s=1", "http://h.test:8080/mcp?s=1", ""},
		{"empty path becomes root", "http://h.test", "http://h.test/", ""},
		{"bracketed ipv6", "http://[::1]:9/mcp", "http://[::1]:9/mcp", ""},
		{"username kept out of the request", "http://someone@h.test/mcp", "http://h.test/mcp", ""},
		// Go lowercases the scheme, so llama-server would reject this one.
		{"uppercase scheme", "HTTP://h.test/mcp", "http://h.test/mcp", ""},
		{"no scheme", "h.test/mcp", "", "invalid URL: no scheme"},
		{"other scheme", "ws://h.test/mcp", "", "unsupported URL scheme: ws"},
		{"no host", "http:///mcp", "", "invalid target URL: missing host"},
		{"password", "http://u:p@h.test/mcp", "", "authentication in target URL is not supported"},
		{"broken authority", "http://[::1", "", "invalid target URL: "},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			u, err := proxyTarget(tc.raw)
			if tc.wantErr != "" {
				if err == nil || !strings.HasPrefix(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want one starting with %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got := u.String(); got != tc.wantURL {
				t.Errorf("target = %q, want %q", got, tc.wantURL)
			}
		})
	}
}

// TestForwardHeadersTable pins what may leave this host.
func TestForwardHeadersTable(t *testing.T) {
	cases := []struct {
		name string
		src  map[string][]string
		want map[string][]string
	}{
		{
			"renamed",
			map[string][]string{"X-Llama-Server-Proxy-header-Accept": {"application/json"}},
			map[string][]string{"Accept": {"application/json"}},
		},
		{
			"values kept apart",
			map[string][]string{"x-llama-server-proxy-header-x-note": {"one", "two"}},
			map[string][]string{"X-Note": {"one", "two"}},
		},
		{
			"caller headers dropped",
			map[string][]string{
				"Cookie":        {"session=secret"},
				"Authorization": {"Bearer local"},
				"Content-Type":  {"text/plain"},
			},
			map[string][]string{},
		},
		{
			"empty name dropped",
			map[string][]string{"x-llama-server-proxy-header-": {"value"}},
			map[string][]string{},
		},
		{
			"host dropped: the transport writes the target host",
			map[string][]string{"x-llama-server-proxy-header-host": {"elsewhere.test"}},
			map[string][]string{},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dst := http.Header{}
			forwardHeaders(dst, tc.src)
			if len(dst) != len(tc.want) {
				t.Fatalf("forwarded %v, want %v", dst, tc.want)
			}
			for name, values := range tc.want {
				got := dst.Values(name)
				if strings.Join(got, ",") != strings.Join(values, ",") {
					t.Errorf("%s = %v, want %v", name, got, values)
				}
			}
		})
	}
}

// A username in the target must not come back as a Basic header upstream, which
// is what Go would build from the URL userinfo.
func TestProxyTargetUserSendsNoAuthorization(t *testing.T) {
	u := newUpstream(t, http.StatusOK, "ok", "text/plain")
	target := "http://someone@" + strings.TrimPrefix(u.server.URL, "http://") + "/mcp"

	rec := proxyReq(t, NewProxy([]string{"http://ui.test"}, testLog(t)), http.MethodGet, target, "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body %s", rec.Code, rec.Body)
	}
	_, headers, _, path := u.snapshot()
	if path != "/mcp" {
		t.Fatalf("upstream path = %q", path)
	}
	if got := headers.Get("Authorization"); got != "" {
		t.Errorf("the target username leaked as %q", got)
	}
}

// NewProxy with no logger still works: the caller may pass nil.
func TestProxyNilLogger(t *testing.T) {
	if p := NewProxy([]string{"*"}, nil); p == nil {
		t.Fatal("an allow list must give a Proxy even without a logger")
	}
}

// A preflight that names no headers still gets the prefix back, so the browser
// can send a request with a proxy header it did not announce.
func TestProxyPreflightDefaults(t *testing.T) {
	rec := proxyReq(t, NewProxy([]string{"*"}, testLog(t)), http.MethodOptions, "http://target.test/mcp", "", nil)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rec.Code)
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "*" {
		t.Errorf("allow origin = %q, want *", got)
	}
	if got := rec.Header().Get("Access-Control-Allow-Headers"); !strings.Contains(got, proxyHeaderPrefix) {
		t.Errorf("allow headers = %q, want the %s prefix", got, proxyHeaderPrefix)
	}
}

// A body that stops arriving is a bad request, and nothing leaves this host.
func TestProxyBodyReadError(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/cors-proxy?url=http://target.test/mcp", nil)
	r.Body = brokenBody{}
	rec := newFlushRecorder()
	NewProxy([]string{"http://ui.test"}, testLog(t)).Handle(rec, r)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body %s", rec.Code, rec.Body)
	}
	if !strings.HasPrefix(rec.Body.String(), `{"error":{"message":"failed to read request body: `) {
		t.Errorf("body = %s", rec.Body)
	}
}

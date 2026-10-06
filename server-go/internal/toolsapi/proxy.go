package toolsapi

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Proxy timeouts and prefixes copied from server-cors-proxy.h.
const (
	// proxyHeaderPrefix names a header the caller wants forwarded. Everything
	// else of the client's headers stays on this side, so cookies and the local
	// API key cannot leak to a third-party host.
	proxyHeaderPrefix = "x-llama-server-proxy-header-"

	// proxyTimeout is the 600 s read/write bound llama-server gives the relay.
	proxyTimeout = 600 * time.Second
)

// Proxy is the CORS relay the browser uses to reach remote MCP servers.
//
// It is reached as GET or POST /cors-proxy?url=<target>. Only the origins given
// to NewProxy get CORS headers back.
type Proxy struct {
	allowed map[string]bool
	all     bool
	log     Log
	client  *http.Client
}

// NewProxy returns nil when allowed is empty: the caller then drops the route
// or lets Handle answer 403, which is what llama-server does with
// --ui-mcp-proxy off.
func NewProxy(allowed []string, logf Log) *Proxy {
	if len(allowed) == 0 {
		return nil
	}
	if logf == nil {
		logf = func(string, ...any) {}
	}
	p := &Proxy{allowed: map[string]bool{}, log: logf, client: &http.Client{}}
	for _, o := range allowed {
		if o == "*" {
			p.all = true
			continue
		}
		p.allowed[o] = true
	}
	return p
}

// Handle serves GET and POST /cors-proxy. A nil Proxy answers 403, so a caller
// can wire the route unconditionally.
func (p *Proxy) Handle(w http.ResponseWriter, r *http.Request) {
	if p == nil {
		writeDisabled(w)
		return
	}

	p.cors(w, r)

	switch r.Method {
	case http.MethodGet, http.MethodPost:
	case http.MethodOptions:
		// A cross-origin POST with a Content-Type header needs a preflight.
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		if req := r.Header.Get("Access-Control-Request-Headers"); req != "" {
			w.Header().Set("Access-Control-Allow-Headers", req)
		} else {
			w.Header().Set("Access-Control-Allow-Headers", proxyHeaderPrefix+"*, Content-Type")
		}
		w.WriteHeader(http.StatusNoContent)
		return
	default:
		// llama-server only registers GET and POST for this path.
		p.fail(w, r, http.StatusMethodNotAllowed, kindInvalidRequest, "method not allowed: "+r.Method)
		return
	}

	target, err := proxyTarget(r.URL.Query().Get("url"))
	if err != nil {
		p.fail(w, r, http.StatusBadRequest, kindInvalidRequest, err.Error())
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		p.fail(w, r, http.StatusBadRequest, kindInvalidRequest, "failed to read request body: "+err.Error())
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), proxyTimeout)
	defer cancel()

	up, err := http.NewRequestWithContext(ctx, r.Method, target.String(), bytes.NewReader(body))
	if err != nil {
		p.fail(w, r, http.StatusBadRequest, kindInvalidRequest, err.Error())
		return
	}
	forwardHeaders(up.Header, r.Header)

	p.log("proxying %s request to %s", r.Method, target.Redacted())

	res, err := p.client.Do(up)
	if err != nil {
		// ctx.Err() is set when the client left or the exchange ran past the
		// bound. Either way the upstream request is already cancelled.
		if ctxErr := ctx.Err(); ctxErr != nil {
			p.log("proxy request to %s cancelled: %v", target.Redacted(), ctxErr)
			return
		}
		p.fail(w, r, http.StatusInternalServerError, kindServer, "proxy request failed: "+err.Error())
		return
	}
	defer res.Body.Close()

	copyProxyHeaders(w.Header(), res.Header)
	w.WriteHeader(res.StatusCode)

	// A stream from the upstream (an MCP server using SSE) has to reach the
	// browser as it arrives, so every read is flushed. Copying stops on its own
	// when ctx is cancelled, because res.Body errors out.
	fl, _ := w.(http.Flusher)
	buf := make([]byte, 32*1024)
	for {
		n, rerr := res.Body.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				p.log("proxy write to %s failed: %v", target.Redacted(), werr)
				return
			}
			if fl != nil {
				fl.Flush()
			}
		}
		if rerr != nil {
			if rerr != io.EOF {
				p.log("proxy read from %s failed: %v", target.Redacted(), rerr)
			}
			return
		}
	}
}

// proxyTarget parses the url parameter. The rules and their wording
// follow proxy_request() in server-cors-proxy.h.
func proxyTarget(raw string) (*url.URL, error) {
	if !strings.Contains(raw, "://") {
		return nil, fmt.Errorf("invalid URL: no scheme")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("invalid target URL: %v", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("unsupported URL scheme: %s", u.Scheme)
	}
	if u.Hostname() == "" {
		return nil, fmt.Errorf("invalid target URL: missing host")
	}
	if _, hasPassword := u.User.Password(); hasPassword {
		return nil, fmt.Errorf("authentication in target URL is not supported")
	}
	// llama-server keeps only the scheme, host, port and path: a username here
	// would otherwise turn into a Basic header that no caller asked for.
	u.User = nil
	if u.Path == "" {
		u.Path = "/"
	}
	return u, nil
}

// forwardHeaders copies the prefixed headers, renamed. Nothing else of the
// caller's headers goes out: a cookie or the local API key cannot reach a
// third-party host. A caller that wants Content-Type upstream sends it under the
// prefix, like the webui does for every MCP header.
func forwardHeaders(dst, src http.Header) {
	for name, values := range src {
		lower := strings.ToLower(name)
		if !strings.HasPrefix(lower, proxyHeaderPrefix) {
			continue
		}
		renamed := name[len(proxyHeaderPrefix):]
		// the transport writes the target host, as llama-server forces it
		if renamed == "" || strings.EqualFold(renamed, "host") {
			continue
		}
		for _, v := range values {
			dst.Add(renamed, v)
		}
	}
}

// stripProxyHeaders are the response headers llama-server drops before handing
// an upstream answer back, because the local layer sets its own copy.
var stripProxyHeaders = []string{
	"server",
	"transfer-encoding",
	"content-length",
	"keep-alive",
}

func copyProxyHeaders(dst, src http.Header) {
	for name, values := range src {
		lower := strings.ToLower(name)
		if strings.HasPrefix(lower, "access-control-") {
			continue // CORS is set here, from the caller's Origin
		}
		skip := false
		for _, drop := range stripProxyHeaders {
			if lower == drop {
				skip = true
				break
			}
		}
		if skip || lower == "content-type" {
			continue
		}
		for _, v := range values {
			dst.Add(name, v)
		}
	}
	if ct := src.Get("Content-Type"); ct != "" {
		dst.Set("Content-Type", ct)
	}
}

// cors echoes the caller's Origin when it is allowed. A "*" list allows any
// origin, and an origin is echoed for it too so a credentialed fetch still works.
func (p *Proxy) cors(w http.ResponseWriter, r *http.Request) {
	origin := r.Header.Get("Origin")
	if origin == "" {
		if p.all {
			w.Header().Set("Access-Control-Allow-Origin", "*")
		}
		return
	}
	if p.all || p.allowed[origin] {
		w.Header().Set("Access-Control-Allow-Origin", origin)
	}
}

// fail answers with the wrapped error body llama-server's exception wrapper
// writes for a handler that throws, which is how the proxy route reports.
func (p *Proxy) fail(w http.ResponseWriter, r *http.Request, code int, kind, message string) {
	p.log("cors-proxy: %s", message)
	writeWrapped(w, code, message, kind, code)
}

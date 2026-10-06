package toolsapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// The /tools endpoint holds one merged registry for its whole life, so a tool an
// MCP server revokes must stop answering through the HTTP handler without a
// restart, and a tool it adds must start answering.
func TestHTTPToolsServeTheLiveMergedView(t *testing.T) {
	builtIn := newSet("read_file")
	mcpSet := newSet("weather")
	a := New(Merge(builtIn, mcpSet), testLog(t))

	rec := post(t, a, `{"tool":"weather","params":{}}`, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s, want the tool to run", rec.Code, rec.Body.String())
	}

	// The MCP server sends notifications/tools/list_changed with weather gone.
	dropTool(t, mcpSet, "weather")

	rec = post(t, a, `{"tool":"weather","params":{}}`, nil)
	wantBody(t, rec, http.StatusNotFound,
		`{"code":400,"message":"unknown tool \"weather\"","type":"invalid_request_error"}`)

	rec = get(t, a)
	// The old snapshot needed a rebuild here, and nothing did one. Building the
	// merge again now says exactly what the live registry already says.
	fresh := toolNames(Merge(builtIn, mcpSet).List())
	if live := toolNames(a.Tools().List()); !equalNames(fresh, live) {
		t.Errorf("a fresh merge lists %v, the live one lists %v", fresh, live)
	}
	listed := rec.Body.String()
	if strings.Contains(listed, "weather") {
		t.Errorf("GET /tools still advertises the revoked tool: %s", listed)
	}
	if !strings.Contains(listed, "read_file") {
		t.Errorf("GET /tools lost the built-in tool: %s", listed)
	}

	// A new tool from the same server is reachable on the next request.
	mcpSet.add(taggedTool("stocks", "from-mcp"))
	rec = post(t, a, `{"tool":"stocks","params":{}}`, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s, want the new tool to run", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "from-mcp") {
		t.Errorf("body = %s, want the MCP copy to answer", rec.Body.String())
	}
}

// A body over the cap is refused with the answer the handler already gives for
// an unreadable body. The read stops at the cap, so a client cannot make this
// server allocate a body it will never use.
func TestCallBodyOverTheCapIsRefused(t *testing.T) {
	s := newSet("read_file")
	tl := s.tool(t, "read_file")
	a := New(s, testLog(t))

	r := httptest.NewRequest(http.MethodPost, "/tools", endlessBody{chunk: make([]byte, 64*1024)})
	r.Header.Set("Content-Type", "application/json")
	rec := newFlushRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		a.HandleCall(rec, r)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the handler kept reading an oversized body")
	}

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400, body = %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"type":"invalid_request_error"`) {
		t.Errorf("body = %s, want the invalid request error shape", body)
	}
	if !strings.Contains(body, "failed to read request body") {
		t.Errorf("body = %s, want the unreadable body wording", body)
	}
	if n := tl.nCalls(); n != 0 {
		t.Errorf("the tool ran %d times, want it never called", n)
	}

	// The same handler still takes a body of a sane size.
	rec = post(t, a, `{"tool":"read_file","params":{}}`, nil)
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d after a normal call, body = %s", rec.Code, rec.Body.String())
	}
}

// The relay caps its body the same way, at its own size.
func TestRelayBodyOverTheCapIsRefused(t *testing.T) {
	u := newUpstream(t, http.StatusOK, "should never be reached", "text/plain")
	p := NewProxy([]string{"http://ui.test"}, testLog(t))

	target := "http://" + strings.TrimPrefix(u.server.URL, "http://") + "/mcp"
	r := httptest.NewRequest(http.MethodPost, "/cors-proxy?url="+target, endlessBody{chunk: make([]byte, 64*1024)})
	r.Header.Set("Content-Type", "application/json")
	rec := newFlushRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		p.Handle(rec, r)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the relay kept reading an oversized body")
	}

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400, body = %s", rec.Code, rec.Body.String())
	}
	if got := rec.Body.String(); !strings.Contains(got, "failed to read request body") {
		t.Errorf("body = %s, want the unreadable body wording", got)
	}
	if n := u.nHits(); n != 0 {
		t.Errorf("the upstream was asked %d times, want it left alone", n)
	}
}

// One stalled upstream must not be able to grow the relay count without bound:
// at capacity a request is refused on the spot, and the slots come back when the
// relays end.
func TestRelayCapacityRefusesInsteadOfQueuing(t *testing.T) {
	release := make(chan struct{})
	reached := make(chan struct{})
	var parked int32
	var once sync.Once

	// The upstream holds every request it gets until the test lets go, so the
	// relays stay inside Handle with their slot taken.
	u := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&parked, 1) == relayCapacity {
			once.Do(func() { close(reached) })
		}
		<-release
		atomic.AddInt32(&parked, -1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(u.Close)

	p := NewProxy([]string{"http://ui.test"}, silentLog)
	target := "http://" + strings.TrimPrefix(u.URL, "http://") + "/mcp"

	for i := 0; i < relayCapacity; i++ {
		go func() {
			rec := newFlushRecorder()
			p.Handle(rec, relayRequest(target))
		}()
	}
	select {
	case <-reached:
	case <-time.After(15 * time.Second):
		t.Fatalf("only %d of %d relays reached the upstream", atomic.LoadInt32(&parked), relayCapacity)
	}

	// The next request is refused rather than queued behind the stalled ones.
	rec := newFlushRecorder()
	started := time.Now()
	p.Handle(rec, relayRequest(target))
	if spent := time.Since(started); spent > time.Second {
		t.Errorf("the 33rd request waited %s, want an immediate refusal", spent)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500, body = %s", rec.Code, rec.Body.String())
	}
	if got := rec.Body.String(); !strings.Contains(got, "proxy at capacity") ||
		!strings.Contains(got, `"type":"server_error"`) {
		t.Errorf("body = %s, want the server error answer with the capacity message", got)
	}

	// The stalled upstream lets go, the relays end, and the slots are free again.
	close(release)
	deadline := time.Now().Add(10 * time.Second)
	for {
		rec := newFlushRecorder()
		p.Handle(rec, relayRequest(target))
		if rec.Code == http.StatusOK {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the relay never came back to life, last body = %s", rec.Body.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// silentLog keeps a relay that outlives the test body from logging into it.
func silentLog(string, ...any) {}

// relayRequest is one POST the browser would send to the relay.
func relayRequest(target string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/cors-proxy?url="+target,
		strings.NewReader(`{"jsonrpc":"2.0"}`))
	r.Header.Set("Content-Type", "application/json")
	return r
}

// endlessBody returns the same chunk forever, standing in for a client that
// keeps sending. A capped reader has to stop it.
type endlessBody struct{ chunk []byte }

func (e endlessBody) Read(p []byte) (int, error) { return copy(p, e.chunk), nil }

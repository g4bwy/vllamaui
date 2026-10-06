package toolsapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"llama-webui/server/internal/contracts"
)

// fakeTool records the requests it was handed and does what fn says.
type fakeTool struct {
	info contracts.ToolInfo
	fn   func(ctx context.Context, req contracts.ToolRequest, out contracts.Sink) contracts.Result

	mu    sync.Mutex
	calls []contracts.ToolRequest
}

func (tl *fakeTool) Info() contracts.ToolInfo { return tl.info }

func (tl *fakeTool) Invoke(ctx context.Context, req contracts.ToolRequest, out contracts.Sink) contracts.Result {
	tl.mu.Lock()
	tl.calls = append(tl.calls, req)
	tl.mu.Unlock()
	if tl.fn == nil {
		return contracts.Result{PlainText: "ok"}
	}
	return tl.fn(ctx, req, out)
}

func (tl *fakeTool) lastCall(t *testing.T) contracts.ToolRequest {
	t.Helper()
	tl.mu.Lock()
	defer tl.mu.Unlock()
	if len(tl.calls) == 0 {
		t.Fatal("the tool was not called")
	}
	return tl.calls[len(tl.calls)-1]
}

func (tl *fakeTool) nCalls() int {
	tl.mu.Lock()
	defer tl.mu.Unlock()
	return len(tl.calls)
}

func toolInfo(name string) contracts.ToolInfo {
	var info contracts.ToolInfo
	info.DisplayName = "Display " + name
	info.Tool = name
	info.Type = "server"
	info.UsesCwd = true
	info.Definition = json.RawMessage(`{"type":"function"}`)
	return info
}

func echoTool(name string) *fakeTool {
	return &fakeTool{
		info: toolInfo(name),
		fn: func(context.Context, contracts.ToolRequest, contracts.Sink) contracts.Result {
			return contracts.Result{PlainText: "ran " + name}
		},
	}
}

// set is a registry of fakes. stream declares which names may stream. A set
// wrapped in quiet says nothing about the capability.
type set struct {
	tools  map[string]*fakeTool
	order  []string
	stream map[string]bool
}

func newSet(names ...string) *set {
	s := &set{tools: map[string]*fakeTool{}, stream: map[string]bool{}}
	for _, n := range names {
		s.add(&fakeTool{info: toolInfo(n)})
	}
	return s
}

func (s *set) add(tools ...*fakeTool) *set {
	for _, tl := range tools {
		s.tools[tl.info.Tool] = tl
		s.order = append(s.order, tl.info.Tool)
	}
	return s
}

func (s *set) List() []contracts.ToolInfo {
	out := make([]contracts.ToolInfo, 0, len(s.order))
	for _, n := range s.order {
		out = append(out, s.tools[n].Info())
	}
	return out
}

func (s *set) Get(name string) (contracts.Tool, bool) {
	tl, ok := s.tools[name]
	return tl, ok
}

func (s *set) SupportsStream(name string) bool { return s.stream[name] }

func (s *set) tool(t *testing.T, name string) *fakeTool {
	t.Helper()
	tl, ok := s.tools[name]
	if !ok {
		t.Fatalf("the registry has no tool %q", name)
	}
	return tl
}

// quiet hides SupportsStream, so the fallback rule of the HTTP layer applies.
type quiet struct{ s *set }

func (q quiet) List() []contracts.ToolInfo             { return q.s.List() }
func (q quiet) Get(name string) (contracts.Tool, bool) { return q.s.Get(name) }

// empty holds no tools.
type empty struct{}

func (empty) List() []contracts.ToolInfo        { return nil }
func (empty) Get(string) (contracts.Tool, bool) { return nil, false }

func testLog(t *testing.T) Log {
	return func(format string, v ...any) { t.Logf(format, v...) }
}

func post(t *testing.T, a *API, body string, headers map[string]string) *flushRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/tools", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	rec := newFlushRecorder()
	a.HandleCall(rec, r)
	return rec
}

func get(t *testing.T, a *API) *flushRecorder {
	t.Helper()
	rec := newFlushRecorder()
	a.HandleList(rec, httptest.NewRequest(http.MethodGet, "/tools", nil))
	return rec
}

func wantBody(t *testing.T, rec *flushRecorder, status int, body string) {
	t.Helper()
	if rec.Code != status {
		t.Errorf("status = %d, want %d, body = %s", rec.Code, status, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); got != jsonType {
		t.Errorf("content type = %q, want %q", got, jsonType)
	}
	if got := rec.Body.String(); got != body {
		t.Errorf("body = %s, want %s", got, body)
	}
}

func TestNewNilForNoTools(t *testing.T) {
	if a := New(empty{}, testLog(t)); a != nil {
		t.Error("New must return nil when the registry holds no tools")
	}
	if a := New(nil, testLog(t)); a != nil {
		t.Error("New must return nil for a nil registry")
	}

	// The caller's helper for a route llama-server did not register.
	disabled := `{"error":{"message":"this feature is disabled","type":"feature_disabled"}}`
	rec := newFlushRecorder()
	(*API)(nil).HandleList(rec, httptest.NewRequest(http.MethodGet, "/tools", nil))
	wantBody(t, rec, http.StatusForbidden, disabled)

	rec = post(t, nil, `{}`, nil)
	wantBody(t, rec, http.StatusForbidden, disabled)

	if (*API)(nil).Tools() != nil {
		t.Error("Tools of a nil API must be nil")
	}
}

func TestHandleListSorted(t *testing.T) {
	s := newSet()
	s.add(&fakeTool{info: toolInfo("zebra")}, &fakeTool{info: toolInfo("alpha")}, &fakeTool{info: toolInfo("mcp_beta")})
	// builtin puts exec_shell_command fourth, so its place here proves the sort.
	s.add(&fakeTool{info: toolInfo(execShellCommand)})

	rec := get(t, New(s, testLog(t)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var infos []contracts.ToolInfo
	if err := json.Unmarshal(rec.Body.Bytes(), &infos); err != nil {
		t.Fatalf("body is not a tool array: %s (%v)", rec.Body, err)
	}
	want := []string{"alpha", execShellCommand, "mcp_beta", "zebra"}
	if len(infos) != len(want) {
		t.Fatalf("got %d tools, want %d: %s", len(infos), len(want), rec.Body)
	}
	for i, name := range want {
		if infos[i].Tool != name {
			t.Errorf("tool %d = %q, want %q", i, infos[i].Tool, name)
		}
	}
	// One element, in the field order llama-server writes.
	wantElem := `{"display_name":"Display alpha","tool":"alpha","type":"server","permissions":{"write":false},"uses_cwd":true,"definition":{"type":"function"}}`
	if !strings.Contains(rec.Body.String(), wantElem) {
		t.Errorf("body must contain %s, got %s", wantElem, rec.Body)
	}
}

func TestHandleListEmptyIsArray(t *testing.T) {
	// New returns nil for a registry without tools, so the only way to reach the
	// list with nothing in it is a registry that changed under us.
	rec := get(t, &API{reg: empty{}, log: testLog(t)})
	wantBody(t, rec, http.StatusOK, `[]`)
}

func TestHandleCallHappyPath(t *testing.T) {
	s := newSet("read_file")
	a := New(s, testLog(t))

	rec := post(t, a, `{"tool":"read_file","params":{"path":"a.txt","start_line":2}}`, nil)
	wantBody(t, rec, http.StatusOK, `{"plain_text_response":"ok"}`)

	got := s.tool(t, "read_file").lastCall(t)
	if got.Name != "read_file" || got.Params["path"] != "a.txt" || got.Stream {
		t.Errorf("request = %+v", got)
	}

	// params defaults to an empty object, stream to false.
	rec = post(t, a, `{"tool":"read_file"}`, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body)
	}
	got = s.tool(t, "read_file").lastCall(t)
	if got.Params == nil || len(got.Params) != 0 {
		t.Errorf("params must default to an empty object, got %#v", got.Params)
	}
}

func TestHandleCallBadBodies(t *testing.T) {
	a := New(newSet("read_file"), testLog(t))

	cases := []struct {
		name string
		body string
		want string
	}{
		{"not json", `{"tool": `, `{"code":400,"message":"invalid JSON in request body: `},
		{"array", `[1,2]`, `{"code":400,"message":"request body must be a JSON object, but is array","type":"invalid_request_error"}`},
		{"string", `"read_file"`, `{"code":400,"message":"request body must be a JSON object, but is string","type":"invalid_request_error"}`},
		{"no body", ``, `{"code":400,"message":"invalid JSON in request body: `},
		{"missing tool", `{"params":{}}`, `{"code":400,"message":"key 'tool' not found","type":"invalid_request_error"}`},
		{"null tool", `{"tool":null}`, `{"code":400,"message":"type must be string, but is null","type":"invalid_request_error"}`},
		{"tool not a string", `{"tool":7}`, `{"code":400,"message":"type must be string, but is number","type":"invalid_request_error"}`},
		{"params not an object", `{"tool":"read_file","params":"x"}`, `{"code":400,"message":"type must be object, but is string","type":"invalid_request_error"}`},
		{"stream not a bool", `{"tool":"read_file","stream":"yes"}`, `{"code":400,"message":"type must be boolean, but is string","type":"invalid_request_error"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := post(t, a, tc.body, nil)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400, body %s", rec.Code, rec.Body)
			}
			if got := rec.Header().Get("Content-Type"); got != jsonType {
				t.Errorf("content type = %q", got)
			}
			if !strings.HasPrefix(rec.Body.String(), tc.want) {
				t.Errorf("body = %s, want prefix %s", rec.Body, tc.want)
			}
			var flat flatError
			if err := json.Unmarshal(rec.Body.Bytes(), &flat); err != nil {
				t.Fatalf("body is not a flat error: %s", rec.Body)
			}
			if flat.Code != 400 || flat.Type != kindInvalidRequest {
				t.Errorf("body = %+v", flat)
			}
		})
	}
}

func TestHeaderPrecedence(t *testing.T) {
	s := newSet("read_file")
	a := New(s, testLog(t))

	rec := post(t, a,
		`{"tool":"read_file","params":{"cwd":"/model/chose","resp_type":"model","runtime":"docker:model","path":"a.txt"}}`,
		map[string]string{"x-tool-cwd": "/caller/chose", "x-resp-type": "base64"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body)
	}

	got := s.tool(t, "read_file").lastCall(t)
	if got.Cwd != "/caller/chose" {
		t.Errorf("cwd = %q, want the header value", got.Cwd)
	}
	if got.RespType != "base64" {
		t.Errorf("resp_type = %q, want the header value", got.RespType)
	}
	for _, key := range []string{"cwd", "resp_type", "runtime"} {
		if _, present := got.Params[key]; present {
			t.Errorf("params must not carry %q to the tool: %#v", key, got.Params)
		}
	}
	if got.Params["path"] != "a.txt" {
		t.Errorf("other params must survive: %#v", got.Params)
	}

	// A header with no value leaves the field empty, as in llama-server.
	rec = post(t, a, `{"tool":"read_file"}`, nil)
	got = s.tool(t, "read_file").lastCall(t)
	if got.Cwd != "" || got.RespType != "" {
		t.Errorf("absent headers must leave the fields empty: %+v", got)
	}
}

func TestRuntimeIsRefused(t *testing.T) {
	s := newSet("read_file")
	a := New(s, testLog(t))

	rec := post(t, a, `{"tool":"read_file"}`, map[string]string{"x-tool-runtime": "ssh:host"})
	wantBody(t, rec, http.StatusInternalServerError,
		`{"code":500,"message":"tool runtime is not supported by this server","type":"server_error"}`)
	if n := s.tool(t, "read_file").nCalls(); n != 0 {
		t.Errorf("a refused runtime must not reach the tool, calls = %d", n)
	}

	// llama-server looks the tool up before it resolves the runtime, so a request
	// that is wrong in both ways reports the tool first.
	rec = post(t, a, `{"tool":"nope"}`, map[string]string{"x-tool-runtime": "ssh:host"})
	wantBody(t, rec, http.StatusNotFound,
		`{"code":400,"message":"unknown tool \"nope\"","type":"invalid_request_error"}`)

	rec = post(t, a, `{"tool":"read_file","stream":true}`, map[string]string{"x-tool-runtime": "ssh:host"})
	wantBody(t, rec, http.StatusNotFound,
		`{"code":400,"message":"tool \"read_file\" does not support stream = true","type":"invalid_request_error"}`)
}

// A runtime the model put in params is not a runtime request: the key is dropped
// and the tool runs on the host, as it does in llama-server without an isolate.
func TestRuntimeInParamsIsDropped(t *testing.T) {
	s := newSet("read_file")
	rec := post(t, New(s, testLog(t)), `{"tool":"read_file","params":{"runtime":"docker:img","path":"a.txt"}}`, nil)
	wantBody(t, rec, http.StatusOK, `{"plain_text_response":"ok"}`)

	got := s.tool(t, "read_file").lastCall(t)
	if _, present := got.Params["runtime"]; present {
		t.Errorf("params must not carry runtime to the tool: %#v", got.Params)
	}
}

func TestUnknownToolAndStreamCapability(t *testing.T) {
	s := newSet("read_file", execShellCommand)
	s.stream[execShellCommand] = true
	a := New(s, testLog(t))

	rec := post(t, a, `{"tool":"nope"}`, nil)
	wantBody(t, rec, http.StatusNotFound,
		`{"code":400,"message":"unknown tool \"nope\"","type":"invalid_request_error"}`)

	rec = post(t, a, `{"tool":"read_file","stream":true}`, nil)
	wantBody(t, rec, http.StatusNotFound,
		`{"code":400,"message":"tool \"read_file\" does not support stream = true","type":"invalid_request_error"}`)

	// A registry that says nothing falls back to the llama-server rule: only
	// exec_shell_command can stream.
	quietAPI := New(quiet{s}, testLog(t))
	rec = post(t, quietAPI, `{"tool":"read_file","stream":true}`, nil)
	if rec.Code != http.StatusNotFound {
		t.Errorf("quiet registry: status = %d, body %s", rec.Code, rec.Body)
	}
	rec = post(t, quietAPI, `{"tool":"`+execShellCommand+`","stream":true}`, nil)
	if rec.Code != http.StatusOK {
		t.Errorf("quiet registry: exec must stream, got %d %s", rec.Code, rec.Body)
	}
}

func TestPanickingTool(t *testing.T) {
	s := newSet().add(&fakeTool{
		info: toolInfo("boom"),
		fn: func(context.Context, contracts.ToolRequest, contracts.Sink) contracts.Result {
			panic("tool exploded")
		},
	})
	rec := post(t, New(s, testLog(t)), `{"tool":"boom"}`, nil)
	wantBody(t, rec, http.StatusInternalServerError,
		`{"code":500,"message":"tool \"boom\" panicked","type":"server_error"}`)
}

func TestToolErrorResult(t *testing.T) {
	s := newSet().add(&fakeTool{
		info: toolInfo("read_file"),
		fn: func(context.Context, contracts.ToolRequest, contracts.Sink) contracts.Result {
			return contracts.Result{Error: "cannot stat file: /nope"}
		},
	})
	// llama-server answers 200 and puts the failure in the body: the webui reads
	// the error field first.
	rec := post(t, New(s, testLog(t)), `{"tool":"read_file"}`, nil)
	wantBody(t, rec, http.StatusOK, `{"error":"cannot stat file: /nope"}`)
}

func TestToolBodyResult(t *testing.T) {
	s := newSet().add(&fakeTool{
		info: toolInfo("get_info"),
		fn: func(context.Context, contracts.ToolRequest, contracts.Sink) contracts.Result {
			return contracts.Result{Body: map[string]any{"cwd": "/tmp/a&b", "os": "linux"}}
		},
	})
	rec := post(t, New(s, testLog(t)), `{"tool":"get_info"}`, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body)
	}
	// The webui reads the body map for a structured answer. Result.JSON() in the
	// contracts package renders it, so decode it back here.
	var out map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("body = %s (%v)", rec.Body, err)
	}
	if out["cwd"] != "/tmp/a&b" || out["os"] != "linux" {
		t.Errorf("decoded body = %v", out)
	}
}

func TestStreamingFrames(t *testing.T) {
	s := newSet().add(&fakeTool{
		info: toolInfo(execShellCommand),
		fn: func(_ context.Context, _ contracts.ToolRequest, out contracts.Sink) contracts.Result {
			out("one ")
			out("") // an empty chunk must never reach the wire
			out("two <b>&</b>")
			out("three")
			return contracts.Result{}
		},
	})
	s.stream[execShellCommand] = true

	rec := post(t, New(s, testLog(t)), `{"tool":"`+execShellCommand+`","stream":true}`, nil)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); got != sseType {
		t.Errorf("content type = %q, want %q", got, sseType)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-cache" {
		t.Errorf("cache control = %q", got)
	}

	want := "data: {\"chunk\":\"one \"}\n\n" +
		"data: {\"chunk\":\"two <b>&</b>\"}\n\n" +
		"data: {\"chunk\":\"three\"}\n\n" +
		"data: {\"done\":true}\n\n"
	if got := rec.Body.String(); got != want {
		t.Errorf("frames = %q, want %q", got, want)
	}
	if strings.Contains(rec.Body.String(), "[DONE]") {
		t.Error("this route has no [DONE] sentinel")
	}
	// One write per frame, so a browser sees them as they come.
	if got := len(rec.writes); got != 4 {
		t.Errorf("writes = %d, want 4 (one per frame)", got)
	}
	for i, n := range rec.writes {
		if n == 0 {
			t.Errorf("write %d is empty", i)
		}
		if i < len(rec.bodies) && !strings.HasSuffix(rec.bodies[i], "\n\n") {
			t.Errorf("write %d does not end a frame: %q", i, rec.bodies[i])
		}
	}
	// The headers are flushed first so the stream opens, then once per frame: a
	// browser reads a frame as it is produced and not when the handler returns.
	if got := len(rec.atFlush); got != len(rec.writes)+1 {
		t.Fatalf("flushes = %d, want %d (headers plus one per frame)", got, len(rec.writes)+1)
	}
	if got := rec.atFlush[0]; got != 0 {
		t.Errorf("the first flush carried %d body bytes, want 0 (the headers)", got)
	}
	seen := 0
	for i, n := range rec.writes {
		seen += n
		if rec.atFlush[i+1] != seen {
			t.Errorf("flush after frame %d is at byte %d, want %d", i, rec.atFlush[i+1], seen)
		}
	}
}

func TestStreamingErrorFrame(t *testing.T) {
	s := newSet().add(&fakeTool{
		info: toolInfo(execShellCommand),
		fn: func(_ context.Context, _ contracts.ToolRequest, out contracts.Sink) contracts.Result {
			out("partial")
			return contracts.Result{Error: "command timed out"}
		},
	})
	s.stream[execShellCommand] = true

	rec := post(t, New(s, testLog(t)), `{"tool":"`+execShellCommand+`","stream":true}`, nil)
	want := "data: {\"chunk\":\"partial\"}\n\ndata: {\"done\":true,\"error\":\"command timed out\"}\n\n"
	if got := rec.Body.String(); got != want {
		t.Errorf("frames = %q, want %q", got, want)
	}
}

func TestStreamingPanicFrame(t *testing.T) {
	s := newSet().add(&fakeTool{
		info: toolInfo(execShellCommand),
		fn: func(_ context.Context, _ contracts.ToolRequest, out contracts.Sink) contracts.Result {
			out("before")
			panic("bad tool")
		},
	})
	s.stream[execShellCommand] = true

	rec := post(t, New(s, testLog(t)), `{"tool":"`+execShellCommand+`","stream":true}`, nil)
	want := "data: {\"chunk\":\"before\"}\n\n" +
		"data: {\"done\":true,\"error\":\"tool \\\"exec_shell_command\\\" panicked\"}\n\n"
	if got := rec.Body.String(); got != want {
		t.Errorf("frames = %q, want %q", got, want)
	}
}

func TestKeysGate(t *testing.T) {
	s := newSet("read_file")
	a := New(s, testLog(t)).Keys(func(r *http.Request) bool {
		return r.Header.Get("Authorization") == "Bearer secret"
	})

	const want401 = `{"error":{"message":"Invalid API Key","type":"authentication_error","code":401}}`

	rec := get(t, a)
	wantBody(t, rec, http.StatusUnauthorized, want401)

	rec = post(t, a, `{"tool":"read_file"}`, nil)
	wantBody(t, rec, http.StatusUnauthorized, want401)
	if n := s.tool(t, "read_file").nCalls(); n != 0 {
		t.Errorf("a rejected request must not reach the tool, calls = %d", n)
	}

	rec = httptestList(t, a, "Bearer secret")
	if rec.Code != http.StatusOK {
		t.Errorf("a good key must pass, got %d %s", rec.Code, rec.Body)
	}
	rec = post(t, a, `{"tool":"read_file"}`, map[string]string{"Authorization": "Bearer secret"})
	if rec.Code != http.StatusOK {
		t.Errorf("a good key must pass, got %d %s", rec.Code, rec.Body)
	}
	if n := s.tool(t, "read_file").nCalls(); n != 1 {
		t.Errorf("calls = %d, want 1", n)
	}
}

func httptestList(t *testing.T, a *API, key string) *flushRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, "/tools", nil)
	r.Header.Set("Authorization", key)
	rec := newFlushRecorder()
	a.HandleList(rec, r)
	return rec
}

// TestStreamStopsWhenClientLeaves tests the claim that the request context is
// enough in Go. Observation: io.ReadAll(r.Body) consumes the body to EOF, which
// arms the background read in net/http. When the client then closes the
// connection that read fails, and the server cancels r.Context() and fires the
// CloseNotifier channel. So a handler that has read its body needs no
// CloseNotifier of its own, and no write is needed to notice the disconnect.
func TestStreamStopsWhenClientLeaves(t *testing.T) {
	entered := make(chan struct{})
	cancelled := make(chan error, 1)
	s := newSet().add(&fakeTool{
		info: toolInfo(execShellCommand),
		fn: func(ctx context.Context, _ contracts.ToolRequest, out contracts.Sink) contracts.Result {
			out("first")
			<-ctx.Done()
			close(entered)
			cancelled <- ctx.Err()
			return contracts.Result{}
		},
	})
	s.stream[execShellCommand] = true

	srv := httptest.NewServer(http.HandlerFunc(New(s, testLog(t)).HandleCall))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL,
		strings.NewReader(`{"tool":"`+execShellCommand+`","stream":true}`))
	if err != nil {
		t.Fatal(err)
	}
	res, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 64)
	n, err := res.Body.Read(buf)
	if err != nil || n == 0 {
		t.Fatalf("the first frame never arrived: n=%d err=%v", n, err)
	}
	// Drop the connection without letting the server finish the stream.
	cancel()
	res.Body.Close()

	select {
	case err := <-cancelled:
		if err != context.Canceled {
			t.Errorf("ctx.Err() = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the tool context was not cancelled after the client left")
	}
	<-entered
}

// flushRecorder tracks write boundaries and flush calls.
type flushRecorder struct {
	*httptest.ResponseRecorder
	flushes int
	writes  []int
	bodies  []string
	atFlush []int
}

func newFlushRecorder() *flushRecorder {
	return &flushRecorder{ResponseRecorder: httptest.NewRecorder()}
}

func (f *flushRecorder) Write(b []byte) (int, error) {
	f.writes = append(f.writes, len(b))
	f.bodies = append(f.bodies, string(b))
	return f.ResponseRecorder.Write(b)
}

func (f *flushRecorder) Flush() {
	f.flushes++
	f.atFlush = append(f.atFlush, f.Body.Len())
}

func TestNilAPISurface(t *testing.T) {
	s := newSet("read_file")
	if a := New(s, nil); a == nil {
		t.Fatal("a registry with tools must give an API even without a logger")
	} else if a.Tools() != contracts.Registry(s) {
		t.Error("Tools must hand back the registry the API was built with")
	}
	if (*API)(nil).Keys(nil) != nil {
		t.Error("Keys of a nil API must stay nil")
	}
}

// brokenBody fails on the first read, the way a connection that dies mid-upload
// does.
type brokenBody struct{}

func (brokenBody) Read([]byte) (int, error) { return 0, context.Canceled }
func (brokenBody) Close() error             { return nil }

func TestHandleCallBodyReadError(t *testing.T) {
	a := New(newSet("read_file"), testLog(t))
	r := httptest.NewRequest(http.MethodPost, "/tools", nil)
	r.Body = brokenBody{}
	rec := newFlushRecorder()
	a.HandleCall(rec, r)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body %s", rec.Code, rec.Body)
	}
	if !strings.HasPrefix(rec.Body.String(), `{"code":400,"message":"failed to read request body: `) {
		t.Errorf("body = %s", rec.Body)
	}
}

func TestTypeNameTable(t *testing.T) {
	cases := []struct {
		value any
		want  string
	}{
		{nil, "null"},
		{true, "boolean"},
		{float64(3), "number"},
		{"x", "string"},
		{[]any{}, "array"},
		{map[string]any{}, "object"},
		{struct{}{}, "unknown"},
	}
	for _, tc := range cases {
		if got := typeName(tc.value); got != tc.want {
			t.Errorf("typeName(%#v) = %q, want %q", tc.value, got, tc.want)
		}
	}
}

// failWriter stands for a connection the browser closed.
type failWriter struct{ writes int }

func (f *failWriter) Write([]byte) (int, error) {
	f.writes++
	return 0, context.Canceled
}

// Once a frame cannot be written the stream is over: later chunks are dropped and
// the tool is cancelled, so it does not keep producing into a dead connection.
func TestStreamWriterStopsAfterAFailedWrite(t *testing.T) {
	cancelled := 0
	w := &failWriter{}
	s := &streamWriter{w: w, cancel: func() { cancelled++ }}

	s.write("first")
	if w.writes != 1 {
		t.Fatalf("writes = %d, want 1", w.writes)
	}
	if cancelled != 1 {
		t.Errorf("cancel calls = %d, want 1", cancelled)
	}
	if !s.closed {
		t.Fatal("the stream must be closed after a failed write")
	}
	s.write("second")
	s.done("")
	if w.writes != 1 {
		t.Errorf("writes after the failure = %d, want 1 (no more frames)", w.writes)
	}
}

func TestEncodeJSONFailure(t *testing.T) {
	got := string(encodeJSON(make(chan int)))
	want := `{"code":500,"message":"serialization failed","type":"server_error"}`
	if got != want {
		t.Errorf("encodeJSON of an unencodable value = %s, want %s", got, want)
	}
}

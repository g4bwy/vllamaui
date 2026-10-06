package chat

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"llama-webui/server/internal/appconf"
	"llama-webui/server/internal/backend"
)

// fakeUpstream is the engine the tests talk to. Nothing here needs a network
// service or the Node mocks.
type fakeUpstream struct {
	mu           sync.Mutex
	models       string
	version      string
	metrics      []string // successive /metrics bodies; the last one is kept
	metricAt     int
	metricsCalls int
	status       int // when set, /v1/chat/completions answers this instead
	statusBody   string
	stream       func(http.ResponseWriter, *http.Request)
	chatBody     map[string]any
}

func (f *fakeUpstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/v1/models":
		if f.models == "" {
			http.Error(w, `{"error":{"message":"no models"}}`, http.StatusNotFound)
			return
		}
		fmt.Fprint(w, f.models)
	case "/version":
		if f.version == "" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		fmt.Fprint(w, f.version)
	case "/metrics":
		f.mu.Lock()
		defer f.mu.Unlock()
		if len(f.metrics) == 0 {
			http.Error(w, "no metrics", http.StatusNotFound)
			return
		}
		i := f.metricAt
		f.metricsCalls++
		if i >= len(f.metrics) {
			i = len(f.metrics) - 1
		} else {
			f.metricAt++
		}
		fmt.Fprint(w, f.metrics[i])
	case "/v1/chat/completions":
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			body = map[string]any{}
		}
		f.mu.Lock()
		f.chatBody = body
		status := f.status
		f.mu.Unlock()
		if status != 0 {
			http.Error(w, f.statusBody, status)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		if f.stream != nil {
			f.stream(&flushWriter{w}, r)
		}
	default:
		http.Error(w, "no route "+r.URL.Path, http.StatusNotFound)
	}
}

func (f *fakeUpstream) sent() map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.chatBody
}

func (f *fakeUpstream) scrapes() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.metricsCalls
}

// flushWriter hands each chunk to the adapter as it is written, the way a real
// engine's event stream does. Without it every test chunk lands at once and the
// measured gaps say nothing.
type flushWriter struct {
	http.ResponseWriter
}

func (f *flushWriter) Write(b []byte) (int, error) {
	n, err := f.ResponseWriter.Write(b)
	if fl, ok := f.ResponseWriter.(http.Flusher); ok {
		fl.Flush()
	}
	return n, err
}

// tester holds one proxy over one fake engine, plus the log it wrote.
type tester struct {
	p    *Proxy
	d    *backend.Deps
	fake *fakeUpstream
	logs func() []string
}

func newTester(t *testing.T, id string, fake *fakeUpstream, tweak func(*appconf.Config)) *tester {
	t.Helper()
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)

	cfg := &appconf.Config{Backend: id, Upstream: srv.URL, Vision: "auto", EngineTimings: true}
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
	return &tester{p: New(b, d), d: d, fake: fake, logs: func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), lines...)
	}}
}

func (t *tester) post(body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	rec := httptest.NewRecorder()
	t.p.Complete(rec, req)
	return rec
}

func (t *tester) hasLog(part string) bool {
	for _, l := range t.logs() {
		if strings.Contains(l, part) {
			return true
		}
	}
	return false
}

func (t *tester) scrapes() int { return t.fake.scrapes() }

const modelsJSON = `{"object":"list","data":[{"id":"qwen-test","object":"model","max_model_len":32768}]}`

// dataLines pulls the payloads the browser would have seen.
func dataLines(body string) []string {
	var out []string
	for _, block := range strings.Split(body, "\n\n") {
		block = strings.TrimSpace(block)
		if strings.HasPrefix(block, "data:") {
			out = append(out, strings.TrimSpace(strings.TrimPrefix(block, "data:")))
		}
	}
	return out
}

// metricsText builds a vLLM /metrics body for the timing tests.
func metricsText(count, prefillSum, decodeSum, computedSum, running, waiting float64) string {
	return fmt.Sprintf(`vllm:request_prefill_time_seconds_count %v
vllm:request_prefill_time_seconds_sum %v
vllm:request_decode_time_seconds_count %v
vllm:request_decode_time_seconds_sum %v
vllm:request_prefill_kv_computed_tokens_count %v
vllm:request_prefill_kv_computed_tokens_sum %v
vllm:num_requests_running %v
vllm:num_requests_waiting %v
vllm:prompt_tokens_total 1000
vllm:generation_tokens_total 500
`, count, prefillSum, count, decodeSum, count, computedSum, running, waiting)
}

// sseScript answers one streamed completion in vLLM spelling.
func sseScript(w http.ResponseWriter, _ *http.Request) {
	w.Write([]byte(": keep-alive\n\n"))
	w.Write([]byte(`data: {"id":"c1","model":"qwen-test","choices":[{"index":0,"delta":{"role":"assistant","content":""}}]}` + "\n\n"))
	w.Write([]byte(`data: {"id":"c1","model":"qwen-test","choices":[{"index":0,"delta":{"reasoning":"deep thought"},"token_ids":[1,2]}]}` + "\n\n"))
	time.Sleep(4 * time.Millisecond)
	w.Write([]byte(`data: {"id":"c1","model":"qwen-test","choices":[{"index":0,"delta":{"content":" answer"},"token_ids":[3]}]}` + "\n\n"))
	w.Write([]byte(`data: {"id":"c1","model":"qwen-test","choices":[],"usage":{"prompt_tokens":10,"completion_tokens":3}}` + "\n\n"))
	w.Write([]byte("data: [DONE]\n\n"))
}

func mustJSON(t *testing.T, text string) map[string]any {
	t.Helper()
	if text == "[DONE]" {
		t.Fatal("[DONE] is not a chunk")
	}
	out, err := backend.DecodeJSON(text)
	if err != nil {
		t.Fatalf("unparsable chunk %q: %v", text, err)
	}
	return out
}

func TestStreamRewriteVLLM(t *testing.T) {
	fake := &fakeUpstream{
		models:  modelsJSON,
		version: `{"version":"0.11.0"}`,
		stream:  sseScript,
		metrics: []string{
			metricsText(10, 1.0, 0.5, 200, 0, 0),
			metricsText(11, 1.2, 0.7, 208, 0, 0),
		},
	}
	tt := newTester(t, "vllm", fake, nil)
	rec := tt.post(`{"messages":[{"role":"user","content":"hi"}],"stream":true,"n_predict":64}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Errorf("content type = %q", ct)
	}
	lines := dataLines(rec.Body.String())
	// role, thinking, text, the reordered usage chunk, then the end marker
	if len(lines) != 5 {
		t.Fatalf("want 5 data frames, got %d: %v", len(lines), lines)
	}
	done := 0
	for _, l := range lines {
		if l == "[DONE]" {
			done++
		}
	}
	if lines[len(lines)-1] != "[DONE]" {
		t.Errorf("the stream must end with [DONE], got %q", lines[len(lines)-1])
	}
	if done != 1 {
		t.Errorf("[DONE] must appear exactly once, got %d", done)
	}

	// the thinking field is renamed for the webui
	if !strings.Contains(lines[1], `"reasoning_content":"deep thought"`) {
		t.Errorf("reasoning must be renamed: %s", lines[1])
	}
	if strings.Contains(lines[1], `"reasoning":`) {
		t.Errorf("the old key must be gone: %s", lines[1])
	}

	// live timings appear once two tokens are counted
	if strings.Contains(lines[0], `"timings"`) {
		t.Errorf("a role-only chunk has nothing to time: %s", lines[0])
	}
	tim := backend.Obj(mustJSON(t, lines[2]), "timings")
	if tim == nil {
		t.Fatalf("the third chunk must carry live timings: %s", lines[2])
	}
	if got := backend.NumOr(tim, "predicted_n", 0); got != 3 {
		t.Errorf("predicted_n = %v, want the three token ids", got)
	}
	if got := backend.NumOr(tim, "predicted_ms", 0); got <= 0 {
		t.Errorf("predicted_ms = %v, want a measured gap", got)
	}
	if _, ok := tim["prompt_ms"]; ok {
		t.Errorf("a live chunk carries no prompt numbers: %v", tim)
	}

	// the usage chunk is held back and sent last, with the final numbers
	final := mustJSON(t, lines[3])
	if backend.Obj(final, "usage") == nil {
		t.Fatalf("the usage chunk must still be there: %s", lines[3])
	}
	timings := backend.Obj(final, "timings")
	if timings == nil {
		t.Fatalf("the last chunk must carry timings: %s", lines[3])
	}
	for k, want := range map[string]float64{"prompt_ms": 200, "predicted_ms": 200, "prompt_n": 8, "cache_n": 2, "predicted_n": 3} {
		if got := backend.NumOr(timings, k, -1); got != want {
			t.Errorf("%s = %v, want %v", k, got, want)
		}
	}
	if !tt.hasLog("timings(engine)") {
		t.Errorf("a clean window must be attributed to the engine, log: %v", tt.logs())
	}
	if got := tt.scrapes(); got != 2 {
		t.Errorf("the window is two forced scrapes, got %d", got)
	}

	sent := fake.sent()
	if backend.Str(sent, "model") != "qwen-test" {
		t.Errorf("the default model id must be injected, got %v", sent["model"])
	}
	if !backend.Bool(backend.Obj(sent, "stream_options"), "include_usage") {
		t.Error("stream_options.include_usage must be added")
	}
	if !backend.Bool(sent, "return_token_ids") {
		t.Error("return_token_ids must be added")
	}
	if backend.NumOr(sent, "max_tokens", 0) != 64 {
		t.Errorf("n_predict must reach the engine as max_tokens: %v", sent["max_tokens"])
	}
}

func TestStreamTailWithoutNewline(t *testing.T) {
	fake := &fakeUpstream{models: modelsJSON, stream: func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`data: {"id":"c1","choices":[{"index":0,"delta":{"content":"a"},"token_ids":[1]}]}` + "\n\n"))
		// a final data line with no newline still has to reach the client
		w.Write([]byte(`data: {"id":"c1","choices":[{"index":0,"delta":{"content":"b"},"token_ids":[2]}]}`))
		w.Write([]byte("\ndata: [DONE]\n\n"))
	}}
	tt := newTester(t, "strata", fake, nil)
	rec := tt.post(`{"messages":[],"stream":true}`)
	lines := dataLines(rec.Body.String())
	if len(lines) != 4 {
		t.Fatalf("un-terminated chunk lost: %v", lines)
	}
	if !strings.Contains(lines[1], `"content":"b"`) {
		t.Errorf("the tail chunk must be forwarded: %v", lines)
	}
	if lines[len(lines)-1] != "[DONE]" {
		t.Errorf("the [DONE] on the next line must still be seen: %v", lines)
	}
}

func TestStreamWithoutDoneIsNotComplete(t *testing.T) {
	fake := &fakeUpstream{models: modelsJSON, stream: func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`data: {"id":"c1","choices":[{"index":0,"delta":{"content":"half"},"token_ids":[1]}]}` + "\n\n"))
		// no usage and no [DONE]: the answer stops here
	}}
	tt := newTester(t, "strata", fake, nil)
	rec := tt.post(`{"messages":[],"stream":true}`)
	if strings.Contains(rec.Body.String(), "[DONE]") {
		t.Error("a lost stream must not be reported as finished")
	}
	if !tt.hasLog("upstream ended early") {
		t.Errorf("want a lost-stream log line, got %v", tt.logs())
	}
}

func TestStreamUpstreamErrorFrame(t *testing.T) {
	fake := &fakeUpstream{models: modelsJSON, stream: func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`data: {"id":"c1","choices":[{"index":0,"delta":{"content":"par"},"token_ids":[1]}]}` + "\n\n"))
		w.Write([]byte(`data: {"error":{"message":"CUDA out of memory"}}` + "\n\n"))
		w.Write([]byte("data: [DONE]\n\n"))
	}}
	tt := newTester(t, "strata", fake, nil)
	rec := tt.post(`{"messages":[],"stream":true}`)
	body := rec.Body.String()
	if strings.Contains(body, "CUDA") {
		t.Error("an engine error frame is not model output")
	}
	if strings.Contains(body, "[DONE]") {
		t.Error("after an error frame the answer is not complete")
	}
	if !tt.hasLog("engine error frame: CUDA out of memory") {
		t.Errorf("the error must be logged, got %v", tt.logs())
	}
}

func TestStreamBadChunkIsSkipped(t *testing.T) {
	fake := &fakeUpstream{models: modelsJSON, stream: func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("data: {not json at all\n\n"))
		w.Write([]byte(`data: {"id":"c1","choices":[{"index":0,"delta":{"content":"ok"},"token_ids":[1]}]}` + "\n\n"))
		w.Write([]byte("data: [DONE]\n\n"))
	}}
	tt := newTester(t, "strata", fake, nil)
	rec := tt.post(`{"messages":[],"stream":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	lines := dataLines(rec.Body.String())
	if len(lines) != 3 {
		t.Errorf("the broken chunk must be skipped: %v", lines)
	}
}

func TestInbandTimingsWin(t *testing.T) {
	fake := &fakeUpstream{models: modelsJSON, stream: func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`data: {"id":"c1","choices":[{"index":0,"delta":{"content":"hello there, a longer answer"},"token_ids":[1,2,3]}]}` + "\n\n"))
		w.Write([]byte(`data: {"id":"c1","choices":[],"timings":{"prompt_n":63,"prompt_ms":84.9,"predicted_n":900,"predicted_ms":9171.1,"cache_n":12,"draft_n":1440,"draft_n_accepted":892}}` + "\n\n"))
		w.Write([]byte("data: [DONE]\n\n"))
	}}
	tt := newTester(t, "strata", fake, nil)
	rec := tt.post(`{"messages":[],"stream":true}`)
	lines := dataLines(rec.Body.String())
	timings := backend.Obj(mustJSON(t, lines[len(lines)-2]), "timings")
	if timings == nil {
		t.Fatalf("the last chunk must carry timings: %v", lines)
	}
	if backend.NumOr(timings, "prompt_ms", 0) != 84.9 || backend.NumOr(timings, "predicted_ms", 0) != 9171.1 {
		t.Errorf("the engine numbers must pass through untouched: %v", timings)
	}
	if backend.NumOr(timings, "draft_n_accepted", 0) != 892 {
		t.Error("the draft counts are part of what the engine measured")
	}
	if !tt.hasLog("timings(engine) pp 63 tok 742.0 t/s | tg 900 tok 98.1 t/s | cache 12") {
		t.Errorf("want the engine log line, got %v", tt.logs())
	}
}

// TestVLLMWindowGuards: the histogram diff is trusted only for a request that
// was alone in the window.
func TestVLLMWindowGuards(t *testing.T) {
	type sample struct {
		count    float64
		prefill  float64
		decode   float64
		computed float64
		running  float64
		waiting  float64
	}
	pre := sample{count: 10, prefill: 1.0, decode: 0.5, computed: 200}
	post := sample{count: 11, prefill: 1.2, decode: 0.7, computed: 208}
	cases := []struct {
		name string
		pre  sample
		post sample
		want string
	}{
		{"clean", pre, post, "timings(engine)"},
		{"no request finished", pre, pre, "timings(wall)"},
		{"two requests finished", pre, sample{count: 12, prefill: 1.2, decode: 0.7, computed: 208}, "timings(wall)"},
		{"something was running", sample{count: 10, prefill: 1, decode: 0.5, computed: 200, running: 1}, post, "timings(wall)"},
		{"something was waiting", sample{count: 10, prefill: 1, decode: 0.5, computed: 200, waiting: 2}, post, "timings(wall)"},
		{"prefill went backwards", pre, sample{count: 11, prefill: 0.9, decode: 0.7, computed: 208}, "timings(wall)"},
		{"computed went backwards", pre, sample{count: 11, prefill: 1.2, decode: 0.7, computed: 190}, "timings(wall)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := &fakeUpstream{
				models: modelsJSON,
				stream: sseScript,
				metrics: []string{
					metricsText(tc.pre.count, tc.pre.prefill, tc.pre.decode, tc.pre.computed, tc.pre.running, tc.pre.waiting),
					metricsText(tc.post.count, tc.post.prefill, tc.post.decode, tc.post.computed, tc.post.running, tc.post.waiting),
				},
			}
			tt := newTester(t, "vllm", fake, nil)
			rec := tt.post(`{"messages":[{"role":"user","content":"hi"}],"stream":true}`)
			if !tt.hasLog(tc.want) {
				t.Errorf("want %s, log was %v", tc.want, tt.logs())
			}
			lines := dataLines(rec.Body.String())
			if backend.Obj(mustJSON(t, lines[len(lines)-2]), "timings") == nil {
				t.Error("the last chunk must always carry timings")
			}
			if lines[len(lines)-1] != "[DONE]" {
				t.Error("a complete answer still ends normally")
			}
		})
	}
}

func TestEngineTimingsSwitchOff(t *testing.T) {
	fake := &fakeUpstream{
		models:  modelsJSON,
		stream:  sseScript,
		metrics: []string{metricsText(10, 1, 0.5, 200, 0, 0), metricsText(11, 1.2, 0.7, 208, 0, 0)},
	}
	tt := newTester(t, "vllm", fake, func(c *appconf.Config) { c.EngineTimings = false })
	tt.post(`{"messages":[],"stream":true}`)
	if tt.scrapes() != 0 {
		t.Errorf("VLLM_ENGINE_TIMINGS=0 must skip the window, %d scrapes", tt.scrapes())
	}
	if !tt.hasLog("timings(wall)") {
		t.Errorf("want wall clock numbers, got %v", tt.logs())
	}
}

func TestMissingMetricsFallsBackToWall(t *testing.T) {
	fake := &fakeUpstream{models: modelsJSON, stream: sseScript}
	tt := newTester(t, "vllm", fake, nil)
	rec := tt.post(`{"messages":[],"stream":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("a dead /metrics must not fail the answer: %d", rec.Code)
	}
	if !tt.hasLog("snapshot failed") || !tt.hasLog("timings(wall)") {
		t.Errorf("want a logged snapshot failure and wall numbers, got %v", tt.logs())
	}
}

func TestUsageAbsentGetsSynthesizedChunk(t *testing.T) {
	fake := &fakeUpstream{models: modelsJSON, stream: func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`data: {"id":"c9","model":"qwen-test","choices":[{"index":0,"delta":{"content":"hi"},"token_ids":[1]}]}` + "\n\n"))
		w.Write([]byte("data: [DONE]\n\n"))
	}}
	tt := newTester(t, "vllm", fake, nil)
	rec := tt.post(`{"messages":[],"stream":true}`)
	lines := dataLines(rec.Body.String())
	last := mustJSON(t, lines[len(lines)-2])
	if backend.Str(last, "object") != "chat.completion.chunk" {
		t.Errorf("the synthesized chunk must look like one: %v", last)
	}
	if backend.Str(last, "id") != "c9" || backend.Str(last, "model") != "qwen-test" {
		t.Errorf("id and model are taken from the stream: %v", last)
	}
	if len(backend.Arr(last, "choices")) != 0 {
		t.Error("a synthesized chunk carries no delta")
	}
	if backend.Obj(last, "timings") == nil {
		t.Error("a synthesized chunk carries the final timings")
	}
}

func TestStrataCountsTokensByCharacters(t *testing.T) {
	fake := &fakeUpstream{models: modelsJSON, stream: func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`data: {"choices":[{"index":0,"delta":{"content":"0123456789012345678901234567890123"}}]}` + "\n\n"))
		w.Write([]byte("data: [DONE]\n\n"))
	}}
	tt := newTester(t, "strata", fake, nil)
	rec := tt.post(`{"messages":[],"stream":true}`)
	lines := dataLines(rec.Body.String())
	timings := backend.Obj(mustJSON(t, lines[len(lines)-2]), "timings")
	if got := backend.NumOr(timings, "predicted_n", 0); got != 9 {
		t.Errorf("predicted_n = %v, want the characters-over-four estimate", got)
	}
}

func TestNonStreamReasoningRename(t *testing.T) {
	fake := &fakeUpstream{models: modelsJSON, stream: func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"c1","model":"qwen-test","choices":[{"index":0,"message":{"role":"assistant","content":"hi","reasoning":"a thought"}}],"usage":{"prompt_tokens":4,"completion_tokens":1}}`))
	}}
	tt := newTester(t, "vllm", fake, nil)
	rec := tt.post(`{"messages":[],"stream":false}`)
	body := rec.Body.String()
	if !strings.Contains(body, `"reasoning_content":"a thought"`) {
		t.Errorf("message.reasoning must be renamed: %s", body)
	}
	if strings.Contains(body, `"reasoning":`) {
		t.Errorf("the old key must be gone: %s", body)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("a non-streamed answer is one JSON body, got %q", ct)
	}
	sent := fake.sent()
	if _, ok := sent["stream_options"]; ok {
		t.Error("a non-streamed request must not ask for stream usage")
	}
	if _, ok := sent["return_token_ids"]; ok {
		t.Error("a non-streamed request needs no token ids")
	}
}

func TestUpstreamRejectionPassesThroughAndCorrectsVision(t *testing.T) {
	const imageBody = `{"stream":false,"messages":[{"role":"user","content":[{"type":"text","text":"look"},{"type":"image_url","image_url":{"url":"data:image/png;base64,AAA"}}]}]}`
	reject := &fakeUpstream{
		models:     modelsJSON,
		status:     http.StatusBadRequest,
		statusBody: `{"error":{"message":"This model's architecture does not support image inputs."}}`,
	}
	tt := newTester(t, "vllm", reject, func(c *appconf.Config) { c.Vision = "1" })
	if !tt.d.Flags.Vision.Get() {
		t.Fatal("VLLM_MODALITY_VISION=1 must start vision on")
	}
	rec := tt.post(imageBody)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want the upstream answer passed through", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "does not support image") {
		t.Errorf("the body must reach the UI: %s", rec.Body)
	}
	if tt.d.Flags.Vision.Get() {
		t.Error("a rejected image must turn vision off")
	}
	if !tt.hasLog("vision support: off") {
		t.Errorf("want the correction logged, got %v", tt.logs())
	}

	accept := &fakeUpstream{models: modelsJSON, stream: func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"choices":[{"index":0,"message":{"role":"assistant","content":"A"}}]}`))
	}}
	on := newTester(t, "vllm", accept, nil) // auto starts off
	on.post(imageBody)
	if !on.d.Flags.Vision.Get() {
		t.Error("an accepted image must turn vision on")
	}
	if !on.hasLog("vision support: on") {
		t.Errorf("want the correction logged, got %v", on.logs())
	}
}

func TestBadJSONBodyAnswers400(t *testing.T) {
	tt := newTester(t, "vllm", &fakeUpstream{models: modelsJSON}, nil)
	rec := tt.post(`{"messages": [`)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "invalid JSON body") {
		t.Errorf("body = %s", rec.Body)
	}
}

func TestEmptyBodyIsAnEmptyRequest(t *testing.T) {
	fake := &fakeUpstream{models: modelsJSON, stream: func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"choices":[{"index":0,"message":{"role":"assistant","content":"A"}}]}`))
	}}
	tt := newTester(t, "vllm", fake, nil)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	rec := httptest.NewRecorder()
	tt.p.Complete(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d body %s", rec.Code, rec.Body)
	}
}

func TestUnreachableEngineAnswers502(t *testing.T) {
	cfg := &appconf.Config{Backend: "vllm", Upstream: "http://127.0.0.1:1", Vision: "auto"}
	var mu sync.Mutex
	var lines []string
	d := backend.NewDeps(cfg, func(f string, v ...any) {
		mu.Lock()
		defer mu.Unlock()
		lines = append(lines, fmt.Sprintf(f, v...))
	})
	b, _ := backend.New("vllm", d)
	p := New(b, d)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"messages":[],"stream":true}`))
	rec := httptest.NewRecorder()
	p.Complete(rec, req)
	if rec.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "cannot reach the vllm backend at") {
		t.Errorf("body = %s", rec.Body)
	}
}

func TestClientDisconnectStopsTheEngine(t *testing.T) {
	fake := &fakeUpstream{models: modelsJSON, stream: func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`data: {"id":"c1","choices":[{"index":0,"delta":{"content":"go"},"token_ids":[1]}]}` + "\n\n"))
		// the engine keeps talking until its client goes away
		<-r.Context().Done()
	}}
	tt := newTester(t, "vllm", fake, nil)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"messages":[],"stream":true}`))
	ctx, cancel := context.WithCancel(req.Context())
	req = req.WithContext(ctx)
	rec := &closingWriter{}
	done := make(chan struct{})
	go func() {
		tt.p.Complete(rec, req)
		close(done)
	}()
	// Wait until the first chunk has been forwarded, so the abort lands mid-stream
	// rather than before the engine was reached at all.
	deadline := time.Now().Add(5 * time.Second)
	for rec.length() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if rec.length() == 0 {
		t.Fatal("nothing reached the client")
	}
	rec.dead.Store(true)
	cancel() // the UI closed the connection
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the handler kept streaming after the client went away")
	}
	if strings.Contains(rec.body.String(), "[DONE]") {
		t.Error("an aborted answer must not claim to be finished")
	}
	if !tt.hasLog("client aborted after 1 tokens, engine cancelled") {
		t.Errorf("want an abort log line, got %v", tt.logs())
	}
}

// closingWriter is a response writer whose client has vanished.
type closingWriter struct {
	mu   sync.Mutex
	body strings.Builder
	dead atomic.Bool
}

func (c *closingWriter) Header() http.Header { return http.Header{} }

func (c *closingWriter) Write(b []byte) (int, error) {
	if c.dead.Load() {
		return 0, errClientGone
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.body.Write(b)
}

func (c *closingWriter) WriteHeader(int) {}
func (c *closingWriter) Flush()          {}

func (c *closingWriter) length() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.body.Len()
}

type goneError struct{}

func (goneError) Error() string { return "client went away" }

var errClientGone = goneError{}

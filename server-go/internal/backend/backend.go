// Package backend holds the engine abstraction. Each engine knows how to
// describe itself, what to send upstream, and where its throughput numbers come
// from, so the HTTP layer stays the same for both.
//
// vLLM speaks plain OpenAI: no timings, no /props, and its numbers live in
// Prometheus counters that mix every client.
//
// Strata is a llama.cpp-style server: it answers /props, /slots and /health,
// names the thinking field the way the webui expects, and puts a
// llama.cpp-shaped timings object on the last chunk.
package backend

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"llama-webui/server/internal/appconf"
)

// Log writes one line with the same HH:MM:SS prefix everywhere.
type Log func(format string, v ...any)

// Flags are the capability flags the two engines report in different ways.
type Flags struct {
	Reasoning syncBool
	Vision    syncBool
}

type syncBool struct {
	m sync.Mutex
	b bool
}

func (s *syncBool) Get() bool {
	s.m.Lock()
	defer s.m.Unlock()
	return s.b
}

func (s *syncBool) Set(v bool) {
	s.m.Lock()
	defer s.m.Unlock()
	s.b = v
}

// Upstream is the HTTP client for the inference server.
type Upstream struct {
	Base   string
	APIKey string
	Client *http.Client
}

// NewUpstream builds a client. No global timeout: a streamed answer is bounded
// by the reader, and control reads pass their own deadline.
func NewUpstream(cfg *appconf.Config) *Upstream {
	return &Upstream{Base: cfg.Upstream, APIKey: cfg.APIKey, Client: &http.Client{}}
}

// NewRequest makes a request against the backend with the usual headers.
func (u *Upstream) NewRequest(ctx context.Context, method, path string, body []byte) (*http.Request, error) {
	var r io.Reader
	if len(body) > 0 {
		r = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, u.Base+path, r)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if u.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+u.APIKey)
	}
	return req, nil
}

// Do sends a request and hands back the live response. The caller closes it.
func (u *Upstream) Do(ctx context.Context, method, path string, body []byte) (*http.Response, error) {
	req, err := u.NewRequest(ctx, method, path, body)
	if err != nil {
		return nil, err
	}
	return u.Client.Do(req)
}

// FetchJSON is a small control read. A stuck endpoint must not stall whatever
// chat request is waiting on it, so every one carries a deadline.
func (u *Upstream) FetchJSON(ctx context.Context, path string, timeout time.Duration) (map[string]any, error) {
	text, err := u.FetchText(ctx, path, timeout)
	if err != nil {
		return nil, err
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		return nil, fmt.Errorf("%s returned invalid JSON", path)
	}
	return out, nil
}

// FetchText GETs a path and returns its body as text.
func (u *Upstream) FetchText(ctx context.Context, path string, timeout time.Duration) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	res, err := u.Do(ctx, http.MethodGet, path, nil)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode > 299 {
		io.Copy(io.Discard, io.LimitReader(res.Body, 1<<16))
		return "", statusErr(path, res.StatusCode)
	}
	return readAll(res.Body)
}

// PostJSON POSTs a body and decodes the reply.
func (u *Upstream) PostJSON(ctx context.Context, path string, body any, timeout time.Duration) (map[string]any, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	res, err := u.Do(ctx, http.MethodPost, path, raw)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	text, err := readAll(res.Body)
	if err != nil {
		return nil, err
	}
	if res.StatusCode < 200 || res.StatusCode > 299 {
		return nil, statusErr(path, res.StatusCode)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		return nil, fmt.Errorf("%s returned invalid JSON", path)
	}
	return out, nil
}

// ModelCache keeps the model list, so a page load does not ask twice.
type ModelCache struct {
	up  *Upstream
	log Log

	mu    sync.Mutex
	at    time.Time
	data  map[string]any
	defID string
}

const modelsTTL = 30 * time.Second
const modelsTimeout = 8 * time.Second

// List returns the model list, or nil when the upstream could not be reached.
// Callers must tell "no answer" apart from "answered with an empty list", so a
// failed refresh falls back to the last good answer.
func (m *ModelCache) List(ctx context.Context, force bool) map[string]any {
	m.mu.Lock()
	cached := m.data
	fresh := time.Since(m.at) < modelsTTL
	m.mu.Unlock()
	if !force && fresh && cached != nil {
		return cached
	}
	data, err := m.up.FetchJSON(ctx, "/v1/models", modelsTimeout)
	if err != nil {
		m.log("model list failed: %s", err)
		return cached
	}
	m.mu.Lock()
	m.at = time.Now()
	m.data = data
	m.mu.Unlock()
	return data
}

// DefaultID is the model to send when the UI leaves it out, which it does in
// single-model mode while both engines want one.
func (m *ModelCache) DefaultID(ctx context.Context, configured string) string {
	if configured != "" {
		return configured
	}
	m.mu.Lock()
	id := m.defID
	m.mu.Unlock()
	if id != "" {
		return id
	}
	models := m.List(ctx, true)
	if e := Entries(models); len(e) > 0 {
		id = Str(e[0], "id")
	}
	m.mu.Lock()
	m.defID = id
	m.mu.Unlock()
	return id
}

// Describe is what a backend reports about itself, before /props shapes it.
type Describe struct {
	// Entry is the model object the upstream reported, nil when there was none.
	Entry     map[string]any
	NCtx      float64
	ModelPath string
	// HasModelPath separates "" from "the backend does not report one".
	HasModelPath  bool
	BuildInfo     string
	ChatTemplate  string
	Modalities    map[string]any
	TotalSlots    float64
	HasTotalSlots bool
	Params        map[string]any
	Speculative   bool
}

// Stream is what the rewriter learned while a response streamed by.
type Stream struct {
	FirstAt   time.Time
	LastAt    time.Time
	Tokens    int
	Chars     int
	Usage     map[string]any
	ID        string
	Model     string
	Inband    map[string]any
	Err       string
	SawDone   bool
	Truncated bool
}

// SnapshotOpts steers a metrics read. A window around a request must read fresh,
// or the two snapshots are the same cached scrape and the delta is zero.
type SnapshotOpts struct {
	Force   bool
	Timeout time.Duration
}

// ProbeFlags says whether startup probes are worth running for an engine.
type ProbeFlags struct {
	Thinking bool
	Vision   bool
}

// RateKeys names the cumulative counters an engine publishes, by their field in
// Snapshot. Empty means the engine does not accumulate that one.
type RateKeys struct {
	Gen         string
	Prompt      string
	Drafts      string
	DraftTokens string
	Accepted    string
}

// Backend is one inference engine.
type Backend interface {
	ID() string
	Title() string
	// WindowsTimings is true for an engine with no in-band timings, where a
	// metrics window around each request is the only way to get real numbers.
	WindowsTimings() bool
	Probes() ProbeFlags
	Detect(ctx context.Context) bool
	Describe(ctx context.Context) (*Describe, error)
	// Translate rewrites the UI request in place into what this engine reads.
	Translate(out map[string]any)
	// Prepare adds the fields the engine needs to report numbers back.
	Prepare(out map[string]any)
	RewriteChunk(chunk map[string]any, st *Stream)
	FinalTimings(st *Stream, pre, post *Snapshot, tSend time.Time) map[string]any
	Snapshot(ctx context.Context, opts SnapshotOpts) (*Snapshot, error)
	Engine(s *Snapshot) map[string]any
	Counters(s *Snapshot) map[string]any
	Gauges(s *Snapshot) map[string]any
	RateKeys() RateKeys
	Labels() map[string]any
}

// Prober is implemented by a backend that can ask its engine about capabilities.
type Prober interface {
	ProbeThinking(ctx context.Context)
	ProbeVision(ctx context.Context)
}

// Deps is everything a backend needs from the running server.
type Deps struct {
	Cfg    *appconf.Config
	Flags  *Flags
	Log    Log
	Up     *Upstream
	Models *ModelCache
	Prom   *PromCache
}

// NewDeps wires a Deps for a config.
func NewDeps(cfg *appconf.Config, log Log) *Deps {
	up := NewUpstream(cfg)
	d := &Deps{Cfg: cfg, Log: log, Up: up, Flags: &Flags{}}
	vision, on := cfg.VisionForced()
	if vision {
		d.Flags.Vision.Set(on)
	}
	d.Models = &ModelCache{up: up, log: log}
	d.Prom = NewPromCache(up)
	return d
}

// New returns a backend by name.
func New(id string, d *Deps) (Backend, error) {
	switch id {
	case "vllm":
		return newVLLM(d), nil
	case "strata":
		return newStrata(d), nil
	}
	return nil, fmt.Errorf("unknown backend %q", id)
}

// AutoDetect asks the upstream what it is. Strata announces itself on /health,
// vLLM answers /version, and either way that is one cheap GET. A failure to
// reach anything is not fatal: vLLM stays the default.
func AutoDetect(ctx context.Context, d *Deps) Backend {
	strata := newStrata(d)
	if strata.Detect(ctx) {
		return strata
	}
	// keep the older default for an unknown server
	return newVLLM(d)
}

// SafeSnapshot reads a snapshot without failing the caller: a backend that
// cannot answer its metrics just leaves the numbers to the wall clock.
func SafeSnapshot(ctx context.Context, b Backend, d *Deps, opts SnapshotOpts) *Snapshot {
	s, err := b.Snapshot(ctx, opts)
	if err != nil {
		d.Log("snapshot failed: %s", err)
		return nil
	}
	return s
}

// ---------- request translation ----------

// dropLlamaOnly are fields an OpenAI server has no use for.
var dropLlamaOnly = map[string]bool{
	"return_progress": true, "sse_ping_interval": true, "timings_per_token": true,
	"backend_sampling": true, "reasoning_control": true, "thinking_budget_tokens": true,
	"samplers": true, "mirostat": true, "mirostat_tau": true, "mirostat_eta": true,
	"xtc_probability": true, "xtc_threshold": true, "typ_p": true,
	"dynatemp_range": true, "dynatemp_exponent": true, "dry_multiplier": true,
	"dry_base": true, "dry_allowed_length": true, "dry_penalty_last_n": true,
	"dry_sequence_breakers": true, "top_n_sigma": true, "n_keep": true,
	"n_discard": true, "min_keep": true, "n_probs": true, "post_sampling_probs": true,
	"grammar": true, "grammar_lazy": true, "grammar_triggers": true,
	"preserved_tokens": true, "chat_format": true, "reasoning_in_content": true,
	"generation_prompt": true, "lora": true, "slot_id": true, "nid": true,
	"cache_prompt": true, "normalize_prefix": true, "penalize_nl": true, "n_ctx": true,
}

// mapReasoning maps the UI thinking switch onto the template kwarg both read.
func mapReasoning(out map[string]any) {
	if Str(out, "reasoning_format") == "none" {
		kw := Obj(out, "chat_template_kwargs")
		merged := map[string]any{}
		for k, v := range kw {
			merged[k] = v
		}
		merged["enable_thinking"] = false
		out["chat_template_kwargs"] = merged
	}
}

// commonTranslate removes fields neither engine reads and settles max_tokens.
func commonTranslate(out map[string]any, drop map[string]bool, log Log) {
	var dropped []string
	for _, set := range []map[string]bool{dropLlamaOnly, drop} {
		for _, k := range sortedKeys(set) {
			if _, ok := out[k]; ok && !contains(dropped, k) {
				dropped = append(dropped, k)
			}
		}
	}
	for _, k := range dropped {
		delete(out, k)
	}
	if len(dropped) > 0 {
		log("dropped params: %s", strings.Join(dropped, " "))
	}

	// n_predict 0 is the prompt warm-up call: process the prompt, generate
	// nothing. Neither engine has that, so ask for the minimum.
	if v, ok := Num(out, "n_predict"); ok {
		if v == 0 {
			out["max_tokens"] = json.Number("1")
		} else {
			out["max_tokens"] = out["n_predict"]
		}
		delete(out, "n_predict")
	}
	if v, ok := Num(out, "max_tokens"); ok {
		if v == 0 {
			out["max_tokens"] = json.Number("1")
		} else if v < 0 {
			delete(out, "max_tokens")
		}
	}
}

// ---------- timings ----------

// WallTimings builds a timings object from what the stream itself told us.
// Every backend falls back to this when the engine reported nothing of its own.
func WallTimings(st *Stream, tSend time.Time) map[string]any {
	promptTotal := NumOr(st.Usage, "prompt_tokens", 0)
	cached := NumOr(Obj(st.Usage, "prompt_tokens_details"), "cached_tokens", 0)
	predictedN := NumOr(st.Usage, "completion_tokens", float64(st.Tokens))
	timings := map[string]any{
		"predicted_n": predictedN,
		"predicted_ms": func() float64 {
			if !st.FirstAt.IsZero() && !st.LastAt.IsZero() {
				return Round1(maxF(float64(msBetween(st.FirstAt, st.LastAt)), 1))
			}
			return 0
		}(),
	}
	promptN := maxF(promptTotal-cached, 0)
	if cached > 0 {
		timings["cache_n"] = cached
	}
	if promptN > 0 && !st.FirstAt.IsZero() {
		timings["prompt_n"] = promptN
		timings["prompt_ms"] = Round1(maxF(float64(msBetween(tSend, st.FirstAt)), 1))
	}
	return timings
}

// SpeedLine is the one log line per request naming the source of the numbers.
func SpeedLine(source string, timings map[string]any) string {
	promptMS := NumOr(timings, "prompt_ms", 0)
	promptN := NumOr(timings, "prompt_n", 0)
	predictedMS := NumOr(timings, "predicted_ms", 0)
	predictedN := NumOr(timings, "predicted_n", 0)
	pp := 0.0
	if promptMS != 0 {
		pp = promptN / promptMS * 1000
	}
	tg := 0.0
	if predictedMS != 0 {
		tg = predictedN / predictedMS * 1000
	}
	return fmt.Sprintf("timings(%s) pp %g tok %.1f t/s | tg %g tok %.1f t/s | cache %g",
		source, promptN, pp, predictedN, tg, NumOr(timings, "cache_n", 0))
}

// ---------- rates ----------

// RateWindow is the previous sample of the cumulative counters.
type RateWindow struct {
	At time.Time
	V  map[string]float64
}

// RatesFrom turns two polls of cumulative counters into the rate the engine is
// holding right now. It returns the new window, which the caller stores.
func RatesFrom(snap *Snapshot, sampleAt time.Time, keys RateKeys, prev *RateWindow) (map[string]any, *RateWindow) {
	now := sampleAt
	if now.IsZero() {
		now = time.Now()
	}
	window := &RateWindow{At: now, V: map[string]float64{}}
	for _, k := range rateKeyList(keys) {
		window.V[k] = snap.Num(k)
	}
	if prev == nil {
		return map[string]any{}, window
	}
	// An engine restart zeroes the counters, which would read as a huge
	// negative throughput. Drop the window and start a new one.
	for _, k := range rateKeyList(keys) {
		if snap.Num(k) < prev.V[k] {
			return map[string]any{}, window
		}
	}
	seconds := now.Sub(prev.At).Seconds()
	if seconds < 0.5 {
		return map[string]any{"window_s": Round1(seconds)}, window
	}
	// A long gap means the window mostly covers idle time, so the number would
	// be a stale average rather than a current rate. Let the next poll measure
	// a short window instead.
	if seconds > 60 {
		return map[string]any{"window_s": Round1(seconds)}, window
	}
	per := func(k string) float64 { return Round1((snap.Num(k) - prev.V[k]) / seconds) }
	out := map[string]any{"window_s": Round1(seconds)}
	if keys.Gen != "" {
		out["generation_tokens_per_second"] = per(keys.Gen)
	}
	if keys.Prompt != "" {
		out["prompt_tokens_per_second"] = per(keys.Prompt)
	}
	if keys.Drafts != "" {
		out["steps_per_second"] = per(keys.Drafts)
	}
	if keys.Accepted != "" && keys.DraftTokens != "" && snap.Num(keys.DraftTokens) > prev.V[keys.DraftTokens] {
		dDraft := snap.Num(keys.DraftTokens) - prev.V[keys.DraftTokens]
		if dDraft > 0 {
			out["window_accept_percent"] = Round1((snap.Num(keys.Accepted) - prev.V[keys.Accepted]) / dDraft * 100)
		} else {
			out["window_accept_percent"] = float64(0)
		}
	}
	return out, window
}

func rateKeyList(keys RateKeys) []string {
	out := make([]string, 0, 5)
	for _, k := range []string{keys.Gen, keys.Prompt, keys.Drafts, keys.DraftTokens, keys.Accepted} {
		if k != "" {
			out = append(out, k)
		}
	}
	return out
}

// ---------- small helpers, shared with the other packages ----------

// Snapshot is one read of the engine counters, in field names both engines map
// onto. Absent fields read as zero, the way the Node code treats undefined.
type Snapshot struct {
	// At is when the scrape finished. Rates must divide by the gap between two
	// scrapes, not between two wall clocks.
	At time.Time
	// P holds the numeric counters.
	P map[string]float64
	// Config holds labelled string values, like the vLLM KV layout.
	Config map[string]string
	// Requests is the per-request history an engine keeps.
	Requests []RequestStat
	// VRAMPercent and HasVRAM carry the GPU fill for an engine without a paged
	// KV cache.
	VRAMPercent float64
	HasVRAM     bool
	Live        map[string]any
}

// RequestStat is one finished request as an engine remembers it.
type RequestStat struct {
	PromptMS   float64
	DecodeMS   float64
	DecodeTokS float64
}

// Num reads a counter, zero when the engine does not publish it.
func (s *Snapshot) Num(k string) float64 {
	if s == nil || s.P == nil {
		return 0
	}
	return s.P[k]
}

// ConfigStr reads a labelled config value, or "" when absent.
func (s *Snapshot) ConfigStr(k string) string {
	if s == nil {
		return ""
	}
	return s.Config[k]
}

// readAll takes a bounded amount of text from a body, so a control endpoint
// that streams forever cannot grow the heap.
func readAll(r io.Reader) (string, error) {
	b, err := io.ReadAll(io.LimitReader(r, 8<<20))
	return string(b), err
}

// Num reads a JSON number that may have arrived as json.Number.
func Num(m map[string]any, k string) (float64, bool) {
	switch v := m[k].(type) {
	case json.Number:
		f, err := v.Float64()
		return f, err == nil
	case float64:
		return v, true
	case int:
		return float64(v), true
	case int64:
		return float64(v), true
	case bool:
		if v {
			return 1, true
		}
		return 0, true
	}
	return 0, false
}

// NumOr reads a JSON number with a fallback.
func NumOr(m map[string]any, k string, def float64) float64 {
	if v, ok := Num(m, k); ok {
		return v
	}
	return def
}

// Bool reads a JSON boolean.
func Bool(m map[string]any, k string) bool {
	v, _ := m[k].(bool)
	return v
}

// Truthy applies the coercion an engine field goes through in the Node version,
// where a value is tested rather than compared.
func Truthy(v any) bool {
	switch t := v.(type) {
	case nil:
		return false
	case bool:
		return t
	case string:
		return t != ""
	case float64:
		return t != 0
	case json.Number:
		return t.String() != "0"
	}
	return true
}

// Str reads a JSON string.
func Str(m map[string]any, k string) string {
	if v, ok := m[k].(string); ok {
		return v
	}
	return ""
}

// Obj reads a nested object.
func Obj(m map[string]any, k string) map[string]any {
	if v, ok := m[k].(map[string]any); ok {
		return v
	}
	return nil
}

// Arr reads a nested array.
func Arr(m map[string]any, k string) []any {
	if v, ok := m[k].([]any); ok {
		return v
	}
	return nil
}

// Entries pulls the data array out of a model list.
func Entries(models map[string]any) []map[string]any {
	var out []map[string]any
	for _, e := range Arr(models, "data") {
		if m, ok := e.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

// DecodeJSON parses text into an object, keeping numbers exact.
func DecodeJSON(text string) (map[string]any, error) {
	dec := json.NewDecoder(strings.NewReader(text))
	dec.UseNumber()
	var out map[string]any
	err := dec.Decode(&out)
	return out, err
}

// DecodeJSONStream reads one value with numbers kept as written.
func DecodeJSONStream(r io.Reader) (map[string]any, error) {
	dec := json.NewDecoder(r)
	dec.UseNumber()
	var out map[string]any
	err := dec.Decode(&out)
	return out, err
}

var spaceRun = regexp.MustCompile(`\s+`)

// Flatten collapses whitespace and caps the length, as the log and build.json do.
func Flatten(s string, max int) string {
	s = strings.TrimSpace(spaceRun.ReplaceAllString(s, " "))
	if max > 0 && len(s) > max {
		s = s[:max]
	}
	return s
}

// Round1 keeps one decimal, the precision the UI renders.
func Round1(n float64) float64 { return math.Round(n*10) / 10 }

func sortedKeys(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func contains(list []string, v string) bool {
	for _, k := range list {
		if k == v {
			return true
		}
	}
	return false
}

func maxF(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}

func msBetween(a, b time.Time) int64 { return b.Sub(a).Milliseconds() }

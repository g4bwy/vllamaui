package backend

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// probeImage is the smallest legal PNG: enough to make the engine run its vision
// encoder.
const probeImage = "data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8DwHwAFAAH/q842iQAAAABJRU5ErkJggg=="

// vLLM speaks no in-band timings, so the metrics window is the only way to get
// real numbers out of it.
type vllm struct {
	d *Deps
}

func newVLLM(d *Deps) *vllm { return &vllm{d: d} }

// vllmUnsupported: this build answers 400 for these while speculative decoding
// is on.
var vllmUnsupported = map[string]bool{"min_p": true, "logit_bias": true}
var noDrop = map[string]bool{}

func (v *vllm) ID() string           { return "vllm" }
func (v *vllm) Title() string        { return "vLLM engine" }
func (v *vllm) WindowsTimings() bool { return true }
func (v *vllm) Probes() ProbeFlags   { return ProbeFlags{Thinking: true, Vision: true} }
func (v *vllm) RateKeys() RateKeys {
	return RateKeys{Gen: "genTokens", Prompt: "promptTokens", Drafts: "specDrafts", DraftTokens: "specDraftTokens", Accepted: "specAcceptedTokens"}
}
func (v *vllm) Labels() map[string]any {
	return map[string]any{"kv": "KV cache", "ttft": "First token"}
}

// Detect: vLLM answers /version.
func (v *vllm) Detect(ctx context.Context) bool {
	res, err := v.d.Up.FetchJSON(ctx, "/version", 4*time.Second)
	return err == nil && nonNil(res, "version")
}

func (v *vllm) Describe(ctx context.Context) (*Describe, error) {
	models := v.d.Models.List(ctx, false)
	if models == nil {
		return nil, fmt.Errorf("cannot read the model list from %s", v.d.Cfg.Upstream)
	}
	version := "unknown"
	if res, err := v.d.Up.FetchJSON(ctx, "/version", 4*time.Second); err == nil {
		if s := asText(res["version"]); s != "" {
			version = s
		}
	}
	entry := pickModel(Entries(models), v.d.Cfg.Model)
	nCtx := float64(v.d.Cfg.NCtx)
	if nCtx == 0 {
		nCtx = NumOr(entry, "max_model_len", 0)
	}
	if nCtx == 0 {
		nCtx = NumOr(entry, "model_max_len", 0)
	}
	if nCtx == 0 {
		nCtx = 4096
	}
	return &Describe{
		Entry: entry,
		NCtx:  nCtx,
		// no host here: /props goes to every browser that loads the page
		BuildInfo: "vllm " + version,
		// vLLM never serves its chat template, and the UI decides whether the
		// model can think by scanning that template. The probe result stands in
		// for it.
		ChatTemplate: map[bool]string{true: "enable_thinking", false: ""}[v.d.Flags.Reasoning.Get()],
		Modalities:   map[string]any{"vision": v.d.Flags.Vision.Get(), "audio": false, "video": false},
		Params:       map[string]any{},
	}, nil
}

func (v *vllm) Translate(out map[string]any) {
	mapReasoning(out)
	if v.d.Cfg.KeepUnsupported {
		commonTranslate(out, noDrop, v.d.Log)
	} else {
		commonTranslate(out, vllmUnsupported, v.d.Log)
	}
}

func (v *vllm) Prepare(out map[string]any) {
	if Bool(out, "stream") {
		// usage on the last chunk is the only exact token count available
		opts := Obj(out, "stream_options")
		merged := map[string]any{}
		for k, val := range opts {
			merged[k] = val
		}
		merged["include_usage"] = true
		out["stream_options"] = merged
		out["return_token_ids"] = true
	}
}

func (v *vllm) RewriteChunk(chunk map[string]any, st *Stream) {
	choice := FirstChoice(chunk)
	d := Obj(choice, "delta")
	if d == nil {
		return
	}
	// vLLM spells the thinking field differently from the webui
	if r := Str(d, "reasoning"); r != "" {
		d["reasoning_content"] = r
		delete(d, "reasoning")
		v.d.Flags.Reasoning.Set(true)
	}
	hasText := Str(d, "content") != "" || Str(d, "reasoning_content") != "" || len(Arr(d, "tool_calls")) > 0
	if hasText {
		now := time.Now()
		if st.FirstAt.IsZero() {
			st.FirstAt = now
		}
		st.LastAt = now
	}
	if ids := Arr(choice, "token_ids"); ids != nil {
		st.Tokens += len(ids)
	} else if hasText {
		st.Tokens++
	}
}

// FinalTimings uses the histogram deltas, but only when this request was alone
// in the window.
func (v *vllm) FinalTimings(st *Stream, pre, post *Snapshot, tSend time.Time) map[string]any {
	timings := WallTimings(st, tSend)
	source := "wall"
	dCount := 0.0
	if pre != nil && post != nil {
		dCount = post.Num("prefillCount") - pre.Num("prefillCount")
	}
	quiet := pre != nil && pre.Num("running") == 0 && pre.Num("waiting") == 0
	rising := post != nil && pre != nil &&
		post.Num("prefillSum") >= pre.Num("prefillSum") &&
		post.Num("decodeSum") >= pre.Num("decodeSum") &&
		post.Num("computedSum") >= pre.Num("computedSum")
	if dCount == 1 && quiet && rising {
		source = "engine"
		promptTotal := NumOr(st.Usage, "prompt_tokens", 0)
		promptN := maxF(math.Round(post.Num("computedSum")-pre.Num("computedSum")), 0)
		timings["prompt_ms"] = Round1((post.Num("prefillSum") - pre.Num("prefillSum")) * 1000)
		timings["predicted_ms"] = orVal(Round1((post.Num("decodeSum")-pre.Num("decodeSum"))*1000), NumOr(timings, "predicted_ms", 0))
		timings["prompt_n"] = promptN
		timings["cache_n"] = maxF(promptTotal-promptN, 0)
	}
	v.d.Log(SpeedLine(source, timings))
	return timings
}

func (v *vllm) Snapshot(ctx context.Context, opts SnapshotOpts) (*Snapshot, error) {
	return v.d.Prom.Snapshot(ctx, opts)
}

func (v *vllm) Engine(s *Snapshot) map[string]any {
	c := map[string]string{}
	if s != nil {
		c = s.Config
	}
	return map[string]any{
		"model":                  nilIfEmpty(orValStr(v.d.Cfg.Model, c["model"])),
		"block_size":             nilIfEmpty(c["block_size"]),
		"num_gpu_blocks":         nilIfEmpty(c["num_gpu_blocks"]),
		"kv_cache_size_tokens":   nilIfEmpty(c["kv_cache_size_tokens"]),
		"prefix_caching":         nilIfEmpty(c["enable_prefix_caching"]),
		"gpu_memory_utilization": nilIfEmpty(c["gpu_memory_utilization"]),
	}
}

func (v *vllm) Counters(s *Snapshot) map[string]any {
	return map[string]any{
		"prompt_tokens":      s.Num("promptTokens"),
		"generation_tokens":  s.Num("genTokens"),
		"requests":           s.Num("prefillCount"),
		"avg_ttft_ms":        div1(s, "ttftSum", "ttftCount"),
		"avg_inter_token_ms": div1(s, "itlSum", "itlCount"),
		"avg_queue_ms":       div1(s, "queueSum", "prefillCount"),
		"avg_prefill_ms":     div1(s, "prefillSum", "prefillCount"),
		"avg_decode_ms":      div1(s, "decodeSum", "decodeCount"),
		"avg_prefill_tokens": avgInt(s, "computedSum", "computedCount"),
	}
}

func (v *vllm) Gauges(s *Snapshot) map[string]any {
	pct := func(a, b float64) float64 {
		if b == 0 {
			return 0
		}
		return Round1(a / b * 100)
	}
	return map[string]any{
		"requests_running":           s.Num("running"),
		"requests_waiting":           s.Num("waiting"),
		"kv_cache_usage_percent":     Round1(s.Num("kvUsage") * 100),
		"prefix_cache_hit_percent":   pct(s.Num("prefixHits"), s.Num("prefixQueries")),
		"spec_decode_accept_percent": pct(s.Num("specAcceptedTokens"), s.Num("specDraftTokens")),
		"tokens_per_step": func() float64 {
			if s.Num("specDrafts") == 0 {
				return 0
			}
			return Round1((s.Num("specAcceptedTokens") + s.Num("specDrafts")) / s.Num("specDrafts"))
		}(),
		"preemptions": s.Num("preemptions"),
	}
}

// ---------- startup probes ----------

var reasoningKeyRe = regexp.MustCompile(`"reasoning"\s*:`)

const probeTimeout = 12 * time.Second

// ProbeThinking asks the engine once whether it thinks, so the first page load
// already offers the control. A short streamed completion answers it.
func (v *vllm) ProbeThinking(ctx context.Context) {
	if !v.d.Cfg.ProbeThinking {
		return
	}
	body, err := json.Marshal(map[string]any{
		"model":      v.d.Models.DefaultID(ctx, v.d.Cfg.Model),
		"messages":   []map[string]any{{"role": "user", "content": "Hi"}},
		"stream":     true,
		"max_tokens": 16,
	})
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	res, err := v.d.Up.Do(ctx, http.MethodPost, "/v1/chat/completions", body)
	if err != nil {
		v.d.Log("thinking probe failed: %s", err)
		return
	}
	defer res.Body.Close()
	text, err := readAll(res.Body)
	if err != nil {
		v.d.Log("thinking probe failed: %s", err)
		return
	}
	if res.StatusCode >= 200 && res.StatusCode <= 299 && reasoningKeyRe.MatchString(text) {
		v.d.Flags.Reasoning.Set(true)
		v.d.Log("thinking support: yes (engine emitted reasoning deltas)")
	}
}

// ProbeVision answers whether the model sees. The UI refuses image files unless
// /props says it can take them, so this has to come from the engine and not from
// a guess. It costs one short prefill at startup.
func (v *vllm) ProbeVision(ctx context.Context) {
	cfg := v.d.Cfg
	if forced, on := cfg.VisionForced(); forced {
		v.d.Flags.Vision.Set(on)
		v.d.Log("vision: %s (VLLM_MODALITY_VISION)", map[bool]string{true: "on", false: "off"}[on])
		return
	}
	if !cfg.ProbeVision {
		v.d.Log("vision: off (probe skipped, set VLLM_MODALITY_VISION=1 to force on)")
		return
	}
	body, err := json.Marshal(map[string]any{
		"model": v.d.Models.DefaultID(ctx, cfg.Model),
		"messages": []map[string]any{{
			"role": "user",
			"content": []map[string]any{
				{"type": "text", "text": "Reply with the letter A."},
				{"type": "image_url", "image_url": map[string]any{"url": probeImage}},
			},
		}},
		"stream":     false,
		"max_tokens": 1,
	})
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	res, err := v.d.Up.Do(ctx, http.MethodPost, "/v1/chat/completions", body)
	if err != nil {
		v.d.Log("vision probe failed: %s", err)
		return
	}
	defer res.Body.Close()
	ok := res.StatusCode >= 200 && res.StatusCode <= 299
	if ok {
		v.d.Flags.Vision.Set(true)
		readAll(res.Body)
		v.d.Log("vision support: yes (engine accepted an image)")
		return
	}
	text, _ := readAll(res.Body)
	reason := "probe failed"
	if ImageRejectRe.MatchString(text) {
		reason = "engine rejects image inputs"
	}
	v.d.Log("vision support: no (%s, HTTP %d) %s", reason, res.StatusCode, Flatten(text, 160))
}

// ImageRejectRe is how an engine says it cannot take an image.
var ImageRejectRe = regexp.MustCompile(`image|visual|multimodal|modality`)

// ---------- shared value helpers ----------

// nonNil reports whether a JSON field is present and not empty.
func nonNil(m map[string]any, k string) bool {
	v, ok := m[k]
	if !ok || v == nil {
		return false
	}
	switch t := v.(type) {
	case string:
		return t != ""
	case bool:
		return t
	}
	return true
}

func asText(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case json.Number:
		return t.String()
	case bool:
		return fmt.Sprintf("%t", t)
	default:
		b, err := json.Marshal(t)
		if err != nil {
			return ""
		}
		return strings.Trim(string(b), `"`)
	}
}

// FirstChoice returns choices[0] of a completion chunk or response.
func FirstChoice(chunk map[string]any) map[string]any {
	for _, c := range Arr(chunk, "choices") {
		if m, ok := c.(map[string]any); ok {
			return m
		}
	}
	return nil
}

// pickModel is the entry the UI asked for, else the first one reported.
func pickModel(list []map[string]any, want string) map[string]any {
	if len(list) == 0 {
		return nil
	}
	for _, m := range list {
		if want != "" && Str(m, "id") == want {
			return m
		}
	}
	return list[0]
}

func nilIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func orVal(v, def float64) float64 {
	if v != 0 {
		return v
	}
	return def
}

func orValStr(v, def string) string {
	if v != "" {
		return v
	}
	return def
}

// div1 is an average in milliseconds, rounded to one decimal.
func div1(s *Snapshot, sum, count string) float64 {
	if s.Num(count) == 0 {
		return 0
	}
	return Round1(s.Num(sum) / s.Num(count) * 1000)
}

func avgInt(s *Snapshot, sum, count string) float64 {
	if s.Num(count) == 0 {
		return 0
	}
	return math.Round(s.Num(sum) / s.Num(count))
}

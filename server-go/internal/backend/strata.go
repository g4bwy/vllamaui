package backend

import (
	"context"
	"math"
	"regexp"
	"sync"
	"time"
)

// Strata is a llama.cpp-style server: it answers /props, /slots and /health,
// names the thinking field the way the webui expects, and puts a
// llama.cpp-shaped timings object on the last chunk.
type strata struct {
	d *Deps

	// Strata reports no cumulative draft counters, so the last request's
	// speculative numbers are the best available.
	specMu    sync.Mutex
	lastDraft float64
	lastAccpt float64
	hasSpec   bool
}

func newStrata(d *Deps) *strata { return &strata{d: d} }

func (s *strata) ID() string           { return "strata" }
func (s *strata) Title() string        { return "Strata engine" }
func (s *strata) WindowsTimings() bool { return false }
func (s *strata) Probes() ProbeFlags   { return ProbeFlags{} }
func (s *strata) Labels() map[string]any {
	// Strata has no paged KV cache to fill, and its prompt_ms covers the read of
	// the prompt rather than the wait for the first token.
	return map[string]any{"kv": "VRAM", "ttft": "Prompt read"}
}
func (s *strata) RateKeys() RateKeys {
	return RateKeys{Gen: "genTokens", Prompt: "promptTokens"}
}

// Detect: Strata announces itself on /health.
func (s *strata) Detect(ctx context.Context) bool {
	health, err := s.d.Up.FetchJSON(ctx, "/health", 4*time.Second)
	return err == nil && Str(health, "service") == "strata"
}

var thinkingTemplateRe = regexp.MustCompile(`enable_thinking|<\|think`)

// Describe: /props is already the shape the webui wants, with a real chat
// template.
func (s *strata) Describe(ctx context.Context) (*Describe, error) {
	models := s.d.Models.List(ctx, false)
	// One page load must not pay for three slow reads in a row.
	var props, health, status map[string]any
	var wg sync.WaitGroup
	wg.Add(3)
	go func() { defer wg.Done(); props = s.fetch(ctx, "/props", 6*time.Second) }()
	go func() { defer wg.Done(); health = s.fetch(ctx, "/health", 4*time.Second) }()
	go func() { defer wg.Done(); status = s.fetch(ctx, "/v1/status", 4*time.Second) }()
	wg.Wait()

	var entry map[string]any
	if e := Entries(models); len(e) > 0 {
		entry = e[0]
	}
	dgs := Obj(props, "default_generation_settings")
	nCtx := float64(s.d.Cfg.NCtx)
	if nCtx == 0 {
		nCtx = NumOr(dgs, "n_ctx", 0)
	}
	if nCtx == 0 {
		nCtx = NumOr(health, "max_context", 0)
	}
	if nCtx == 0 {
		nCtx = NumOr(Obj(entry, "meta"), "n_ctx", 0)
	}
	if nCtx == 0 {
		nCtx = 4096
	}

	vision := false
	if forced, on := s.d.Cfg.VisionForced(); forced {
		vision = on
	} else if mod := Obj(props, "modalities"); mod != nil && mod["vision"] != nil {
		vision = Truthy(mod["vision"])
	} else if health != nil {
		vision = Truthy(health["images"])
	}
	s.d.Flags.Vision.Set(vision)
	// the template proves it: Strata ships one that mentions thinking
	if thinkingTemplateRe.MatchString(Str(props, "chat_template")) {
		s.d.Flags.Reasoning.Set(true)
	}

	params := map[string]any{}
	for k, v := range Obj(dgs, "params") {
		params[k] = v
	}
	delete(params, "n_predict")

	if entry != nil {
		entry = copyMap(entry)
		if _, ok := entry["id"]; !ok {
			entry["id"] = Str(health, "model")
		}
	}

	modelPath := ""
	if v, ok := props["model_path"]; ok && v != nil {
		modelPath = asText(v)
	} else if v, ok := props["model_alias"]; ok && v != nil {
		modelPath = asText(v)
	} else {
		modelPath = orValStr(Str(health, "model"), "unknown")
	}

	buildInfo := ""
	if v, ok := props["build_info"]; ok && v != nil {
		buildInfo = asText(v)
	} else if eng := Str(status, "engine"); eng != "" {
		buildInfo = "strata " + eng
	} else {
		buildInfo = "strata"
	}

	totalSlots, hasSlots := Num(props, "total_slots")

	return &Describe{
		Entry:         entry,
		NCtx:          nCtx,
		ModelPath:     modelPath,
		HasModelPath:  true,
		BuildInfo:     buildInfo,
		ChatTemplate:  Str(props, "chat_template"),
		Modalities:    map[string]any{"vision": vision, "audio": false, "video": false},
		TotalSlots:    totalSlots,
		HasTotalSlots: hasSlots,
		Params:        params,
	}, nil
}

func (s *strata) Translate(out map[string]any) {
	mapReasoning(out)
	// Strata knows min_p and stop; only the llama.cpp-only names go, and
	// n_predict becomes max_tokens because Strata reads neither.
	commonTranslate(out, noDrop, s.d.Log)
}

func (s *strata) Prepare(out map[string]any) {}

func (s *strata) RewriteChunk(chunk map[string]any, st *Stream) {
	if t := Obj(chunk, "timings"); t != nil {
		s.specMu.Lock()
		s.lastDraft = NumOr(t, "draft_n", 0)
		s.lastAccpt = NumOr(t, "draft_n_accepted", 0)
		s.hasSpec = true
		s.specMu.Unlock()
	}
	choice := FirstChoice(chunk)
	d := Obj(choice, "delta")
	if d == nil {
		return
	}
	text := Str(d, "content")
	if text == "" {
		text = Str(d, "reasoning_content")
	}
	stamp := func() {
		now := time.Now()
		if st.FirstAt.IsZero() {
			st.FirstAt = now
		}
		st.LastAt = now
	}
	if len(Arr(d, "tool_calls")) > 0 {
		stamp()
	}
	if text == "" {
		return
	}
	stamp()
	// no token ids and no per-token timings in the stream, so the live count is
	// an estimate until the engine's own numbers arrive
	st.Chars += len(text)
	est := int(math.Round(float64(st.Chars) / 4))
	if est > st.Tokens {
		st.Tokens = est
	}
}

// FinalTimings prefers what the engine measured, and falls back to the observed
// stream.
func (s *strata) FinalTimings(st *Stream, pre, post *Snapshot, tSend time.Time) map[string]any {
	timings := st.Inband
	source := "engine"
	if timings == nil {
		timings = WallTimings(st, tSend)
		source = "wall"
	}
	s.d.Log(SpeedLine(source, timings))
	return timings
}

// Snapshot: Strata's /metrics is JSON, not Prometheus. Map the few shared
// fields.
func (s *strata) Snapshot(ctx context.Context, opts SnapshotOpts) (*Snapshot, error) {
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = 8 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var m, status map[string]any
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); m = s.fetchCtx(ctx, "/metrics", 6*time.Second) }()
	go func() { defer wg.Done(); status = s.fetchCtx(ctx, "/v1/status", 4*time.Second) }()
	wg.Wait()
	if m == nil {
		return nil, nil
	}

	t := Obj(m, "totals")
	live := Obj(m, "live")
	running := 0.0
	if state := Str(live, "state"); state != "" && state != "idle" && state != "unloaded" {
		running = 1
	}
	gpu := Obj(Obj(status, "machine"), "gpu")
	spec := Obj(status, "last_timings")

	snap := &Snapshot{At: time.Now(), P: map[string]float64{}, Config: map[string]string{}, Live: live}
	for _, r := range Arr(m, "requests") {
		rm, ok := r.(map[string]any)
		if !ok {
			continue
		}
		snap.Requests = append(snap.Requests, RequestStat{
			PromptMS:   NumOr(rm, "prompt_ms", 0),
			DecodeMS:   NumOr(rm, "decode_ms", 0),
			DecodeTokS: NumOr(rm, "decode_tok_s", 0),
		})
	}
	n := float64(len(snap.Requests))
	var ttftSum, itlSum float64
	for _, r := range snap.Requests {
		ttftSum += r.PromptMS
		itlSum += r.DecodeMS
	}

	snap.P["prefillCount"] = NumOr(t, "requests", 0)
	snap.P["decodeCount"] = NumOr(t, "requests", 0)
	snap.P["computedCount"] = NumOr(t, "requests", 0)
	snap.P["prefillSum"] = NumOr(t, "prompt_ms", 0) / 1000
	snap.P["decodeSum"] = NumOr(t, "decode_ms", 0) / 1000
	snap.P["computedSum"] = maxF(NumOr(t, "prompt_tokens", 0)-NumOr(t, "reused", 0), 0)
	snap.P["promptTokens"] = NumOr(t, "prompt_tokens", 0)
	snap.P["genTokens"] = NumOr(t, "output_tokens", 0)
	snap.P["prefixHits"] = NumOr(t, "reused", 0)
	snap.P["prefixQueries"] = NumOr(t, "prompt_tokens", 0)
	snap.P["running"] = running
	snap.P["waiting"] = NumOr(live, "queued", 0)
	snap.P["ttftCount"] = n
	snap.P["ttftSum"] = ttftSum / 1000
	snap.P["itlCount"] = n
	snap.P["itlSum"] = itlSum / 1000
	snap.Config["model"] = Str(Obj(m, "engine"), "model")
	snap.Config["kv_cache_size_tokens"] = asText(Obj(m, "engine")["max_context"])
	snap.Config["prefix_caching"] = "True"

	if gpu != nil && NumOr(gpu, "total_mib", 0) != 0 {
		used := NumOr(gpu, "used_mib", 0)
		total := NumOr(gpu, "total_mib", 0)
		snap.HasVRAM = true
		snap.VRAMPercent = Round1(used / total * 100)
	}

	// The draft numbers come from this request, or from the last one the stream
	// carried when the engine has forgotten.
	draft, accpt, has := s.lastSpec()
	if spec != nil {
		draft = NumOr(spec, "draft_n", 0)
		accpt = NumOr(spec, "draft_n_accepted", 0)
		has = true
	}
	if has {
		snap.P["specAcceptedTokens"] = accpt
		snap.P["specDraftTokens"] = draft
		snap.P["specDrafts"] = maxF(1, draft-accpt)
	}
	return snap, nil
}

func (s *strata) lastSpec() (float64, float64, bool) {
	s.specMu.Lock()
	defer s.specMu.Unlock()
	return s.lastDraft, s.lastAccpt, s.hasSpec
}

func (s *strata) Engine(snap *Snapshot) map[string]any {
	return map[string]any{
		"model":                nilIfEmpty(snap.ConfigStr("model")),
		"kv_cache_size_tokens": nilIfEmpty(snap.ConfigStr("kv_cache_size_tokens")),
		"serving":              "one request at a time",
	}
}

func (s *strata) Counters(snap *Snapshot) map[string]any {
	reqs := snap.Requests
	n := float64(len(reqs))
	if n == 0 {
		n = 1
	}
	ms := func(pick func(RequestStat) float64) float64 {
		var sum float64
		for _, r := range reqs {
			sum += pick(r)
		}
		return Round1(sum / n)
	}
	var itl []float64
	for _, r := range reqs {
		if r.DecodeTokS > 0 {
			itl = append(itl, r.DecodeTokS)
		}
	}
	avgInterToken := 0.0
	if len(itl) > 0 {
		var sum float64
		for _, v := range itl {
			sum += v
		}
		avgInterToken = Round1(1000 / (sum / float64(len(itl))))
	}
	return map[string]any{
		"prompt_tokens":      snap.Num("promptTokens"),
		"generation_tokens":  snap.Num("genTokens"),
		"requests":           snap.Num("prefillCount"),
		"avg_ttft_ms":        ms(func(r RequestStat) float64 { return r.PromptMS }),
		"avg_inter_token_ms": avgInterToken,
		"avg_queue_ms":       0,
		"avg_prefill_ms":     ms(func(r RequestStat) float64 { return r.PromptMS }),
		"avg_decode_ms":      ms(func(r RequestStat) float64 { return r.DecodeMS }),
		"avg_prefill_tokens": avgInt(snap, "computedSum", "computedCount"),
	}
}

func (s *strata) Gauges(snap *Snapshot) map[string]any {
	pct := func(a, b float64) float64 {
		if b == 0 {
			return 0
		}
		return Round1(a / b * 100)
	}
	vram := any(nil)
	if snap.HasVRAM {
		vram = snap.VRAMPercent
	}
	accept := 0.0
	if snap.Num("specDraftTokens") != 0 {
		accept = pct(snap.Num("specAcceptedTokens"), snap.Num("specDraftTokens"))
	}
	step := 0.0
	if snap.Num("specDraftTokens") != 0 {
		step = Round1(1 + snap.Num("specAcceptedTokens")/snap.Num("specDraftTokens"))
	}
	return map[string]any{
		"requests_running":           snap.Num("running"),
		"requests_waiting":           snap.Num("waiting"),
		"kv_cache_usage_percent":     vram,
		"kv_cache_label":             "VRAM",
		"prefix_cache_hit_percent":   pct(snap.Num("prefixHits"), snap.Num("prefixQueries")),
		"spec_decode_accept_percent": accept,
		"tokens_per_step":            step,
		"preemptions":                0,
	}
}

// fetch reads a control endpoint and drops the error: a backend that does not
// answer one route still works through the others.
func (s *strata) fetch(ctx context.Context, path string, timeout time.Duration) map[string]any {
	out, err := s.d.Up.FetchJSON(ctx, path, timeout)
	if err != nil {
		return nil
	}
	return out
}

func (s *strata) fetchCtx(ctx context.Context, path string, timeout time.Duration) map[string]any {
	out, err := s.d.Up.FetchJSON(ctx, path, timeout)
	if err != nil {
		return nil
	}
	return out
}

func copyMap(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

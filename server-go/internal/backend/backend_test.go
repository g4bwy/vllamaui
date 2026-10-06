package backend

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"llama-webui/server/internal/appconf"
)

// testDeps builds a Deps that talks to nothing.
func testDeps(cfg *appconf.Config, log *[]string) *Deps {
	if cfg.Upstream == "" {
		cfg.Upstream = "http://127.0.0.1:1"
	}
	return NewDeps(cfg, func(format string, v ...any) {
		if log != nil {
			*log = append(*log, strings.TrimSpace(fmt.Sprintf(format, v...)))
		}
	})
}

func TestVLLMTranslateDropsAndMaps(t *testing.T) {
	var log []string
	d := testDeps(&appconf.Config{}, &log)
	b, err := New("vllm", d)
	if err != nil {
		t.Fatal(err)
	}

	out := map[string]any{
		"n_predict":    json.Number("0"),
		"min_p":        json.Number("0.1"),
		"logit_bias":   map[string]any{"1": 2.0},
		"n_ctx":        json.Number("4096"),
		"cache_prompt": true,
		"grammar":      "root ::= 'a'",
		"temperature":  json.Number("0.7"),
	}
	b.Translate(out)

	if got := out["max_tokens"]; got != json.Number("1") {
		t.Errorf("n_predict 0 must become max_tokens 1, got %v", got)
	}
	if _, ok := out["n_predict"]; ok {
		t.Error("n_predict must be gone")
	}
	for _, gone := range []string{"min_p", "logit_bias", "n_ctx", "cache_prompt", "grammar"} {
		if _, ok := out[gone]; ok {
			t.Errorf("%s must be dropped by default", gone)
		}
	}
	if out["temperature"] != json.Number("0.7") {
		t.Error("a param both engines read must survive untouched")
	}
	if len(log) == 0 || !strings.HasPrefix(log[0], "dropped params: ") {
		t.Fatalf("expected one dropped-params line, got %v", log)
	}
	if !strings.Contains(log[0], "min_p") || !strings.Contains(log[0], "grammar") {
		t.Errorf("drop line must name what went: %s", log[0])
	}
}

func TestVLLMKeepUnsupportedOverride(t *testing.T) {
	d := testDeps(&appconf.Config{KeepUnsupported: true}, nil)
	b, _ := New("vllm", d)
	out := map[string]any{"min_p": json.Number("0.1"), "logit_bias": map[string]any{}}
	b.Translate(out)
	if _, ok := out["min_p"]; !ok {
		t.Error("VLLM_KEEP_UNSUPPORTED=1 must send min_p")
	}
	if _, ok := out["logit_bias"]; !ok {
		t.Error("VLLM_KEEP_UNSUPPORTED=1 must send logit_bias")
	}
}

func TestTranslateNegativeAndZeroTokenCaps(t *testing.T) {
	d := testDeps(&appconf.Config{}, nil)
	b, _ := New("vllm", d)

	out := map[string]any{"n_predict": json.Number("-1")}
	b.Translate(out)
	if _, ok := out["max_tokens"]; ok {
		t.Error("a negative cap must be removed, not sent")
	}

	out = map[string]any{"max_tokens": json.Number("0")}
	b.Translate(out)
	if out["max_tokens"] != json.Number("1") {
		t.Errorf("max_tokens 0 must become 1, got %v", out["max_tokens"])
	}

	out = map[string]any{"max_tokens": json.Number("-2")}
	b.Translate(out)
	if _, ok := out["max_tokens"]; ok {
		t.Error("max_tokens -2 must be removed")
	}
}

func TestTranslateThinkingSwitch(t *testing.T) {
	d := testDeps(&appconf.Config{}, nil)
	b, _ := New("strata", d)
	out := map[string]any{
		"reasoning_format":     "none",
		"chat_template_kwargs": map[string]any{"use_cache": true},
	}
	b.Translate(out)
	kw := Obj(out, "chat_template_kwargs")
	if Bool(kw, "enable_thinking") {
		t.Error("reasoning_format none must turn thinking off")
	}
	if kw["enable_thinking"] != false {
		t.Errorf("enable_thinking must be the boolean false, got %v", kw["enable_thinking"])
	}
	if !Bool(kw, "use_cache") {
		t.Error("existing template kwargs must be kept")
	}
}

func TestStrataKeepsWhatItReads(t *testing.T) {
	d := testDeps(&appconf.Config{}, nil)
	b, _ := New("strata", d)
	out := map[string]any{"min_p": json.Number("0.1"), "stop": []any{"x"}, "slot_id": json.Number("2")}
	b.Translate(out)
	if _, ok := out["min_p"]; !ok {
		t.Error("Strata reads min_p")
	}
	if _, ok := out["stop"]; !ok {
		t.Error("Strata reads stop")
	}
	if _, ok := out["slot_id"]; ok {
		t.Error("slot_id is llama.cpp only and must go for both engines")
	}
}

func TestPrepareAsksForUsageOnVLLMOnly(t *testing.T) {
	d := testDeps(&appconf.Config{}, nil)
	v, _ := New("vllm", d)
	out := map[string]any{"stream": true}
	v.Prepare(out)
	if !Bool(Obj(out, "stream_options"), "include_usage") {
		t.Error("a streamed vLLM answer needs usage on the last chunk")
	}
	if !Bool(out, "return_token_ids") {
		t.Error("return_token_ids is how the exact token count arrives")
	}

	nostream := map[string]any{}
	v.Prepare(nostream)
	if _, ok := nostream["stream_options"]; ok {
		t.Error("stream_options belongs to a streamed request only")
	}

	s, _ := New("strata", d)
	str := map[string]any{"stream": true}
	s.Prepare(str)
	if _, ok := str["stream_options"]; ok {
		t.Error("Strata already reports usage, so it needs no options")
	}
	if _, ok := str["return_token_ids"]; ok {
		t.Error("Strata has no token ids")
	}
}

func TestChunkTokenCounts(t *testing.T) {
	d := testDeps(&appconf.Config{}, nil)
	v, _ := New("vllm", d)
	st := &Stream{}
	v.RewriteChunk(jsonMap(`{"choices":[{"index":0,"delta":{"reasoning":"a thought"},"token_ids":[1,2,3]}]}`), st)
	if st.Tokens != 3 {
		t.Errorf("vLLM counts ids, got %d", st.Tokens)
	}
	if !d.Flags.Reasoning.Get() {
		t.Error("a reasoning delta must turn the thinking flag on")
	}
	v.RewriteChunk(jsonMap(`{"choices":[{"index":0,"delta":{"content":"hello"}}]}`), st)
	if st.Tokens != 4 {
		t.Errorf("a text chunk with no ids counts as one, got %d", st.Tokens)
	}
	if st.FirstAt.IsZero() || st.LastAt.Before(st.FirstAt) {
		t.Error("text chunks stamp first and last time")
	}

	s := newStrata(&Deps{Cfg: &appconf.Config{}, Log: func(string, ...any) {}, Flags: &Flags{}})
	st = &Stream{}
	for i := 0; i < 4; i++ {
		s.RewriteChunk(jsonMap(`{"choices":[{"index":0,"delta":{"content":"12345678"}}]}`), st)
	}
	if st.Tokens != 8 {
		t.Errorf("Strata estimates a token per four characters, got %d", st.Tokens)
	}
}

func TestRatesFromGuards(t *testing.T) {
	keys := RateKeys{Gen: "genTokens", Prompt: "promptTokens", Drafts: "specDrafts", DraftTokens: "specDraftTokens", Accepted: "specAcceptedTokens"}
	snap := func(gen, prompt float64) *Snapshot {
		return &Snapshot{P: map[string]float64{"genTokens": gen, "promptTokens": prompt, "specDrafts": 1, "specDraftTokens": 10, "specAcceptedTokens": 5}}
	}
	t0 := time.Unix(1737000000, 0)

	first, win := RatesFrom(snap(10, 100), t0, keys, nil)
	if len(first) != 0 {
		t.Errorf("the first sample has no window to measure: %v", first)
	}

	// A clean 10 s window.
	rates, _ := RatesFrom(snap(110, 210), t0.Add(10*time.Second), keys, win)
	if got := rates["generation_tokens_per_second"]; got != 10.0 {
		t.Errorf("generation rate = %v, want 10", got)
	}
	if got := rates["prompt_tokens_per_second"]; got != 11.0 {
		t.Errorf("prompt rate = %v, want 11", got)
	}
	if got := rates["window_s"]; got != 10.0 {
		t.Errorf("window_s = %v, want 10", got)
	}
	if _, ok := rates["window_accept_percent"]; ok {
		t.Error("no draft movement must report no acceptance percent")
	}

	// An engine restart zeroes the counters.
	back, _ := RatesFrom(snap(1, 1), t0.Add(10*time.Second), keys, win)
	if len(back) != 0 {
		t.Errorf("a counter going backwards must drop the window: %v", back)
	}

	// A window too short to mean anything.
	short, _ := RatesFrom(snap(11, 101), t0.Add(100*time.Millisecond), keys, win)
	if len(short) != 1 || short["window_s"] == nil {
		t.Errorf("a 0.1 s window must only report its length: %v", short)
	}

	// A window that mostly covers idle time.
	long, _ := RatesFrom(snap(11, 101), t0.Add(300*time.Second), keys, win)
	if _, ok := long["generation_tokens_per_second"]; ok {
		t.Errorf("a 300 s window must not report a current rate: %v", long)
	}

	// An engine that does not accumulate drafts must not get that key.
	slow := RateKeys{Gen: "genTokens", Prompt: "promptTokens"}
	plain, _ := RatesFrom(snap(110, 210), t0.Add(10*time.Second), slow, win)
	if _, ok := plain["steps_per_second"]; ok {
		t.Errorf("Strata has no draft counter, so no steps_per_second: %v", plain)
	}
}

func TestSpeedLineFormat(t *testing.T) {
	line := SpeedLine("engine", map[string]any{
		"prompt_n": 63.0, "prompt_ms": 84.9, "predicted_n": 900.0, "predicted_ms": 9171.0, "cache_n": 0.0,
	})
	if line != "timings(engine) pp 63 tok 742.0 t/s | tg 900 tok 98.1 t/s | cache 0" {
		t.Errorf("unexpected log line: %s", line)
	}
	// Counts with no measured window behind them get no rate.
	line = SpeedLine("wall", map[string]any{
		"prompt_n": 338.0, "predicted_n": 38.0, "predicted_ms": 550.0, "cache_n": 0.0,
	})
	if line != "timings(wall) pp 338 tok - t/s | tg 38 tok 69.1 t/s | cache 0" {
		t.Errorf("unexpected log line: %s", line)
	}
}

// ---------- attribution of the vLLM histogram window ----------

// hist is one scrape of the counters the attribution test reads, plus the two
// gauges a shared server always carries.
type hist struct {
	prefillCount  float64
	decodeCount   float64
	computedCount float64
	prefillSum    float64
	decodeSum     float64
	computedSum   float64
	running       float64
	waiting       float64
}

func (h hist) snapshot() *Snapshot {
	return &Snapshot{P: map[string]float64{
		"prefillCount":  h.prefillCount,
		"decodeCount":   h.decodeCount,
		"computedCount": h.computedCount,
		"prefillSum":    h.prefillSum,
		"decodeSum":     h.decodeSum,
		"computedSum":   h.computedSum,
		"running":       h.running,
		"waiting":       h.waiting,
	}}
}

// measuredPre is the opening scrape of the window, taken on a box that serves
// three requests at a time with four more waiting. Nothing about it is idle.
var measuredPre = hist{
	prefillCount: 1000, decodeCount: 1000, computedCount: 1000,
	prefillSum: 200, decodeSum: 400, computedSum: 300000,
	running: 3, waiting: 4,
}

// after is the closing scrape, given what moved during the window: the three
// request counts, the prefill and decode time in milliseconds, and the tokens
// the engine computed rather than reused.
func after(dPrefill, dDecode, dComputed, prefillMS, decodeMS, computed float64) hist {
	return hist{
		prefillCount:  measuredPre.prefillCount + dPrefill,
		decodeCount:   measuredPre.decodeCount + dDecode,
		computedCount: measuredPre.computedCount + dComputed,
		prefillSum:    measuredPre.prefillSum + prefillMS/1000,
		decodeSum:     measuredPre.decodeSum + decodeMS/1000,
		computedSum:   measuredPre.computedSum + computed,
		running:       measuredPre.running,
		waiting:       measuredPre.waiting,
	}
}

// measuredStream is our own request as the stream reported it: a prompt of
// total tokens, 38 tokens of output, the first token ttftMS after the send, and
// the last one 550 ms after that. vLLM answers prompt_tokens_details as null on
// this build, so the usage chunk says nothing about reuse.
func measuredStream(total float64, ttftMS int64) (*Stream, time.Time) {
	tSend := time.Unix(1737000000, 0)
	return &Stream{
		FirstAt: tSend.Add(time.Duration(ttftMS) * time.Millisecond),
		LastAt:  tSend.Add(time.Duration(ttftMS+550) * time.Millisecond),
		Tokens:  38,
		Usage:   jsonMap(fmt.Sprintf(`{"prompt_tokens":%d,"completion_tokens":38}`, int(total))),
	}, tSend
}

func TestVLLMFinalTimingsAttribution(t *testing.T) {
	cases := []struct {
		name     string
		post     hist
		total    float64
		ttft     int64
		attribut bool
		wantVals map[string]float64
		wantLog  string
	}{
		{
			// The engine truth for this request was 336 computed tokens in
			// 198 ms of prefill. Dividing the same tokens by the 229 ms of
			// wall time instead gives 1467.2, the wrong number this replaced.
			name:     "one finished request on a busy server",
			post:     after(1, 1, 1, 198, 400, 336),
			total:    338,
			ttft:     229,
			attribut: true,
			wantVals: map[string]float64{"prompt_n": 336, "prompt_ms": 198, "cache_n": 2, "predicted_n": 38, "predicted_ms": 400},
			wantLog:  "timings(engine) pp 336 tok 1697.0 t/s | tg 38 tok 95.0 t/s | cache 2",
		},
		{
			// A warm prefix: 363 of the 383 tokens were reused, so only 20
			// count as work, in 15 ms.
			name:     "warm prefix, most of the prompt reused",
			post:     after(1, 1, 1, 15, 400, 20),
			total:    383,
			ttft:     240,
			attribut: true,
			wantVals: map[string]float64{"prompt_n": 20, "prompt_ms": 15, "cache_n": 363, "predicted_ms": 400},
			wantLog:  "timings(engine) pp 20 tok 1333.3 t/s | tg 38 tok 95.0 t/s | cache 363",
		},
		{
			// A foreign request finished: it cannot have taken three seconds
			// of prefill when our first token arrived after 229 ms.
			name:     "prefill longer than the wait for the first token",
			post:     after(1, 1, 1, 3000, 400, 336),
			total:    338,
			ttft:     229,
			wantVals: map[string]float64{"prompt_n": 338, "cache_n": 0, "predicted_ms": 550},
			wantLog:  "timings(wall) pp 338 tok - t/s | tg 38 tok 69.1 t/s | cache 0",
		},
		{
			// Our first token was queue wait, not the prefill on offer: 2000
			// ms is past 3x1 ms plus the 250 ms of slack.
			name:     "queue dominated window",
			post:     after(1, 1, 1, 1, 400, 336),
			total:    338,
			ttft:     2000,
			wantVals: map[string]float64{"prompt_n": 338, "cache_n": 0},
			wantLog:  "timings(wall) pp 338 tok - t/s | tg 38 tok 69.1 t/s | cache 0",
		},
		{
			name:     "computed more tokens than the whole prompt",
			post:     after(1, 1, 1, 198, 400, 400),
			total:    338,
			ttft:     229,
			wantVals: map[string]float64{"prompt_n": 338, "cache_n": 0},
			wantLog:  "timings(wall) pp 338 tok - t/s | tg 38 tok 69.1 t/s | cache 0",
		},
		{
			name:     "nothing was computed",
			post:     after(1, 1, 1, 198, 400, 0),
			total:    338,
			ttft:     229,
			wantVals: map[string]float64{"prompt_n": 338, "cache_n": 0},
			wantLog:  "timings(wall) pp 338 tok - t/s | tg 38 tok 69.1 t/s | cache 0",
		},
		{
			name:     "no prefill finished",
			post:     after(0, 1, 1, 198, 400, 336),
			total:    338,
			ttft:     229,
			wantVals: map[string]float64{"prompt_n": 338, "cache_n": 0},
			wantLog:  "timings(wall) pp 338 tok - t/s | tg 38 tok 69.1 t/s | cache 0",
		},
		{
			name:     "two prefills finished",
			post:     after(2, 2, 2, 198, 400, 336),
			total:    338,
			ttft:     229,
			wantVals: map[string]float64{"prompt_n": 338, "cache_n": 0},
			wantLog:  "timings(wall) pp 338 tok - t/s | tg 38 tok 69.1 t/s | cache 0",
		},
		{
			name:     "no decode finished",
			post:     after(1, 0, 1, 198, 400, 336),
			total:    338,
			ttft:     229,
			wantVals: map[string]float64{"prompt_n": 338, "cache_n": 0},
			wantLog:  "timings(wall) pp 338 tok - t/s | tg 38 tok 69.1 t/s | cache 0",
		},
		{
			name:     "prefill time moved backwards",
			post:     after(1, 1, 1, -5, 400, 336),
			total:    338,
			ttft:     229,
			wantVals: map[string]float64{"prompt_n": 338, "cache_n": 0},
			wantLog:  "timings(wall) pp 338 tok - t/s | tg 38 tok 69.1 t/s | cache 0",
		},
		{
			name:     "the computed count moved backwards",
			post:     after(1, 1, -1, 198, 400, 336),
			total:    338,
			ttft:     229,
			wantVals: map[string]float64{"prompt_n": 338, "cache_n": 0},
			wantLog:  "timings(wall) pp 338 tok - t/s | tg 38 tok 69.1 t/s | cache 0",
		},
		{
			// The engine restarted mid-request and every counter started over.
			name:     "the counters restarted",
			post:     hist{},
			total:    338,
			ttft:     229,
			wantVals: map[string]float64{"prompt_n": 338, "cache_n": 0},
			wantLog:  "timings(wall) pp 338 tok - t/s | tg 38 tok 69.1 t/s | cache 0",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var log []string
			d := testDeps(&appconf.Config{}, &log)
			st, tSend := measuredStream(tc.total, tc.ttft)
			post := tc.post
			timings := newVLLM(d).FinalTimings(st, measuredPre.snapshot(), post.snapshot(), tSend)

			if len(log) != 1 || log[0] != tc.wantLog {
				t.Errorf("log = %v, want %q", log, tc.wantLog)
			}
			if _, ok := timings["prompt_ms"]; ok != tc.attribut {
				t.Errorf("prompt_ms present = %v, want %v", ok, tc.attribut)
			}
			for k, want := range tc.wantVals {
				if got := NumOr(timings, k, -1); got != want {
					t.Errorf("%s = %v, want %v", k, got, want)
				}
			}
		})
	}
}

// TestVLLMFinalTimingsWithoutSnapshots: a dead /metrics leaves the window empty.
func TestVLLMFinalTimingsWithoutSnapshots(t *testing.T) {
	var log []string
	d := testDeps(&appconf.Config{}, &log)
	st, tSend := measuredStream(338, 229)
	b := newVLLM(d)
	for _, c := range []struct {
		name      string
		pre, post *Snapshot
	}{
		{"no snapshots", nil, nil},
		{"no opening snapshot", nil, measuredPre.snapshot()},
		{"no closing snapshot", measuredPre.snapshot(), nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			timings := b.FinalTimings(st, c.pre, c.post, tSend)
			if _, ok := timings["prompt_ms"]; ok {
				t.Errorf("no window means no prompt rate: %v", timings)
			}
			if !strings.Contains(log[len(log)-1], "timings(wall)") {
				t.Errorf("log = %s, want the wall fallback", log[len(log)-1])
			}
		})
	}
}

// TestWallTimingsAreCountsOnly: the fallback keeps the counts the UI adds up for
// the context gauge, and never carries a prompt rate.
func TestWallTimingsAreCountsOnly(t *testing.T) {
	st, _ := measuredStream(338, 229)
	timings := WallTimings(st)
	if got := sortedKeysOf(timings); strings.Join(got, " ") != "cache_n predicted_ms predicted_n prompt_n" {
		t.Errorf("key set = %v, want the counts and the decode window", got)
	}
	if _, ok := timings["prompt_ms"]; ok {
		t.Error("the send-to-first-token window is queueing plus prefill, so it gives no prompt rate")
	}
	if got := NumOr(timings, "prompt_n", -1); got != 338 {
		t.Errorf("with no cached_tokens field the whole prompt counts as prompt_n, got %v", got)
	}
	if got := NumOr(timings, "cache_n", -1); got != 0 {
		t.Errorf("cache_n = %v, want 0", got)
	}
	if got := NumOr(timings, "predicted_ms", -1); got != 550 {
		t.Errorf("decode stays, since it is directly observed: predicted_ms = %v", got)
	}

	// The same request on a build that does report reuse.
	st.Usage = jsonMap(`{"prompt_tokens":338,"completion_tokens":38,"prompt_tokens_details":{"cached_tokens":300}}`)
	timings = WallTimings(st)
	if got := NumOr(timings, "prompt_n", -1); got != 38 {
		t.Errorf("prompt_n = %v, want the 38 tokens left after reuse", got)
	}
	if got := NumOr(timings, "cache_n", -1); got != 300 {
		t.Errorf("cache_n = %v, want 300", got)
	}
	if _, ok := timings["prompt_ms"]; ok {
		t.Error("a reported cache does not make the wall window a prefill")
	}
}

func sortedKeysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func jsonMap(text string) map[string]any {
	out, err := DecodeJSON(text)
	if err != nil {
		panic(err)
	}
	return out
}

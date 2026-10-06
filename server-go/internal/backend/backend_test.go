package backend

import (
	"encoding/json"
	"fmt"
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
}

func jsonMap(text string) map[string]any {
	out, err := DecodeJSON(text)
	if err != nil {
		panic(err)
	}
	return out
}

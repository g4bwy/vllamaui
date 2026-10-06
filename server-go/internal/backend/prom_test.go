package backend

import (
	"math"
	"strings"
	"testing"
)

func TestParsePrometheusSamples(t *testing.T) {
	text := strings.Join([]string{
		"# HELP vllm:num_requests_running Number of running requests",
		"# TYPE vllm:num_requests_running gauge",
		"vllm:num_requests_running 3",
		`vllm:num_requests_waiting{model_name="Qwen3.8 Flash",engine="0"} 2`,
		`vllm:prompt_tokens_total{model_name="a b c"} 1200 1737000000000`,
		"vllm:generation_tokens_total 40",
		`vllm:prefix_cache_hits_total{model="x"} 5`,
		`vllm:prefix_cache_hits_total{model="y"} 7.5`,
		"vllm:time_to_first_token_seconds_count 4",
		"vllm:time_to_first_token_seconds_sum 1.25",
		"vllm:spec_decode_num_drafts_total 4",
		"vllm:num_preemptions_total 0",
		"unrelated_metric 9",
		"vllm:not_in_the_map_total 9",
		"vllm:kv_cache_usage_perc NaN",
		"vllm:cache_config_info{block_size=\"16\"} 1",
		"",
	}, "\n")

	got := ParsePrometheus(text)

	want := map[string]float64{
		"running":      3,
		"waiting":      2,
		"promptTokens": 1200,
		"genTokens":    40,
		"prefixHits":   12.5,
		"ttftCount":    4,
		"ttftSum":      1.25,
		"specDrafts":   4,
		"preemptions":  0,
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %v, want %v", k, got[k], v)
		}
	}
	if _, ok := got["kvUsage"]; ok {
		t.Error("a non-finite value must be skipped, not summed")
	}
	if len(got) != len(want) {
		t.Errorf("unexpected fields: %v", got)
	}
}

func TestParsePrometheusValueForms(t *testing.T) {
	for _, tc := range []struct {
		line string
		want float64
		ok   bool
	}{
		{"vllm:generation_tokens_total 7", 7, true},
		{"vllm:generation_tokens_total 1e3", 1000, true},
		{"vllm:generation_tokens_total 0.5", 0.5, true},
		{"vllm:generation_tokens_total 7 1737000000000", 7, true},
		{`vllm:generation_tokens_total{a="b"} 7`, 7, true},
		{`vllm:generation_tokens_total{a="b c",d="e"} 7 123456`, 7, true},
		{"vllm:generation_tokens_total", 0, false},
		{"vllm:generation_tokens_total abc", 0, false},
		{"  vllm:generation_tokens_total 7", 0, false},
	} {
		got := ParsePrometheus(tc.line)
		v, ok := got["genTokens"]
		if tc.ok && (!ok || v != tc.want) {
			t.Errorf("%q parsed to %v, %v; want %v", tc.line, v, ok, tc.want)
		}
		if !tc.ok && ok {
			t.Errorf("%q must not parse, got %v", tc.line, v)
		}
	}
}

func TestParsePrometheusSkipsNonFinite(t *testing.T) {
	for _, bad := range []string{"NaN", "Inf", "-Inf", "+Inf", "nan"} {
		got := ParsePrometheus("vllm:prompt_tokens_total " + bad)
		if v, ok := got["promptTokens"]; ok {
			t.Errorf("%s must be skipped, got %v", bad, v)
		}
	}
	if v := ParsePrometheus("vllm:prompt_tokens_total 1e400")["promptTokens"]; !math.IsInf(v, 0) {
		t.Logf("1e400 parsed to %v; Go accepts it as +Inf and the sum stays finite only by luck", v)
	}
}

func TestParseCacheConfig(t *testing.T) {
	text := strings.Join([]string{
		"vllm:something_else 1",
		`vllm:cache_config_info{_block_size=16,block_size="16",num_gpu_blocks="100",kv_cache_size_tokens="1600",enable_prefix_caching="True",gpu_memory_utilization="0.9"} 1`,
		"",
	}, "\n")
	got := ParseCacheConfig(text)
	if got["block_size"] != "16" {
		t.Errorf("block_size = %q", got["block_size"])
	}
	if got["kv_cache_size_tokens"] != "1600" {
		t.Errorf("kv_cache_size_tokens = %q", got["kv_cache_size_tokens"])
	}
	if got["enable_prefix_caching"] != "True" {
		t.Errorf("enable_prefix_caching = %q", got["enable_prefix_caching"])
	}
	if _, ok := got["_block_size"]; ok {
		t.Error("an unquoted label is not a string label")
	}
	if len(got) != 5 {
		t.Errorf("unexpected labels: %v", got)
	}
}

func TestParseCacheConfigAbsent(t *testing.T) {
	if got := ParseCacheConfig("vllm:prompt_tokens_total 1\n"); len(got) != 0 {
		t.Errorf("want no labels, got %v", got)
	}
}

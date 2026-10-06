package backend

import (
	"context"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// prometheusNames maps a vLLM metric to the shared snapshot field. Only vLLM
// publishes these; Strata serves JSON on the same path instead.
var prometheusNames = map[string]string{
	"vllm:request_prefill_time_seconds_sum":         "prefillSum",
	"vllm:request_prefill_time_seconds_count":       "prefillCount",
	"vllm:request_decode_time_seconds_sum":          "decodeSum",
	"vllm:request_decode_time_seconds_count":        "decodeCount",
	"vllm:request_queue_time_seconds_sum":           "queueSum",
	"vllm:time_to_first_token_seconds_sum":          "ttftSum",
	"vllm:time_to_first_token_seconds_count":        "ttftCount",
	"vllm:inter_token_latency_seconds_sum":          "itlSum",
	"vllm:inter_token_latency_seconds_count":        "itlCount",
	"vllm:request_prefill_kv_computed_tokens_sum":   "computedSum",
	"vllm:request_prefill_kv_computed_tokens_count": "computedCount",
	"vllm:prefix_cache_hits_total":                  "prefixHits",
	"vllm:prefix_cache_queries_total":               "prefixQueries",
	"vllm:prompt_tokens_total":                      "promptTokens",
	"vllm:generation_tokens_total":                  "genTokens",
	"vllm:num_requests_running":                     "running",
	"vllm:num_requests_waiting":                     "waiting",
	"vllm:kv_cache_usage_perc":                      "kvUsage",
	"vllm:spec_decode_num_drafts_total":             "specDrafts",
	"vllm:spec_decode_num_draft_tokens_total":       "specDraftTokens",
	"vllm:spec_decode_num_accepted_tokens_total":    "specAcceptedTokens",
	"vllm:num_preemptions_total":                    "preemptions",
}

// sampleRe reads one Prometheus sample: a name, an optional label set, a value,
// and an optional trailing timestamp.
var sampleRe = regexp.MustCompile(`^(\S+?)(?:\{[^\n]*\})? (\S+)(?: \d+)?$`)

var labelRe = regexp.MustCompile(`([a-zA-Z_0-9]+)="([^"]*)"`)

// ParsePrometheus folds every label set of a metric into one number.
func ParsePrometheus(text string) map[string]float64 {
	out := map[string]float64{}
	for _, line := range strings.Split(text, "\n") {
		if line == "" || line[0] == '#' {
			continue
		}
		m := sampleRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		field, ok := prometheusNames[m[1]]
		if !ok {
			continue
		}
		v, err := strconv.ParseFloat(m[2], 64)
		if err != nil || math.IsNaN(v) || math.IsInf(v, 0) {
			continue
		}
		out[field] += v
	}
	return out
}

// ParseCacheConfig reads the KV layout, which vLLM publishes in one labelled
// Prometheus line rather than as JSON.
func ParseCacheConfig(text string) map[string]string {
	out := map[string]string{}
	for _, line := range strings.Split(text, "\n") {
		if !strings.HasPrefix(line, "vllm:cache_config_info") {
			continue
		}
		for _, m := range labelRe.FindAllStringSubmatch(line, -1) {
			out[m[1]] = m[2]
		}
		return out
	}
	return out
}

// PromCache reads /metrics once and keeps the parse for a moment, so several
// browser tabs cost one upstream read.
type PromCache struct {
	up *Upstream

	mu     sync.Mutex
	at     time.Time
	parsed *Snapshot
}

const promTTL = 1500 * time.Millisecond

// NewPromCache builds the cache over an upstream.
func NewPromCache(up *Upstream) *PromCache { return &PromCache{up: up} }

// Snapshot returns the counters. A forced read is what a timing window needs:
// two ends of a window served from one cache entry make the delta zero.
func (p *PromCache) Snapshot(ctx context.Context, opts SnapshotOpts) (*Snapshot, error) {
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = 8 * time.Second
	}
	p.mu.Lock()
	fresh := p.parsed != nil && time.Since(p.at) < promTTL
	p.mu.Unlock()
	if opts.Force || !fresh {
		// The deadline starts here, so it also bounds waiting on a slow scrape.
		ctx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		res, err := p.up.Do(ctx, "GET", "/metrics", nil)
		if err != nil {
			return nil, err
		}
		defer res.Body.Close()
		if res.StatusCode < 200 || res.StatusCode > 299 {
			return nil, statusErr("/metrics", res.StatusCode)
		}
		text, err := readAll(res.Body)
		if err != nil {
			return nil, err
		}
		snap := &Snapshot{At: time.Now(), P: ParsePrometheus(text), Config: ParseCacheConfig(text)}
		p.mu.Lock()
		p.at = snap.At
		p.parsed = snap
		p.mu.Unlock()
		return snap, nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.parsed, nil
}

func statusErr(path string, code int) error {
	return fmt.Errorf("%s returned HTTP %d", path, code)
}

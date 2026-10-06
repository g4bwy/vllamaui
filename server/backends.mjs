// Backend definitions for the adapter. Each one knows how to describe itself,
// what to send upstream, and where its throughput numbers come from.
//
// vLLM speaks plain OpenAI: no timings, no /props, no idea what it is capable
// of, and its numbers live in Prometheus counters that mix every client.
//
// Strata (github.com/Niko1221/Strata) is a llama.cpp-style server: it answers
// /props, /slots and /health, names the thinking field the way the webui
// expects, and puts a llama.cpp-shaped timings object on the last chunk.

// Prometheus name -> shared snapshot field. Only vLLM publishes these.
const PROMETHEUS = new Map([
	['vllm:request_prefill_time_seconds_sum', 'prefillSum'],
	['vllm:request_prefill_time_seconds_count', 'prefillCount'],
	['vllm:request_decode_time_seconds_sum', 'decodeSum'],
	['vllm:request_decode_time_seconds_count', 'decodeCount'],
	['vllm:request_queue_time_seconds_sum', 'queueSum'],
	['vllm:time_to_first_token_seconds_sum', 'ttftSum'],
	['vllm:time_to_first_token_seconds_count', 'ttftCount'],
	['vllm:inter_token_latency_seconds_sum', 'itlSum'],
	['vllm:inter_token_latency_seconds_count', 'itlCount'],
	['vllm:request_prefill_kv_computed_tokens_sum', 'computedSum'],
	['vllm:request_prefill_kv_computed_tokens_count', 'computedCount'],
	['vllm:prefix_cache_hits_total', 'prefixHits'],
	['vllm:prefix_cache_queries_total', 'prefixQueries'],
	['vllm:prompt_tokens_total', 'promptTokens'],
	['vllm:generation_tokens_total', 'genTokens'],
	['vllm:num_requests_running', 'running'],
	['vllm:num_requests_waiting', 'waiting'],
	['vllm:kv_cache_usage_perc', 'kvUsage'],
	['vllm:spec_decode_num_drafts_total', 'specDrafts'],
	['vllm:spec_decode_num_draft_tokens_total', 'specDraftTokens'],
	['vllm:spec_decode_num_accepted_tokens_total', 'specAcceptedTokens'],
	['vllm:num_preemptions_total', 'preemptions']
]);

const SAMPLE = /^(\S+?)(?:\{[^\n]*\})? (\S+)(?: \d+)?$/;

export function parsePrometheus(text) {
	const out = {};
	for (const line of text.split('\n')) {
		if (!line || line[0] === '#') continue;
		const m = SAMPLE.exec(line);
		if (!m) continue;
		const field = PROMETHEUS.get(m[1]);
		if (!field) continue;
		const v = Number(m[2]);
		if (Number.isFinite(v)) out[field] = (out[field] || 0) + v;
	}
	return out;
}

const round1 = (n) => Math.round(n * 10) / 10;

// llama.cpp-only fields an OpenAI server has no use for
const DROP_LLAMA_ONLY = new Set([
	'return_progress',
	'sse_ping_interval',
	'timings_per_token',
	'backend_sampling',
	'reasoning_control',
	'thinking_budget_tokens',
	'samplers',
	'mirostat',
	'mirostat_tau',
	'mirostat_eta',
	'xtc_probability',
	'xtc_threshold',
	'typ_p',
	'dynatemp_range',
	'dynatemp_exponent',
	'dry_multiplier',
	'dry_base',
	'dry_allowed_length',
	'dry_penalty_last_n',
	'dry_sequence_breakers',
	'top_n_sigma',
	'n_keep',
	'n_discard',
	'min_keep',
	'n_probs',
	'post_sampling_probs',
	'grammar',
	'grammar_lazy',
	'grammar_triggers',
	'preserved_tokens',
	'chat_format',
	'reasoning_in_content',
	'generation_prompt',
	'lora',
	'slot_id',
	'nid',
	'cache_prompt',
	'normalize_prefix',
	'penalize_nl',
	'n_ctx'
]);

// shared: map the UI's thinking switch onto the template kwarg both engines read
function mapReasoning(out) {
	if (out.reasoning_format === 'none') {
		out.chat_template_kwargs = { ...(out.chat_template_kwargs || {}), enable_thinking: false };
		return true;
	}
	return false;
}

function commonTranslate(out, drop, dropped) {
	for (const k of DROP_LLAMA_ONLY) if (k in out) dropped.push(k);
	for (const k of drop) if (k in out) dropped.push(k);
	for (const k of dropped) delete out[k];

	if (typeof out.n_predict === 'number') {
		// n_predict 0 is the prompt warm-up call: process the prompt, generate
		// nothing. Neither engine has that, so ask for the minimum.
		out.max_tokens = out.n_predict === 0 ? 1 : out.n_predict;
		delete out.n_predict;
	}
	if (out.max_tokens === 0) out.max_tokens = 1;
	if (typeof out.max_tokens === 'number' && out.max_tokens < 0) delete out.max_tokens;
	return out;
}

// A wall-clock timings object from what the stream itself told us. Used by
// every backend when the engine reported nothing of its own.
function wallTimings(st, usage, tSend) {
	const promptTotal = usage?.prompt_tokens ?? 0;
	const cached = usage?.prompt_tokens_details?.cached_tokens ?? 0;
	const predicted_n = usage?.completion_tokens ?? st.tokens;
	const timings = {
		predicted_n,
		predicted_ms: round1(st.firstAt && st.lastAt ? Math.max(st.lastAt - st.firstAt, 1) : 0)
	};
	const prompt_n = Math.max(promptTotal - cached, 0);
	if (cached > 0) timings.cache_n = cached;
	if (prompt_n > 0 && st.firstAt) {
		timings.prompt_n = prompt_n;
		timings.prompt_ms = round1(Math.max(st.firstAt - tSend, 1));
	}
	return timings;
}

function logSpeed(source, timings) {
	const pp = timings.prompt_ms ? ((timings.prompt_n || 0) / timings.prompt_ms) * 1000 : 0;
	const tg = timings.predicted_ms ? (timings.predicted_n / timings.predicted_ms) * 1000 : 0;
	return `timings(${source}) pp ${timings.prompt_n || 0} tok ${pp.toFixed(1)} t/s | tg ${timings.predicted_n} tok ${tg.toFixed(1)} t/s | cache ${timings.cache_n || 0}`;
}

// ---------------------------------------------------------------- vLLM

// vLLM describes its KV layout in one labelled Prometheus line, not JSON
export function parseCacheConfig(text) {
	const line = text.split('\n').find((l) => l.startsWith('vllm:cache_config_info'));
	if (!line) return {};
	const labels = {};
	for (const m of line.matchAll(/([a-zA-Z_0-9]+)="([^"]*)"/g)) labels[m[1]] = m[2];
	return labels;
}

export function vllmBackend(ctx) {
	return {
		id: 'vllm',
		title: 'vLLM engine',
		// the only engine here that has no in-band timings, so a scrape window
		// around each request is the only way to get real numbers
		windowsTimings: true,
		probes: { thinking: true, vision: true },

		async detect() {
			const res = await ctx.fetchJson('/version', 4000).catch(() => null);
			return Boolean(res?.version);
		},

		async describe() {
			const models = await ctx.listModels();
			const version = (await ctx.fetchJson('/version', 4000).catch(() => null))?.version ?? 'unknown';
			const entry = models?.data?.find((m) => m.id === ctx.cfg.model) || models?.data?.[0];
			return {
				entry: entry || null,
				n_ctx: ctx.cfg.nCtx || entry?.max_model_len || entry?.model_max_len || 4096,
				build_info: `vllm ${version}`,
				// Strata ships a template; vLLM does not expose its own, so the
				// UI cannot scan it. The probe result stands in for it.
				chat_template: ctx.flags.reasoning ? 'enable_thinking' : '',
				modalities: { vision: ctx.flags.vision, audio: false, video: false },
				params: {}
			};
		},

		translate(out) {
			const dropped = [];
			mapReasoning(out);
			// this build answers 400 for these while speculative decoding is on
			if (!ctx.cfg.keepUnsupported) commonTranslate(out, new Set(['min_p', 'logit_bias']), dropped);
			else commonTranslate(out, new Set(), dropped);
			if (dropped.length) ctx.log('dropped params:', dropped.join(' '));
			return out;
		},

		prepare(out) {
			if (out.stream) {
				// usage on the last chunk is the only exact token count available
				out.stream_options = { ...(out.stream_options || {}), include_usage: true };
				out.return_token_ids = true;
			}
			return out;
		},

		rewriteChunk(chunk, st) {
			const choice = chunk.choices?.[0];
			const d = choice?.delta;
			if (!d) return;
			// vLLM spells the thinking field differently from the webui
			if (typeof d.reasoning === 'string' && d.reasoning.length) {
				d.reasoning_content = d.reasoning;
				delete d.reasoning;
				ctx.flags.reasoning = true;
			}
			const hasText = (d.content && d.content.length) || (d.reasoning_content && d.reasoning_content.length) || d.tool_calls?.length;
			if (hasText) {
				if (!st.firstAt) st.firstAt = Date.now();
				st.lastAt = Date.now();
			}
			if (Array.isArray(choice.token_ids)) st.tokens += choice.token_ids.length;
			else if (hasText) st.tokens += 1;
		},

		// histogram deltas, but only when this request was alone in the window
		finalTimings(st, pre, post, tSend) {
			const timings = wallTimings(st, st.usage, tSend);
			let source = 'wall';
			const dCount = pre && post ? post.prefillCount - pre.prefillCount : 0;
			const quiet = pre && (pre.running || 0) === 0 && (pre.waiting || 0) === 0;
			const rising = post && pre && post.prefillSum >= pre.prefillSum && post.decodeSum >= pre.decodeSum && post.computedSum >= pre.computedSum;
			if (dCount === 1 && quiet && rising) {
				source = 'engine';
				const promptTotal = st.usage?.prompt_tokens ?? 0;
				const prompt_n = Math.max(Math.round(post.computedSum - pre.computedSum), 0);
				timings.prompt_ms = round1((post.prefillSum - pre.prefillSum) * 1000);
				timings.predicted_ms = round1((post.decodeSum - pre.decodeSum) * 1000) || timings.predicted_ms;
				timings.prompt_n = prompt_n;
				timings.cache_n = Math.max(promptTotal - prompt_n, 0);
			}
			ctx.log(logSpeed(source, timings));
			return { timings, source };
		},

		// opts carries force and the deadline: the window around a request must
		// read fresh, or the pre and post snapshots are the same cached scrape
		// and the delta is zero
		async snapshot(opts = {}) {
			return ctx.prometheusSnapshot(opts);
		},

		engine(snap) {
			const c = snap?.config || {};
			return {
				model: ctx.cfg.model || c.model,
				block_size: c.block_size,
				num_gpu_blocks: c.num_gpu_blocks,
				kv_cache_size_tokens: c.kv_cache_size_tokens,
				prefix_caching: c.enable_prefix_caching,
				gpu_memory_utilization: c.gpu_memory_utilization
			};
		},

		counters(snap) {
			return {
				prompt_tokens: snap.promptTokens || 0,
				generation_tokens: snap.genTokens || 0,
				requests: snap.prefillCount || 0,
				avg_ttft_ms: snap.ttftCount ? round1((snap.ttftSum / snap.ttftCount) * 1000) : 0,
				avg_inter_token_ms: snap.itlCount ? round1((snap.itlSum / snap.itlCount) * 1000) : 0,
				avg_queue_ms: snap.prefillCount ? round1(((snap.queueSum || 0) / snap.prefillCount) * 1000) : 0,
				avg_prefill_ms: snap.prefillCount ? round1((snap.prefillSum / snap.prefillCount) * 1000) : 0,
				avg_decode_ms: snap.decodeCount ? round1((snap.decodeSum / snap.decodeCount) * 1000) : 0,
				avg_prefill_tokens: snap.computedCount ? Math.round(snap.computedSum / snap.computedCount) : 0
			};
		},

		gauges(snap) {
			const pct = (a, b) => (b ? round1((a / b) * 100) : 0);
			return {
				requests_running: snap.running || 0,
				requests_waiting: snap.waiting || 0,
				kv_cache_usage_percent: round1((snap.kvUsage || 0) * 100),
				prefix_cache_hit_percent: pct(snap.prefixHits || 0, snap.prefixQueries || 0),
				spec_decode_accept_percent: pct(snap.specAcceptedTokens || 0, snap.specDraftTokens || 0),
				tokens_per_step: snap.specDrafts ? round1(((snap.specAcceptedTokens || 0) + snap.specDrafts) / snap.specDrafts) : 0,
				preemptions: snap.preemptions || 0
			};
		},

		rateKeys: { gen: 'genTokens', prompt: 'promptTokens', drafts: 'specDrafts', draftTokens: 'specDraftTokens', accepted: 'specAcceptedTokens' },
		labels: { kv: 'KV cache', ttft: 'First token' }
	};
}

// ---------------------------------------------------------------- Strata

export function strataBackend(ctx) {
	// Strata reports no cumulative draft counters, so the last request's
	// speculative numbers are the best available
	let lastSpec = null;

	return {
		id: 'strata',
		title: 'Strata engine',
		windowsTimings: false,
		probes: { thinking: false, vision: false },

		async detect() {
			const health = await ctx.fetchJson('/health', 4000).catch(() => null);
			return health?.service === 'strata';
		},

		// /props is already the shape the webui wants, with a real chat template
		async describe() {
			const [models, props, health, status] = await Promise.all([
				ctx.listModels(),
				ctx.fetchJson('/props', 6000).catch(() => null),
				ctx.fetchJson('/health', 4000).catch(() => null),
				ctx.fetchJson('/v1/status', 4000).catch(() => null)
			]);
			const entry = models?.data?.[0];
			const n_ctx = ctx.cfg.nCtx || props?.default_generation_settings?.n_ctx || health?.max_context || entry?.meta?.n_ctx || 4096;
			const vision = ctx.cfg.vision === 'auto' ? Boolean(props?.modalities?.vision ?? health?.images) : ctx.cfg.vision;
			ctx.flags.vision = vision;
			// the template proves it: Strata ships one that mentions thinking
			if (/enable_thinking|<\|think/.test(props?.chat_template || '')) ctx.flags.reasoning = true;
			const params = { ...(props?.default_generation_settings?.params || {}) };
			delete params.n_predict;
			return {
				entry: entry ? { ...entry, id: entry.id ?? health?.model } : null,
				n_ctx,
				model_path: props?.model_path ?? props?.model_alias ?? health?.model ?? 'unknown',
				build_info: props?.build_info ?? (status?.engine ? `strata ${status.engine}` : 'strata'),
				chat_template: props?.chat_template ?? '',
				modalities: { vision, audio: false, video: false },
				total_slots: props?.total_slots ?? 1,
				params
			};
		},

		translate(out) {
			const dropped = [];
			mapReasoning(out);
			// Strata knows min_p and stop; only the llama.cpp-only names go, and
			// n_predict becomes max_tokens because Strata reads neither
			commonTranslate(out, new Set(), dropped);
			if (dropped.length) ctx.log('dropped params:', dropped.join(' '));
			return out;
		},

		prepare(out) {
			return out;
		},

		rewriteChunk(chunk, st) {
			if (chunk.error) {
				st.engineError = chunk.error.message || 'the engine reported an error';
				return;
			}
			if (chunk.timings) lastSpec = chunk.timings;
			const choice = chunk.choices?.[0];
			const d = choice?.delta;
			if (!d) return;
			const text = (typeof d.content === 'string' && d.content) || (typeof d.reasoning_content === 'string' && d.reasoning_content) || '';
			if (d.tool_calls?.length) {
				if (!st.firstAt) st.firstAt = Date.now();
				st.lastAt = Date.now();
			}
			if (!text.length) return;
			if (!st.firstAt) st.firstAt = Date.now();
			st.lastAt = Date.now();
			// no token ids and no per-token timings in the stream, so the live
			// count is an estimate until the engine's own numbers arrive
			st.chars = (st.chars || 0) + text.length;
			st.tokens = Math.max(st.tokens, Math.round(st.chars / 4));
		},

		// prefer what the engine measured, fall back to the observed stream
		finalTimings(st, pre, post, tSend) {
			const fromEngine = st.inbandTimings;
			let source = 'engine';
			let timings = fromEngine;
			if (!timings) {
				timings = wallTimings(st, st.usage, tSend);
				source = 'wall';
			}
			ctx.log(logSpeed(source, timings));
			return { timings, source };
		},

		// Strata's /metrics is JSON, not Prometheus. Map the few shared fields.
		async snapshot() {
			const [m, s] = await Promise.all([ctx.fetchJson('/metrics', 6000).catch(() => null), ctx.fetchJson('/v1/status', 4000).catch(() => null)]);
			if (!m) return null;
			const t = m.totals || {};
			const live = m.live || {};
			const running = live.state && live.state !== 'idle' && live.state !== 'unloaded' ? 1 : 0;
			const gpu = s?.machine?.gpu;
			const spec = s?.last_timings;
			return {
				prefillCount: t.requests || 0,
				decodeCount: t.requests || 0,
				computedCount: t.requests || 0,
				prefillSum: (t.prompt_ms || 0) / 1000,
				decodeSum: (t.decode_ms || 0) / 1000,
				computedSum: Math.max(0, (t.prompt_tokens || 0) - (t.reused || 0)),
				promptTokens: t.prompt_tokens || 0,
				genTokens: t.output_tokens || 0,
				prefixHits: t.reused || 0,
				prefixQueries: t.prompt_tokens || 0,
				running,
				waiting: live.queued || 0,
				kvUsage: gpu && gpu.total_mib ? gpu.used_mib / gpu.total_mib : null,
				spec: s?.last_timings,
				vram_percent: gpu && gpu.total_mib ? round1((gpu.used_mib / gpu.total_mib) * 100) : null,
				live,
				requests: m.requests || [],
				config: {
					model: m.engine?.model,
					kv_cache_size_tokens: String(m.engine?.max_context ?? ''),
					prefix_caching: 'True'
				},
				ttftCount: (m.requests || []).length,
				ttftSum: (m.requests || []).reduce((a, r) => a + (r.prompt_ms || 0), 0) / 1000,
				itlCount: (m.requests || []).length,
				itlSum: (m.requests || []).reduce((a, r) => a + (r.decode_ms || 0), 0) / 1000,
				specAcceptedTokens: spec?.draft_n_accepted ?? lastSpec?.draft_n_accepted,
				specDraftTokens: spec?.draft_n ?? lastSpec?.draft_n,
				specDrafts: spec || lastSpec ? Math.max(1, ((spec ?? lastSpec).draft_n || 0) - ((spec ?? lastSpec).draft_n_accepted || 0)) : undefined
			};
		},

		engine(snap) {
			return { model: snap?.config?.model, kv_cache_size_tokens: snap?.config?.kv_cache_size_tokens, serving: 'one request at a time' };
		},

		counters(snap) {
			const reqs = snap.requests || [];
			const n = reqs.length || 1;
			const ms = (k) => round1(reqs.reduce((a, r) => a + (r[k] || 0), 0) / n);
			const itl = reqs.filter((r) => r.decode_tok_s > 0);
			return {
				prompt_tokens: snap.promptTokens || 0,
				generation_tokens: snap.genTokens || 0,
				requests: snap.prefillCount || 0,
				avg_ttft_ms: ms('prompt_ms'),
				avg_inter_token_ms: itl.length ? round1(1000 / (itl.reduce((a, r) => a + r.decode_tok_s, 0) / itl.length)) : 0,
				avg_queue_ms: 0,
				avg_prefill_ms: ms('prompt_ms'),
				avg_decode_ms: ms('decode_ms'),
				avg_prefill_tokens: snap.computedCount ? Math.round(snap.computedSum / snap.computedCount) : 0
			};
		},

		gauges(snap) {
			const pct = (a, b) => (b ? round1((a / b) * 100) : 0);
			return {
				requests_running: snap.running || 0,
				requests_waiting: snap.waiting || 0,
				kv_cache_usage_percent: snap.vram_percent,
				kv_cache_label: 'VRAM',
				prefix_cache_hit_percent: pct(snap.prefixHits || 0, snap.prefixQueries || 0),
				spec_decode_accept_percent: snap.specDraftTokens ? pct(snap.specAcceptedTokens, snap.specDraftTokens) : 0,
				tokens_per_step: snap.specDraftTokens ? round1(1 + snap.specAcceptedTokens / snap.specDraftTokens) : 0,
				preemptions: 0
			};
		},

		rateKeys: { gen: 'genTokens', prompt: 'promptTokens' },
		// Strata has no paged KV cache to fill, and its prompt_ms covers the read
		// of the prompt rather than the wait for the first token
		labels: { kv: 'VRAM', ttft: 'Prompt read' }
	};
}

export const BACKENDS = { vllm: vllmBackend, strata: strataBackend };

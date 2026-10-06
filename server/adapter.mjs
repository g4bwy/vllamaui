#!/usr/bin/env node
// Serves the extracted llama.cpp webui and translates its HTTP contract to a
// vLLM OpenAI server. Node stdlib only.

import http from 'node:http';
import { once } from 'node:events';
import { createReadStream, readFileSync } from 'node:fs';
import { stat } from 'node:fs/promises';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import { Readable } from 'node:stream';
import { pipeline } from 'node:stream/promises';

const HERE = path.dirname(fileURLToPath(import.meta.url));

// A real deployment knows things this repo must not: the backend host, the api
// key. They live in .env, which is gitignored. A variable already set in the
// environment wins over the file.
function loadDotEnv(file) {
	let text;
	try {
		text = readFileSync(file, 'utf8');
	} catch {
		return false;
	}
	for (const line of text.split('\n')) {
		const m = /^\s*([A-Z0-9_]+)\s*=\s*(.*?)\s*$/.exec(line);
		if (!m || line.trim().startsWith('#')) continue;
		if (process.env[m[1]] === undefined) process.env[m[1]] = m[2].replace(/^["']|["']$/g, '');
	}
	return true;
}

const usingDotEnv = loadDotEnv(path.resolve(HERE, '..', '.env'));

const cfg = {
	upstream: (process.env.VLLM_UPSTREAM || 'http://localhost:8000').replace(/\/+$/, ''),
	apiKey: process.env.VLLM_API_KEY || process.env.OPENAI_API_KEY || '',
	model: process.env.VLLM_MODEL || '',
	nCtx: Number(process.env.VLLM_N_CTX || 0),
	// auto = ask the engine. '1' forces it on, '0' forces it off
	vision: process.env.VLLM_MODALITY_VISION === '1' ? true : process.env.VLLM_MODALITY_VISION === '0' ? false : 'auto',
	engineTimings: process.env.VLLM_ENGINE_TIMINGS !== '0',
	keepUnsupported: process.env.VLLM_KEEP_UNSUPPORTED === '1',
	host: process.env.HOST || '0.0.0.0',
	port: Number(process.env.PORT || 8080),
	dist: process.env.UI_DIST || path.resolve(HERE, '..', 'dist')
};

// llama.cpp-only fields vLLM does not implement. It ignores unknown fields, but
// dropping them keeps the request clean and the logs readable.
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

// Rejected outright (HTTP 400) by vLLM while speculative decoding is enabled.
const DROP_UNSUPPORTED = new Set(['min_p', 'logit_bias']);

const CONTENT_TYPES = {
	'.html': 'text/html; charset=utf-8',
	'.js': 'text/javascript; charset=utf-8',
	'.mjs': 'text/javascript; charset=utf-8',
	'.css': 'text/css; charset=utf-8',
	'.json': 'application/json; charset=utf-8',
	'.webmanifest': 'application/manifest+json',
	'.svg': 'image/svg+xml',
	'.png': 'image/png',
	'.jpg': 'image/jpeg',
	'.ico': 'image/x-icon',
	'.woff2': 'font/woff2',
	'.woff': 'font/woff',
	'.ttf': 'font/ttf',
	'.webp': 'image/webp',
	'.map': 'application/json'
};

// Prometheus metric name -> snapshot field. Sums across label sets (multi-engine).
const METRICS = new Map([
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

let cachedModels = { at: 0, data: null };
let lastStats = null;
let observedReasoning = false;
// resolved capability flag: false until the probe or a successful image says otherwise
let visionSupported = cfg.vision === 'auto' ? false : cfg.vision;
// /props waits for these, bounded so a dead backend cannot stall the first
// paint: after 2 s the page loads with what we know and retries /props anyway
let probesReady = Promise.resolve();
const probeWait = () => Promise.race([probesReady, new Promise((r) => setTimeout(r, 2000))]);

// smallest legal PNG: enough to make the engine run its vision encoder
const PROBE_IMAGE =
	'data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8DwHwAFAAH/q842iQAAAABJRU5ErkJggg==';

function hasImagePart(messages) {
	return Array.isArray(messages) && messages.some((m) => Array.isArray(m.content) && m.content.some((p) => p?.type === 'image_url'));
}

const log = (...a) => console.log(new Date().toISOString().slice(11, 19), ...a);

function sendJson(res, status, obj, headers = {}) {
	const body = JSON.stringify(obj);
	res.writeHead(status, { 'Content-Type': 'application/json; charset=utf-8', 'Content-Length': Buffer.byteLength(body), ...headers });
	res.end(body);
}

const MAX_BODY_BYTES = 64 * 1024 * 1024;

async function readJson(req) {
	const chunks = [];
	let size = 0;
	for await (const c of req) {
		size += c.length;
		if (size > MAX_BODY_BYTES) throw new Error(`request body over ${MAX_BODY_BYTES} bytes`);
		chunks.push(c);
	}
	const text = Buffer.concat(chunks).toString('utf8');
	if (!text) return {};
	return JSON.parse(text);
}

async function upstreamFetch(pathname, init = {}) {
	const headers = { 'Content-Type': 'application/json', ...init.headers };
	if (cfg.apiKey) headers.Authorization = `Bearer ${cfg.apiKey}`;
	return fetch(cfg.upstream + pathname, { ...init, headers });
}

// Small control reads must not hang: a stuck /metrics would otherwise stall
// whatever chat request is waiting on it.
async function fetchJson(pathname, timeoutMs = 8000) {
	const res = await upstreamFetch(pathname, { signal: AbortSignal.timeout(timeoutMs) });
	if (!res.ok) {
		res.body?.cancel().catch(() => {});
		throw new Error(`${pathname} returned HTTP ${res.status}`);
	}
	return res.json();
}

// Returns the model list, or null when the upstream could not be reached.
// Callers must tell "no answer" apart from "answered with an empty list".
async function listModels(force = false) {
	if (!force && Date.now() - cachedModels.at < 30000 && cachedModels.data) return cachedModels.data;
	try {
		cachedModels = { at: Date.now(), data: await fetchJson('/v1/models') };
		return cachedModels.data;
	} catch (e) {
		log('model list failed:', e.message);
		return cachedModels.data || null;
	}
}

async function upstreamVersion() {
	try {
		return (await fetchJson('/version', 4000)).version || 'unknown';
	} catch {
		return 'unknown';
	}
}

const SAMPLE = /^(\S+?)(?:\{[^\n]*\})? (\S+)(?: \d+)?$/;

function parseMetrics(text) {
	const out = {};
	for (const line of text.split('\n')) {
		if (!line || line[0] === '#') continue;
		const m = SAMPLE.exec(line);
		if (!m) continue;
		const field = METRICS.get(m[1]);
		if (!field) continue;
		const v = Number(m[2]);
		if (Number.isFinite(v)) out[field] = (out[field] || 0) + v;
	}
	return out;
}

// The UI polls /vllm/stats every few seconds. Cache the scrape so several tabs
// share one upstream request, but never serve a stale body to a timing window:
// the pre/post snapshots around a completion must be fresh or the delta is 0.
let metricsCache = { at: 0, text: '' };

async function metricsText(force = false, timeoutMs = 8000) {
	if (!force && metricsCache.text && Date.now() - metricsCache.at < 1500) return metricsCache.text;
	const res = await upstreamFetch('/metrics', { signal: AbortSignal.timeout(timeoutMs) });
	if (!res.ok) {
		res.body?.cancel().catch(() => {});
		throw new Error(`metrics returned HTTP ${res.status}`);
	}
	const text = await res.text();
	metricsCache = { at: Date.now(), text };
	return text;
}

async function metricsSnapshot(timeoutMs = 8000) {
	try {
		const snap = parseMetrics(await metricsText(true, timeoutMs));
		lastStats = { at: Date.now(), snap };
		return snap;
	} catch (e) {
		log('metrics failed:', e.message);
		return null;
	}
}

// ---------- llama.cpp API surface ----------

async function handleProps(req, res) {
	await probeWait();
	const models = await listModels();
	if (!models) {
		// 503 is the code the UI reads as "backend not ready": it shows a
		// spinner and retries once a second, so a restarting vLLM heals itself
		return sendJson(res, 503, { error: { message: `cannot reach vLLM at ${cfg.upstream}` } });
	}
	const entry = models.data?.find((m) => m.id === cfg.model) || models.data?.[0];
	const version = await upstreamVersion();
	const spec = lastStats?.snap?.specDrafts > 0;
	const n_ctx = cfg.nCtx || entry?.max_model_len || entry?.model_max_len || 4096;

	sendJson(res, 200, {
		role: 'model',
		model_path: entry?.id ?? cfg.model ?? 'unknown',
		total_slots: 1,
		// vLLM never serves its chat template, and the UI decides "this model
		// can think" by string-scanning that template (utils/
		// chat-template-thinking-detector.ts). Report the marker once the engine
		// has actually emitted a reasoning delta, so the thinking control and
		// the enable_thinking kwarg we map below become reachable in the UI.
		chat_template: observedReasoning ? 'enable_thinking' : '',
		bos_token: '',
		eos_token: entry?.eos_token ?? '',
		// no backend address here: this response goes to every browser that loads
		// the page, and the host is a private deployment detail. See var/adapter.log
		build_info: `vllm ${version}`,
		cors_proxy_enabled: false,
		modalities: { vision: visionSupported, audio: false, video: false },
		default_generation_settings: {
			id: 0,
			id_task: 0,
			n_ctx,
			speculative: Boolean(spec),
			is_processing: false,
			prompt: '',
			next_token: { has_next_token: false, has_new_line: false, n_remain: 0, n_decoded: 0, stopping_word: '' },
			// Empty on purpose: the UI then sends no sampling params and vLLM
			// server defaults rule. Values discovered here are copied into the UI
			// parameter store, so a wrong default would follow every request.
			params: {}
		}
	});
}

function translateRequest(body) {
	const out = { ...body };
	const dropped = [];

	if (out.reasoning_format === 'none') {
		out.chat_template_kwargs = { ...(out.chat_template_kwargs || {}), enable_thinking: false, thinking: false };
	}

	for (const k of DROP_LLAMA_ONLY) if (k in out) dropped.push(k);
	if (!cfg.keepUnsupported) for (const k of DROP_UNSUPPORTED) if (k in out) dropped.push(k);
	for (const k of dropped) delete out[k];

	if (typeof out.n_predict === 'number') {
		// n_predict 0 is the prompt warm-up call: process the prompt, generate
		// nothing. vLLM needs at least one token, so ask for the minimum.
		out.max_tokens = out.n_predict === 0 ? 1 : out.n_predict;
		delete out.n_predict;
		dropped.push('n_predict>max_tokens');
	}
	if (out.max_tokens === 0) out.max_tokens = 1;
	if (typeof out.max_tokens === 'number' && out.max_tokens < 0) delete out.max_tokens;

	if (out.stream) {
		// usage on the last chunk gives exact token counts for the timings
		out.stream_options = { ...(out.stream_options || {}), include_usage: true };
		out.return_token_ids = true;
	}

	if (dropped.length) log('dropped params:', dropped.join(' '));
	return out;
}

// The UI omits "model" in single-model role, and vLLM rejects an empty id.
let cachedDefaultModel = '';

async function defaultModelId() {
	if (cfg.model) return cfg.model;
	if (!cachedDefaultModel) {
		const models = await listModels(true);
		cachedDefaultModel = models?.data?.[0]?.id || '';
	}
	return cachedDefaultModel;
}

function mapMessageReasoning(payload) {
	const choice = payload?.choices?.[0];
	const msg = choice?.message;
	if (msg && typeof msg.reasoning === 'string' && msg.reasoning) {
		msg.reasoning_content = msg.reasoning;
		delete msg.reasoning;
	}
	return payload;
}

function round1(n) {
	return Math.round(n * 10) / 10;
}

function finalTimings(st, pre, post, tSend) {
	const usage = st.usage || {};
	const promptTotal = usage.prompt_tokens ?? 0;
	const predicted_n = usage.completion_tokens ?? st.tokens;
	const wallTtft = st.firstAt ? st.firstAt - tSend : 0;
	const wallDecode = st.firstAt && st.lastAt ? Math.max(st.lastAt - st.firstAt, 1) : 0;

	let prompt_ms = round1(wallTtft);
	let predicted_ms = round1(wallDecode);
	let prompt_n = promptTotal;
	let cache_n = 0;
	let source = 'wall';

	// Trust the engine histograms only when the window is unambiguous: exactly
	// one request finished, nothing else was in flight when it started, and no
	// counter went backwards (that means the engine restarted mid-request).
	const dCount = pre && post ? post.prefillCount - pre.prefillCount : 0;
	const quiet = pre && (pre.running || 0) === 0 && (pre.waiting || 0) === 0;
	const monotonic = post && pre && post.prefillSum >= pre.prefillSum && post.decodeSum >= pre.decodeSum && post.computedSum >= pre.computedSum;
	if (dCount === 1 && quiet && monotonic) {
		source = 'engine';
		prompt_ms = round1((post.prefillSum - pre.prefillSum) * 1000);
		predicted_ms = round1((post.decodeSum - pre.decodeSum) * 1000) || predicted_ms;
		prompt_n = Math.max(Math.round(post.computedSum - pre.computedSum), 0);
		cache_n = Math.max(promptTotal - prompt_n, 0);
	}

	const timings = { predicted_n, predicted_ms };
	if (prompt_n > 0 && prompt_ms > 0) {
		timings.prompt_n = prompt_n;
		timings.prompt_ms = prompt_ms;
	}
	if (cache_n > 0) timings.cache_n = cache_n;

	const pp = timings.prompt_ms ? (timings.prompt_n / timings.prompt_ms) * 1000 : 0;
	const tg = predicted_ms ? (predicted_n / predicted_ms) * 1000 : 0;
	log(
		`timings(${source}) pp ${timings.prompt_n || 0} tok ${pp.toFixed(1)} t/s | tg ${predicted_n} tok ${tg.toFixed(1)} t/s | cache ${cache_n}`
	);
	return { timings, source };
}

async function handleChat(req, res) {
	let body;
	try {
		body = await readJson(req);
	} catch (e) {
		return sendJson(res, 400, { error: { message: 'invalid JSON body: ' + e.message } });
	}

	let out = translateRequest(body);
	if (!out.model) out.model = await defaultModelId();

	// Scrape metrics before the request leaves, but do not wait for it here: the
	// engine's own prefill covers the round trip, and the timing window still
	// starts before vLLM sees this request.
	const prePromise = cfg.engineTimings ? metricsSnapshot() : Promise.resolve(null);
	const tSend = Date.now();

	// The UI cancels a generation by closing the fetch, so the only reliable
	// signal is this response going away before it was finished. Without the
	// abort, vLLM keeps generating into a dead socket: an abandoned request
	// here ran to 8092 tokens while nothing was listening.
	const abort = new AbortController();
	res.on('close', () => {
		if (!res.writableFinished) abort.abort();
	});

	let upstream;
	try {
		upstream = await upstreamFetch('/v1/chat/completions', {
			method: 'POST',
			body: JSON.stringify(out),
			signal: abort.signal
		});
	} catch (e) {
		if (abort.signal.aborted) {
			log('client aborted before the engine answered');
			return;
		}
		return sendJson(res, 502, { error: { message: `cannot reach vLLM at ${cfg.upstream}: ${e.message}` } });
	}

	// keep the advertised capability in step with what the engine really does
	const sentImage = hasImagePart(body.messages);

	if (!upstream.ok) {
		const text = await upstream.text();
		if (sentImage && visionSupported && /image|visual|multimodal/i.test(text)) {
			visionSupported = false;
			log('vision support: off, the engine rejected an image request');
		}
		res.writeHead(upstream.status, { 'Content-Type': upstream.headers.get('content-type') || 'application/json' });
		return res.end(text);
	}

	if (sentImage && !visionSupported) {
		visionSupported = true;
		log('vision support: on, the engine accepted an image request');
	}

	if (!out.stream) {
		const json = mapMessageReasoning(await upstream.json());
		return sendJson(res, 200, json);
	}

	res.writeHead(200, {
		'Content-Type': 'text/event-stream; charset=utf-8',
		'Cache-Control': 'no-cache, no-transform',
		Connection: 'keep-alive',
		'X-Accel-Buffering': 'no'
	});

	const st = { firstAt: 0, lastAt: 0, tokens: 0, usage: null, sawDone: false, truncated: false };
	const pre = await prePromise;
	const decoder = new TextDecoder();
	let buf = '';
	let pending = null; // usage chunk held back until the post-request snapshot
	let finished = false;

	async function writeEvent(obj) {
		if (res.destroyed) return;
		if (!res.write(`data: ${JSON.stringify(obj)}\n\n`)) {
			// a throttled client must not make us buffer the rest of the answer
			await Promise.race([once(res, 'drain'), once(res, 'close')]).catch(() => {});
		}
	}

	const finish = async () => {
		if (finished) return;
		finished = true;

		if (abort.signal.aborted) {
			upstream.body?.cancel().catch(() => {});
			log(`client aborted after ${st.tokens} tokens, engine cancelled`);
			return;
		}

		// short deadline: a stuck /metrics must not hold up the end of the stream
		const post = cfg.engineTimings ? await metricsSnapshot(1500) : null;
		const { timings } = finalTimings(st, pre, post, tSend);

		if (st.truncated) {
			// no [DONE] here: the UI reads that as a finished answer, and a
			// cut-off reply must not look complete
			log(`upstream ended early after ${st.tokens} tokens, reporting a lost stream`);
			return res.end();
		}

		if (pending) pending.timings = timings;
		else
			pending = {
				id: st.id || 'adapter',
				object: 'chat.completion.chunk',
				created: Math.floor(Date.now() / 1000),
				model: st.model || out.model,
				choices: [],
				timings
			};
		await writeEvent(pending);
		if (!res.destroyed) res.write('data: [DONE]\n\n');
		res.end();
	};

	async function handleLine(line) {
		if (!line.startsWith('data:')) return; // blank separators and comments
		const payload = line.slice(5).trim();
		if (payload === '[DONE]') {
			st.sawDone = true;
			return;
		}

		let chunk;
		try {
			chunk = JSON.parse(payload);
		} catch {
			return;
		}

		st.id = chunk.id || st.id;
		st.model = chunk.model || st.model;
		const choice = chunk.choices?.[0];

		if (choice?.delta) {
			const d = choice.delta;
			if (typeof d.reasoning === 'string' && d.reasoning.length) {
				d.reasoning_content = d.reasoning;
				delete d.reasoning;
				observedReasoning = true;
			}
			const hasText = (d.content && d.content.length) || (d.reasoning_content && d.reasoning_content.length) || d.tool_calls?.length;
			if (hasText) {
				if (!st.firstAt) st.firstAt = Date.now();
				st.lastAt = Date.now();
			}
			if (Array.isArray(choice.token_ids)) st.tokens += choice.token_ids.length;
			else if (hasText) st.tokens += 1;
		}

		if (chunk.usage) {
			st.usage = chunk.usage;
			pending = chunk;
			return;
		}

		// live decode speed: the UI only knows t/s from server timings
		if (st.tokens >= 2 && st.firstAt) {
			const ms = Date.now() - st.firstAt;
			if (ms > 0) chunk.timings = { predicted_n: st.tokens, predicted_ms: round1(ms) };
		}

		await writeEvent(chunk);
	}

	try {
		for await (const raw of Readable.fromWeb(upstream.body)) {
			if (abort.signal.aborted) break;
			buf += decoder.decode(raw, { stream: true });
			let nl;
			while ((nl = buf.indexOf('\n')) >= 0) {
				await handleLine(buf.slice(0, nl).replace(/\r$/, ''));
				buf = buf.slice(nl + 1);
			}
		}
		buf += decoder.decode();
		// a final line with no newline still has to reach the client
		if (buf) await handleLine(buf.replace(/\r$/, ''));
		st.truncated = !st.sawDone && !abort.signal.aborted;
	} catch (e) {
		if (!abort.signal.aborted) {
			st.truncated = true;
			log('upstream stream error:', e.message);
		}
	}

	await finish();
}

// System-wide throughput between two polls: counters are cumulative, so a
// delta over a real time window is the rate the engine is holding right now.
let rateWindow = null;

function ratesFrom(snap, sampleAt) {
	const now = sampleAt || Date.now();
	const prev = rateWindow;
	rateWindow = {
		at: now,
		gen: snap.genTokens || 0,
		prompt: snap.promptTokens || 0,
		drafts: snap.specDrafts || 0,
		draftTokens: snap.specDraftTokens || 0,
		accepted: snap.specAcceptedTokens || 0
	};
	if (!prev) return {};

	// an engine restart zeroes the counters, which would read as a huge
	// negative throughput. Drop the window and start a new one.
	if (rateWindow.gen < prev.gen || rateWindow.prompt < prev.prompt || rateWindow.draftTokens < prev.draftTokens) return {};

	const seconds = (now - prev.at) / 1000;
	// A long gap means the window mostly covers idle time. Averaging over it
	// would report a "current" rate that is really a stale average, so give up
	// on rates for this sample and let the next poll measure a short window.
	if (seconds < 0.5 || seconds > 60) return { window_s: round1(seconds) };
	const per = (a, b) => round1((a - b) / seconds);
	const dDraft = rateWindow.draftTokens - prev.draftTokens;
	return {
		window_s: round1(seconds),
		generation_tokens_per_second: per(rateWindow.gen, prev.gen),
		prompt_tokens_per_second: per(rateWindow.prompt, prev.prompt),
		steps_per_second: per(rateWindow.drafts, prev.drafts),
		window_accept_percent: dDraft > 0 ? round1(((rateWindow.accepted - prev.accepted) / dDraft) * 100) : 0
	};
}

async function handleStats(req, res) {
	let snap = {};
	let conf = {};
	try {
		const text = await metricsText();
		snap = parseMetrics(text);
		const line = text.split('\n').find((l) => l.startsWith('vllm:cache_config_info'));
		if (line) {
			conf = {};
			for (const m of line.matchAll(/([a-zA-Z_0-9]+)="([^"]*)"/g)) conf[m[1]] = m[2];
		}
	} catch (e) {
		return sendJson(res, 502, { error: `cannot read ${cfg.upstream}/metrics: ${e.message}` });
	}

	const pct = (a, b) => (b ? round1((a / b) * 100) : 0);
	sendJson(res, 200, {
		sampled_at: new Date().toISOString(),
		gauges: {
			requests_running: snap.running || 0,
			requests_waiting: snap.waiting || 0,
			kv_cache_usage_percent: round1((snap.kvUsage || 0) * 100),
			prefix_cache_hit_percent: pct(snap.prefixHits || 0, snap.prefixQueries || 0),
			spec_decode_accept_percent: pct(snap.specAcceptedTokens || 0, snap.specDraftTokens || 0),
			tokens_per_step: snap.specDrafts ? round1(((snap.specAcceptedTokens || 0) + snap.specDrafts) / snap.specDrafts) : 0,
			preemptions: snap.preemptions || 0
		},
		rates: ratesFrom(snap, metricsCache.at),
		counters: {
			prompt_tokens: snap.promptTokens || 0,
			generation_tokens: snap.genTokens || 0,
			requests: snap.prefillCount || 0,
			avg_ttft_ms: snap.ttftCount ? round1((snap.ttftSum / snap.ttftCount) * 1000) : 0,
			avg_inter_token_ms: snap.itlCount ? round1((snap.itlSum / snap.itlCount) * 1000) : 0,
			avg_queue_ms: snap.prefillCount ? round1(((snap.queueSum || 0) / snap.prefillCount) * 1000) : 0,
			avg_prefill_ms: snap.prefillCount ? round1((snap.prefillSum / snap.prefillCount) * 1000) : 0,
			avg_decode_ms: snap.decodeCount ? round1((snap.decodeSum / snap.decodeCount) * 1000) : 0,
			avg_prefill_tokens: snap.computedCount ? Math.round(snap.computedSum / snap.computedCount) : 0
		},
		engine: {
			model: cfg.model,
			block_size: conf.block_size,
			num_gpu_blocks: conf.num_gpu_blocks,
			kv_cache_size_tokens: conf.kv_cache_size_tokens,
			prefix_caching: conf.enable_prefix_caching,
			gpu_memory_utilization: conf.gpu_memory_utilization
		}
	});
}

// ---------- static files ----------

async function serveStatic(req, res, urlPath) {
	let rel;
	try {
		rel = decodeURIComponent(urlPath);
	} catch {
		return sendJson(res, 400, { error: 'bad request path' });
	}
	if (rel.endsWith('/')) rel += 'index.html';

	const root = path.resolve(cfg.dist);
	let file = path.resolve(root, '.' + (rel.startsWith('/') ? rel : '/' + rel));
	// startsWith(root) alone would accept a sibling such as ../dist-secret
	if (file !== root && !file.startsWith(root + path.sep)) return sendJson(res, 403, { error: 'forbidden' });

	let info = await stat(file).catch(() => null);
	if (!info?.isFile()) {
		// only route paths fall back to the SPA: a missing .js must 404, or the
		// browser gets HTML where it expects a module and reports a syntax error
		if (path.extname(file)) return sendJson(res, 404, { error: 'not found' });
		file = path.join(root, 'index.html');
		info = await stat(file).catch(() => null);
		if (!info) return sendJson(res, 404, { error: 'ui not built: run ./build.sh' });
		return streamFile(res, req.method, file, info.size, 'text/html; charset=utf-8', 'no-cache');
	}

	const ext = path.extname(file);
	const cacheable = file.includes(`${path.sep}immutable${path.sep}`);
	streamFile(res, req.method, file, info.size, CONTENT_TYPES[ext] || 'application/octet-stream', cacheable ? 'public, max-age=31536000, immutable' : 'no-cache');
}

function streamFile(res, method, file, size, type, cache) {
	res.writeHead(200, { 'Content-Type': type, 'Content-Length': size, 'Cache-Control': cache });
	if (method === 'HEAD') return res.end();
	// pipeline forwards a read error and always releases the fd, unlike pipe
	pipeline(createReadStream(file), res).catch((e) => log('asset read failed:', file, e.message));
}

// ---------- router ----------

const NOT_FOUND_STREAM = { error: { message: 'stream replay is a llama.cpp feature, not available for vLLM' } };

async function route(req, res) {
	const url = new URL(req.url, `http://${req.headers.host || 'localhost'}`);
	const p = url.pathname.replace(/\/+$/, '') || '/';
	const method = req.method;

	if (method === 'OPTIONS') {
		res.writeHead(204, {
			'Access-Control-Allow-Origin': '*',
			'Access-Control-Allow-Headers': 'Content-Type, Authorization, X-Conversation-Id, api-key',
			'Access-Control-Allow-Methods': 'GET,POST,DELETE,OPTIONS'
		});
		return res.end();
	}
	res.setHeader('Access-Control-Allow-Origin', '*');

	if (p === '/props') return handleProps(req, res);
	if (p === '/v1/models' && method === 'GET') {
		const models = await listModels(true);
		return sendJson(res, models ? 200 : 502, models || { error: { message: `cannot reach vLLM at ${cfg.upstream}` } });
	}
	if (p === '/v1/chat/completions' && method === 'POST') return handleChat(req, res);
	if (p === '/v1/chat/completions/control' && method === 'POST') return sendJson(res, 200, { success: false, error: 'not supported by vLLM' });
	if (p === '/slots') return sendJson(res, 200, []);
	if (p === '/tools') return sendJson(res, 200, []);
	if (p === '/v1/streams/lookup' && method === 'POST') return sendJson(res, 200, []);
	if (p === '/v1/stream') {
		if (method === 'DELETE') return sendJson(res, 200, { success: true });
		return sendJson(res, 404, NOT_FOUND_STREAM);
	}
	if (p === '/build.json') return sendJson(res, 200, { version: `vllm ${await upstreamVersion()}` });
	if (p === '/vllm/stats') return handleStats(req, res);
	if (p.startsWith('/models') || p === '/cors-proxy' || p === '/completion' || p === '/tokenize') {
		return sendJson(res, 501, { success: false, error: `not available on a vLLM backend (${p})` });
	}

	if (method !== 'GET' && method !== 'HEAD') return sendJson(res, 405, { error: 'method not allowed' });
	return serveStatic(req, res, url.pathname);
}

// Ask the engine once at startup whether it thinks, so the first page load
// already offers the control. A short streamed completion answers it.
async function probeThinkingSupport() {
	if (process.env.VLLM_PROBE_THINKING === '0') return;
	try {
		const res = await upstreamFetch('/v1/chat/completions', {
			method: 'POST',
			body: JSON.stringify({
				model: await defaultModelId(),
				messages: [{ role: 'user', content: 'Hi' }],
				stream: true,
				max_tokens: 16
			})
		});
		if (res.ok && /"reasoning"\s*:/.test(await res.text())) {
			observedReasoning = true;
			log('thinking support: yes (engine emitted reasoning deltas)');
		}
	} catch (e) {
		log('thinking probe failed:', e.message);
	}
}

// The frontend only accepts an image when /props says the model sees, so this
// has to be answered from the engine, not from a guess: one request with a 1x1
// image tells us. It costs one short prefill at startup.
async function probeVisionSupport() {
	if (cfg.vision !== 'auto') {
		visionSupported = cfg.vision;
		log(`vision: ${visionSupported ? 'on' : 'off'} (VLLM_MODALITY_VISION)`);
		return;
	}
	if (process.env.VLLM_PROBE_VISION === '0') {
		log('vision: off (probe skipped, set VLLM_MODALITY_VISION=1 to force on)');
		return;
	}
	try {
		const res = await upstreamFetch('/v1/chat/completions', {
			method: 'POST',
			signal: AbortSignal.timeout(12000),
			body: JSON.stringify({
				model: await defaultModelId(),
				messages: [
					{
						role: 'user',
						content: [
							{ type: 'text', text: 'Reply with the letter A.' },
							{ type: 'image_url', image_url: { url: PROBE_IMAGE } }
						]
					}
				],
				stream: false,
				max_tokens: 1
			})
		});
		if (res.ok) {
			visionSupported = true;
			await res.body?.cancel();
			log('vision support: yes (engine accepted an image)');
		} else {
			const text = await res.text();
			const reason = /image|visual|multimodal|modality/i.test(text) ? 'engine rejects image inputs' : 'probe failed';
			log(`vision support: no (${reason}, HTTP ${res.status}) ${text.replace(/\s+/g, ' ').slice(0, 160)}`);
		}
	} catch (e) {
		log('vision probe failed:', e.message);
	}
}

const server = http.createServer((req, res) => {
	route(req, res).catch((e) => {
		log('handler error:', e.stack || e.message);
		if (!res.headersSent) sendJson(res, 500, { error: e.message });
		else res.end();
	});
});

// Capability probes start before the port binds, so a client that loads
// immediately cannot win the race against them.
probesReady = Promise.all([probeThinkingSupport(), probeVisionSupport()]);

server.listen(cfg.port, cfg.host, async () => {
	const models = await listModels(true);
	const version = await upstreamVersion();
	await metricsSnapshot();
	log(`llama.cpp webui -> vLLM adapter`);
	log(`  ui      http://${cfg.host}:${cfg.port}/`);
	log(`  backend ${cfg.upstream} (vllm ${version})`);
	if (usingDotEnv) log('  loaded  .env (gitignored)');
	log(`  model   ${models?.data?.map((m) => m.id).join(', ') || 'none reported'}`);
	log(`  timings ${cfg.engineTimings ? 'engine histograms, wall-clock fallback' : 'wall clock only'}`);
	await probeWait();
});

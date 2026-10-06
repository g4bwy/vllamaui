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
import { BACKENDS, parsePrometheus, parseCacheConfig } from './backends.mjs';

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
	// auto asks the upstream what it is; set vllm or strata to skip that
	backend: (process.env.BACKEND || 'auto').toLowerCase(),
	upstream: (process.env.UPSTREAM_URL || process.env.VLLM_UPSTREAM || 'http://localhost:8000').replace(/\/+$/, ''),
	apiKey: process.env.UPSTREAM_API_KEY || process.env.VLLM_API_KEY || process.env.OPENAI_API_KEY || '',
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

let cachedModels = { at: 0, data: null };
let lastSnapshot = null;
// capability flags the two engines report in different ways
const flags = { reasoning: false, vision: cfg.vision === 'auto' ? false : cfg.vision };
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

// one short label for /build.json, which the About dialog shows
async function backendVersion() {
	const d = await backend.describe().catch(() => null);
	return (d?.build_info || backend.id).replace(/\s+/g, ' ').slice(0, 60);
}

// vLLM publishes Prometheus counters; the snapshot helper hands them to the
// vllm backend. Strata publishes JSON on the same path, which its own backend
// reads through fetchJson instead, so nothing here parses text for it.
let metricsCache = { at: 0, text: '', parsed: null };

async function prometheusSnapshot({ force = false, timeoutMs = 8000 } = {}) {
	const fresh = force || !metricsCache.parsed || Date.now() - metricsCache.at >= 1500;
	if (fresh) {
		const res = await upstreamFetch('/metrics', { signal: AbortSignal.timeout(timeoutMs) });
		if (!res.ok) {
			res.body?.cancel().catch(() => {});
			throw new Error(`metrics returned HTTP ${res.status}`);
		}
		const text = await res.text();
		const parsed = parsePrometheus(text);
		parsed.config = parseCacheConfig(text);
		metricsCache = { at: Date.now(), text, parsed };
	}
	lastSnapshot = { at: metricsCache.at, snap: metricsCache.parsed };
	return metricsCache.parsed;
}

// errors are expected here only when the backend is down
async function safeSnapshot(opts) {
	try {
		return await backend.snapshot(opts);
	} catch (e) {
		log('snapshot failed:', e.message);
		return null;
	}
}

// ---------- backend selection ----------

if (cfg.backend !== 'auto' && !BACKENDS[cfg.backend]) {
	console.error(`BACKEND must be auto, ${Object.keys(BACKENDS).join(', ')}. Got "${cfg.backend}".`);
	process.exit(1);
}

const ctx = {
	cfg,
	flags,
	log,
	fetchJson,
	listModels,
	prometheusSnapshot: (opts) => prometheusSnapshot(opts),
	parsePrometheus
};

let backend = null;

async function selectBackend() {
	const wanted = cfg.backend;
	if (wanted !== 'auto') return { id: wanted, def: BACKENDS[wanted](ctx) };
	// Strata announces itself; vLLM answers /version. Either is one cheap GET.
	const strata = BACKENDS.strata(ctx);
	if (await strata.detect().catch(() => false)) return { id: 'strata', def: strata };
	const vllm = BACKENDS.vllm(ctx);
	if (await vllm.detect().catch(() => false)) return { id: 'vllm', def: vllm };
	return { id: 'vllm', def: vllm }; // keep the older default for an unknown server
}

// ---------- llama.cpp API surface ----------

async function handleProps(req, res) {
	await probeWait();
	const models = await listModels();
	if (!models) {
		// 503 is the code the UI reads as "backend not ready": it shows a
		// spinner and retries once a second, so a restarting backend heals itself
		return sendJson(res, 503, { error: { message: `cannot reach the backend at ${cfg.upstream}` } });
	}
	const d = await backend.describe(models);

	sendJson(res, 200, {
		role: 'model',
		model_path: d.model_path ?? d.entry?.id ?? cfg.model ?? 'unknown',
		total_slots: d.total_slots ?? 1,
		// vLLM never serves its chat template, and the UI decides whether the
		// model can think by string-scanning that template (utils/
		// chat-template-thinking-detector.ts). Strata ships a real one, so its
		// backend passes the original text through and the UI reads it directly.
		chat_template: d.chat_template ?? '',
		bos_token: '',
		eos_token: d.entry?.eos_token ?? '',
		// no backend address here: this response goes to every browser that loads
		// the page, and the host is a private deployment detail. See var/adapter.log
		build_info: d.build_info,
		cors_proxy_enabled: false,
		modalities: d.modalities,
		default_generation_settings: {
			id: 0,
			id_task: 0,
			n_ctx: d.n_ctx,
			speculative: Boolean(d.speculative),
			is_processing: false,
			prompt: '',
			next_token: { has_next_token: false, has_new_line: false, n_remain: 0, n_decoded: 0, stopping_word: '' },
			// vLLM reports nothing usable here, so the UI sends no sampling params
			// and the server defaults rule. Strata answers this from its /props.
			params: d.params ?? {}
		}
	});
}

function translateRequest(body) {
	const out = { ...body };
	backend.translate(out);
	return backend.prepare(out);
}

// The UI omits "model" in single-model role, and both engines want one.
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

async function handleChat(req, res) {
	let body;
	try {
		body = await readJson(req);
	} catch (e) {
		return sendJson(res, 400, { error: { message: 'invalid JSON body: ' + e.message } });
	}

	let out = translateRequest(body);
	if (!out.model) out.model = await defaultModelId();

	// vLLM reports nothing in the stream, so a Prometheus window around each
	// request is the only way to get its real numbers. Start the first scrape
	// before the request leaves, but do not wait for it: the engine's prefill
	// covers the round trip. Strata puts timings in the stream, so no window.
	const needWindow = backend.windowsTimings && cfg.engineTimings;
	const prePromise = needWindow ? safeSnapshot({ force: true }) : Promise.resolve(null);
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
		return sendJson(res, 502, { error: { message: `cannot reach the ${backend.id} backend at ${cfg.upstream}: ${e.message}` } });
	}

	// keep the advertised capability in step with what the engine really does
	const sentImage = hasImagePart(body.messages);

	if (!upstream.ok) {
		const text = await upstream.text();
		if (sentImage && flags.vision && /image|visual|multimodal/i.test(text)) {
			flags.vision = false;
			log('vision support: off, the engine rejected an image request');
		}
		res.writeHead(upstream.status, { 'Content-Type': upstream.headers.get('content-type') || 'application/json' });
		return res.end(text);
	}

	if (sentImage && !flags.vision) {
		flags.vision = true;
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
		const post = needWindow ? await safeSnapshot({ force: true, timeoutMs: 1500 }) : null;
		const { timings } = backend.finalTimings(st, pre, post, tSend);

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
		// an engine that measures its own speed is always the better source
		if (chunk.timings) st.inbandTimings = chunk.timings;
		if (chunk.error) {
			st.engineError = chunk.error.message || 'the engine reported an error';
			log('engine error frame:', st.engineError);
			return;
		}
		backend.rewriteChunk(chunk, st);

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
		st.truncated = (!st.sawDone || Boolean(st.engineError)) && !abort.signal.aborted;
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

function ratesFrom(snap, sampleAt, keys) {
	const now = sampleAt || Date.now();
	const prev = rateWindow;
	rateWindow = { at: now };
	for (const k of Object.values(keys)) rateWindow[k] = snap[k] || 0;
	if (!prev) return {};

	const grew = (k) => (snap[k] || 0) >= (prev[k] || 0);
	// an engine restart zeroes the counters, which would read as a huge
	// negative throughput. Drop the window and start a new one.
	if (!Object.values(keys).every(grew)) return {};

	const seconds = (now - prev.at) / 1000;
	// A long gap means the window mostly covers idle time. Averaging over it
	// would report a "current" rate that is really a stale average, so give up
	// on rates for this sample and let the next poll measure a short window.
	if (seconds < 0.5 || seconds > 60) return { window_s: round1(seconds) };
	const per = (k) => round1(((snap[k] || 0) - (prev[k] || 0)) / seconds);
	const out = { window_s: round1(seconds) };
	if (keys.gen) out.generation_tokens_per_second = per(keys.gen);
	if (keys.prompt) out.prompt_tokens_per_second = per(keys.prompt);
	if (keys.drafts) out.steps_per_second = per(keys.drafts);
	if (keys.accepted && keys.draftTokens && snap[keys.draftTokens] > prev[keys.draftTokens]) {
		const dDraft = snap[keys.draftTokens] - prev[keys.draftTokens];
		out.window_accept_percent = dDraft > 0 ? round1(((snap[keys.accepted] - prev[keys.accepted]) / dDraft) * 100) : 0;
	}
	return out;
}

// Short cache: the UI polls this every few seconds, and several tabs should
// cost one upstream read.
let statsCache = { at: 0, snap: null };

async function statsSnapshot() {
	if (statsCache.snap && Date.now() - statsCache.at < 1500) return statsCache;
	const snap = await safeSnapshot({});
	statsCache = { at: Date.now(), snap };
	return statsCache;
}

async function handleStats(req, res) {
	const { at, snap } = await statsSnapshot();
	if (!snap) return sendJson(res, 502, { error: `cannot read stats from ${backend.id}` });

	sendJson(res, 200, {
		title: backend.title,
		sampled_at: new Date(at).toISOString(),
		gauges: backend.gauges(snap),
		labels: backend.labels,
		rates: ratesFrom(snap, at, backend.rateKeys),
		counters: backend.counters(snap),
		engine: backend.engine(snap)
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

const NOT_FOUND_STREAM = { error: { message: 'stream replay is a llama.cpp feature, not available on this backend' } };

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
		return sendJson(res, models ? 200 : 502, models || { error: { message: `cannot reach the backend at ${cfg.upstream}` } });
	}
	if (p === '/v1/chat/completions' && method === 'POST') return handleChat(req, res);
	if (p === '/v1/chat/completions/control' && method === 'POST') return sendJson(res, 200, { success: false, error: `not supported by ${backend.id}` });
	if (p === '/slots') return sendJson(res, 200, []);
	if (p === '/tools') return sendJson(res, 200, []);
	if (p === '/v1/streams/lookup' && method === 'POST') return sendJson(res, 200, []);
	if (p === '/v1/stream') {
		if (method === 'DELETE') return sendJson(res, 200, { success: true });
		return sendJson(res, 404, NOT_FOUND_STREAM);
	}
	if (p === '/build.json') return sendJson(res, 200, { version: await backendVersion() });
	if (p === '/vllm/stats' || p === '/engine/stats') return handleStats(req, res);
	if (p.startsWith('/models') || p === '/cors-proxy' || p === '/completion' || p === '/tokenize') {
		return sendJson(res, 501, { success: false, error: `not available on a ${backend.id} backend (${p})` });
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
			flags.reasoning = true;
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
		flags.vision = cfg.vision;
		log(`vision: ${flags.vision ? 'on' : 'off'} (VLLM_MODALITY_VISION)`);
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
			flags.vision = true;
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

// Pick the backend first: everything below depends on what it can tell us.
const chosen = await selectBackend();
backend = chosen.def;

// Capability probes only run for a backend that has nothing better to say.
// Strata reports vision and thinking in /props, so probing it again is waste.
probesReady = (async () => {
	if (backend.probes.thinking) await probeThinkingSupport();
	else log('thinking support: from /props');
	if (backend.probes.vision) await probeVisionSupport();
	else log('vision support: from /props');
})();

server.listen(cfg.port, cfg.host, async () => {
	const models = await listModels(true);
	await safeSnapshot({ force: true });
	log(`llama.cpp webui -> ${backend.id} adapter`);
	log(`  ui        http://${cfg.host}:${cfg.port}/`);
	log(`  backend   ${cfg.upstream}`);
	if (usingDotEnv) log('  loaded    .env (gitignored)');
	log(`  model     ${models?.data?.map((m) => m.id).join(', ') || 'none reported'}`);
	log(`  timings   ${backend.windowsTimings ? (cfg.engineTimings ? 'metrics window, wall-clock fallback' : 'wall clock only') : 'reported by the engine'}`);
	await probeWait();
});

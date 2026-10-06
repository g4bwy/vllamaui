#!/usr/bin/env node
// Stand-in for the Strata inference server (serve/server.py), for testing the
// adapter without a GPU. Run it with: node tests/mock-strata.mjs [port]
//
//   GET  /health /props /slots /metrics /v1/status /v1/models /models
//   POST /v1/chat/completions
//
// Shapes come from the real server, captured on a `--engine mock` run. Two
// details differ on purpose: this answers HTTP/1.1 with chunked encoding, where
// Strata answers HTTP/1.0 and ends a stream by closing the socket, and the real
// mock engine reports no timings at all, so the `timings` member here exists to
// exercise the pass-through path. README.md lists what each backend gives.
//
// Request body switches on /v1/chat/completions:
//   "truncate": true          close after three content chunks, no [DONE]
//   "error_mid_stream": true  one engine error frame, then [DONE]
//   "no_timings": true        omit timings on the final chunk, keep usage
// Environment switches:
//   MOCK_API_KEY=secret       require the key on every route but /health
//   MOCK_VISION=1             the model sees, and image_url parts are accepted
//   MOCK_TIMINGS=0            same as the "no_timings" body switch
import http from 'node:http';

const port = Number(process.argv[2] || 8121);
const MODEL = 'qwen3.8-flash-next';
const MAX_CTX = 32768;
const vision = process.env.MOCK_VISION === '1';
const apiKey = process.env.MOCK_API_KEY || '';
const timingsOn = process.env.MOCK_TIMINGS !== '0';

// cumulative counters, like Strata's Service.totals: an adapter can diff them
const totals = { since: Date.now() / 1000, requests: 0, prompt_tokens: 0, reused: 0, output_tokens: 0, prompt_ms: 0, decode_ms: 0 };
const history = []; // last requests, newest first
const seen = new Map(); // prompt text -> token count, the source of cached_tokens
let busy = null; // { phase, prompt_total, prompt_read, generated, started }

const TEMPLATE = [
	'{%- if enable_thinking is defined and enable_thinking is false %}{{ "<|im_start|>assistant\\n" }}{%- else %}',
	'{{ ' + "<|im_start|>assistant\\n<|im_start|>\\n" + ' }}{%- endif %}',
	'{%- for message in messages %}{{ message.content }}{% endfor %}'
].join('\n');

const ANSWER =
	'The quick brown fox jumps over the lazy dog while the rain keeps falling on the tin roof of the shed. ' +
	'It is a dull afternoon, so the dog sleeps anyway and the fox watches the gate for something to happen.';
const THINKING =
	'The user wants a plain answer to a short prompt. I will restate a sentence with enough words in it to ' +
	'make the token count and the decode time measurable for the client reading this stream.';

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
const round1 = (v) => Math.round(v * 10) / 10;
const hex = () => Math.random().toString(16).slice(2, 26).padEnd(24, '0');

function json(res, status, body) {
	const text = JSON.stringify(body);
	res.writeHead(status, { 'Content-Type': 'application/json', 'Content-Length': Buffer.byteLength(text) });
	res.end(text);
}

function tokenCount(body) {
	return Math.max(12, Math.ceil(JSON.stringify(body.messages ?? []).length / 4));
}

function chunks(text, size) {
	const words = text.split(/\s+/).filter(Boolean);
	const out = [];
	for (let i = 0; i < words.length; i += size) out.push((i ? ' ' : '') + words.slice(i, i + size).join(' '));
	return out;
}

function props() {
	return {
		default_generation_settings: { n_ctx: MAX_CTX, params: { n_predict: -1 } },
		total_slots: 1,
		model_alias: MODEL,
		chat_template: TEMPLATE,
		modalities: { vision },
		models_autoload: false,
		is_sleeping: false
	};
}

function models() {
	return {
		object: 'list',
		data: [
			{
				id: MODEL,
				object: 'model',
				status: { value: 'loaded' },
				meta: { n_ctx: MAX_CTX },
				architecture: { input_modalities: vision ? ['text', 'image'] : ['text'], output_modalities: ['text'] }
			}
		]
	};
}

function timings(prompt, cached, gen, prompt_ms, decode_ms) {
	const computed = Math.max(0, prompt - cached);
	return {
		cache_n: cached,
		prompt_n: computed,
		prompt_ms: round1(prompt_ms),
		prompt_per_token_ms: computed ? round1(prompt_ms / computed) : null,
		prompt_per_second: computed ? round1(computed / (prompt_ms / 1000)) : null,
		predicted_n: gen,
		predicted_ms: round1(decode_ms),
		predicted_per_token_ms: gen ? round1(decode_ms / gen) : null,
		predicted_per_second: gen ? round1(gen / (decode_ms / 1000)) : null,
		// about 62% of drafted tokens accepted, llama.cpp's names
		draft_n: Math.round(gen * 1.6),
		draft_n_accepted: Math.round(gen * 1.6 * 0.62)
	};
}

function metrics() {
	return {
		engine: { model: MODEL, max_context: MAX_CTX, images: vision },
		live: {
			state: busy ? (busy.phase === 'reading' ? 'reading' : 'generating') : 'idle',
			queued: 0,
			phase: busy ? busy.phase : null,
			prompt_tokens: busy ? busy.prompt_total : null,
			prompt_read: busy && busy.phase === 'reading' ? busy.prompt_read : null,
			prompt_total: busy && busy.phase === 'reading' ? busy.prompt_total : null,
			generated: busy && busy.phase === 'decoding' ? busy.generated : null,
			max_tokens: null,
			elapsed_s: busy ? round1((Date.now() - busy.started) / 1000) : null,
			tok_s: busy && busy.phase === 'decoding' ? 41.3 : null,
			tok_s_mean: busy && busy.phase === 'decoding' ? 40.1 : null,
			prefill_tok_s_mean: null,
			tok_s_window_s: null
		},
		requests: history.slice(0, 12),
		requests_kept: history.length,
		totals,
		hardware: { cpu: 22.5, ram_used: 3_200_000_000, ram_total: 64_000_000_000, tok_s: 0 },
		hardware_static: { gpu_name: vision ? 'Mock GPU' : null, gpu_count: 1, cpu_name: 'mock', threads: 8, psutil: false },
		history: {},
		time: Date.now() / 1000
	};
}

function v1Status() {
	const last = history[0];
	return {
		service: 'strata',
		model: MODEL,
		loaded: true,
		auto_load: false,
		engine: 'mock',
		started: Math.round(totals.since),
		uptime_s: Math.round(Date.now() / 1000 - totals.since),
		cache_max_tokens: MAX_CTX,
		context: { native: MAX_CTX, max_positions: MAX_CTX },
		concurrency: { serving: 1, requested: 1 },
		dialects: ['/v1/chat/completions', '/v1/messages'],
		vision: { enabled: vision, available: vision, error: null },
		activity: { requests: totals.requests, in_flight: busy ? 1 : 0 },
		last_timings: last ? last.timings : null,
		// a GPU appears here so the VRAM row has something to render; the real
		// mock engine build reports gpu_name null and the row hides itself
		machine: {
			gpu: vision ? { name: 'Mock GPU', used_mib: 6144, total_mib: 12288, util_pct: 97, temp_c: 66, power_w: 210 } : null,
			ram: { used_gib: 3, total_gib: 64 }
		}
	};
}

function authorized(req) {
	if (!apiKey) return true;
	const header = req.headers.authorization || '';
	const bearer = header.toLowerCase().startsWith('bearer ') ? header.slice(7) : '';
	return bearer === apiKey || req.headers['x-api-key'] === apiKey;
}

function hasImage(body) {
	return (body.messages ?? []).some((m) => Array.isArray(m.content) && m.content.some((p) => p?.type === 'image_url'));
}

// one completion: the numbers a request produces, shared by both response modes
function plan(body) {
	const prompt = tokenCount(body);
	const key = JSON.stringify(body.messages ?? '');
	const cached = seen.has(key) ? Math.round(prompt * 0.9) : 0;
	seen.set(key, prompt);
	// max_tokens really limits the answer, so a test can tell 1 from 42
	const full = chunks(ANSWER, 3).length * 3;
	const cap = Number(body.max_tokens ?? body.max_completion_tokens ?? 0);
	const gen = cap > 0 ? Math.max(1, Math.min(full, cap)) : full;
	const prompt_ms = Math.max(1, (prompt - cached) / 1.4); // about 1400 tokens/s
	const decode_ms = (gen / 40) * 1000; // about 40 tokens/s
	return { prompt, cached, gen, prompt_ms, decode_ms };
}

function record(p, t) {
	const pm = Number(p.prompt_ms) || 0;
	const dm = Number(p.decode_ms) || 0;
	totals.requests += 1;
	totals.prompt_tokens += p.prompt;
	totals.reused += p.cached;
	totals.output_tokens += p.gen;
	totals.prompt_ms += pm;
	totals.decode_ms += dm;
	history.unshift({
		prompt_ms: round1(pm),
		decode_ms: round1(dm),
		decode_tok_s: dm > 0 ? round1(p.gen / (dm / 1000)) : 0,
		reused: p.cached,
		finish: 'stop',
		duration_s: round1((p.prompt_ms + p.decode_ms) / 1000),
		prompt_tokens: p.prompt,
		completion_tokens: p.gen,
		timings: t
	});
	if (history.length > 24) history.pop();
}

async function streamChat(req, res, body) {
	const p = plan(body);
	const id = 'chatcmpl-' + hex();
	const created = Math.floor(Date.now() / 1000);
	const send = (obj) => res.write(`data: ${JSON.stringify(obj)}\n\n`);
	const frame = (delta, finish) => ({ id, object: 'chat.completion.chunk', created, model: MODEL, choices: [{ index: 0, delta, finish_reason: finish ?? null }] });

	res.writeHead(200, { 'Content-Type': 'text/event-stream', 'Cache-Control': 'no-cache', Connection: 'close' });
	busy = { phase: 'reading', prompt_total: p.prompt, prompt_read: 0, generated: 0, started: Date.now() };
	send(frame({ role: 'assistant', content: '' }));
	// Strata holds the connection with SSE comments while it reads the prompt
	for (let i = 0; i < 3; i++) {
		await sleep(120);
		res.write(': keep-alive\n\n');
		busy.prompt_read = Math.round((p.prompt * (i + 1)) / 3);
	}
	busy.phase = 'decoding';

	const wantTimings = timingsOn && body.no_timings !== true;

	if (body.error_mid_stream === true) {
		for (const piece of chunks(ANSWER, 3).slice(0, 2)) {
			await sleep(60);
			send(frame({ content: piece }));
			busy.generated += 3;
		}
		send({ error: { type: 'server_error', message: 'the engine stopped unexpectedly (mock)' } });
		res.write('data: [DONE]\n\n');
		record({ ...p, gen: busy.generated }, null);
		busy = null;
		return res.end();
	}

	for (const piece of chunks(THINKING, 4)) {
		await sleep(35);
		send(frame({ reasoning_content: piece }));
	}
	const content = chunks(ANSWER, 3);
	for (let i = 0; i < content.length; i++) {
		await sleep(45);
		send(frame({ content: content[i] }));
		busy.generated += 3;
		if (body.truncate === true && i === 2) {
			// a real engine failure or client hang-up: the socket just goes
			busy = null;
			return res.socket?.destroy();
		}
	}
	const gen = body.truncate === true ? busy.generated : p.gen;
	const last = frame({}, 'stop');
	last.usage = {
		prompt_tokens: p.prompt,
		completion_tokens: gen,
		total_tokens: p.prompt + gen,
		prompt_tokens_details: { cached_tokens: p.cached }
	};
	if (wantTimings) last.timings = timings(p.prompt, p.cached, gen, p.prompt_ms, p.decode_ms);
	send(last);
	res.write('data: [DONE]\n\n');
	record({ ...p, gen }, wantTimings ? last.timings : null);
	busy = null;
	res.end();
}

async function collectChat(res, body) {
	const p = plan(body);
	await sleep(180);
	const wantTimings = timingsOn && body.no_timings !== true;
	const t = wantTimings ? timings(p.prompt, p.cached, p.gen, p.prompt_ms, p.decode_ms) : null;
	record(p, t);
	json(res, 200, {
		id: 'chatcmpl-' + hex(),
		object: 'chat.completion',
		created: Math.floor(Date.now() / 1000),
		model: MODEL,
		choices: [{ index: 0, message: { role: 'assistant', content: ANSWER, reasoning_content: THINKING }, finish_reason: 'stop' }],
		usage: {
			prompt_tokens: p.prompt,
			completion_tokens: p.gen,
			total_tokens: p.prompt + p.gen,
			prompt_tokens_details: { cached_tokens: p.cached }
		},
		...(t ? { timings: t } : {})
	});
}

const server = http.createServer(async (req, res) => {
	const path = (req.url || '/').split('?')[0].replace(/\/+$/, '') || '/';

	if (apiKey && path !== '/health' && !authorized(req)) {
		return json(res, 401, { error: { type: 'authentication_error', message: 'missing or wrong API key' } });
	}

	if (req.method === 'GET') {
		if (path === '/health') return json(res, 200, { status: 'ok', max_context: MAX_CTX, model: MODEL, images: vision, api_key: Boolean(apiKey), loaded: true, service: 'strata' });
		if (path === '/props') return json(res, 200, props());
		if (path === '/models' || path === '/v1/models') return json(res, 200, models());
		if (path === '/slots') return json(res, 200, [{ id: 0, n_ctx: MAX_CTX, is_processing: Boolean(busy) }]);
		if (path === '/metrics') return json(res, 200, metrics());
		if (path === '/v1/status' || path === '/status') return json(res, 200, path === '/status' ? { busy: Boolean(busy), queued: 0 } : v1Status());
		return json(res, 404, { error: { message: 'not found' } });
	}

	if (req.method !== 'POST') return json(res, 405, { error: { message: 'method not allowed' } });

	let body = '';
	for await (const c of req) body += c;
	let parsed = {};
	try {
		parsed = body ? JSON.parse(body) : {};
	} catch {
		return json(res, 400, { error: { type: 'invalid_request_error', message: 'send a JSON object' } });
	}

	if (path !== '/v1/chat/completions') return json(res, 404, { error: { message: 'not found' } });
	if (hasImage(parsed) && !vision) {
		return json(res, 400, { error: { type: 'invalid_request_error', message: "This model's architecture does not support image inputs." } });
	}

	res.on('error', () => {});
	if (parsed.stream) return streamChat(req, res, parsed);
	return collectChat(res, parsed);
});

server.listen(port, '127.0.0.1', () => console.log(`mock Strata on http://127.0.0.1:${port} (vision ${vision ? 'on' : 'off'}, timings ${timingsOn ? 'on' : 'off'})`));

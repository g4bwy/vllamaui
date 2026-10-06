#!/usr/bin/env node
// Minimal stand-in for vLLM, for testing the adapter without a GPU.
//   node tests/mock-vllm.mjs [port]
//
//   GET  /v1/models, /version, /metrics   static answers
//   POST /v1/chat/completions             streams a fixed answer
//
// Query and header switches on /v1/chat/completions:
//   "truncate": true            stop mid-stream, no [DONE]
//   "tool_batch": true          stream tool calls numbered globally, split by a
//                               content chunk (the shape that broke the webui)
//   X-Mock-Status: 400          answer with an OpenAI-style error body
import http from 'node:http';

const port = Number(process.argv[2] || 8011);
let tokens = 0;

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

// Cumulative counters that advance with every request, the way a real engine's
// do. Frozen counters hide bugs in a proxy that diffs two scrapes, and hid one.
const counters = {
	requests: 0,
	prefill_s: 0,
	decode_s: 0,
	computed: 0,
	prefixHits: 0,
	prefixQueries: 0,
	promptTokens: 0,
	ttft_s: 0,
	itl_s: 0,
	drafts: 0,
	draftTokens: 0,
	accepted: 0
};

// the prompt speed the mock claims, in tokens per second
const PREFILL_TPS = 1400;

const seenPrompts = new Map();

function recordRequest(prompt, computed, cached, gen) {
	counters.requests += 1;
	counters.prefill_s += computed / PREFILL_TPS; // about 1400 tokens/s reading the prompt
	counters.decode_s += gen / 40; // about 40 tokens/s writing the answer
	counters.computed += computed;
	counters.prefixQueries += prompt;
	counters.prefixHits += cached;
	counters.promptTokens += prompt;
	counters.ttft_s += computed / PREFILL_TPS;
	counters.itl_s += gen > 1 ? gen / 40 : 0;
	counters.drafts += 1;
	counters.draftTokens += Math.round(gen * 1.6);
	counters.accepted += Math.round(gen * 1.6 * 0.62);
	tokens += gen;
}

const WORDS = 'the quick brown fox jumps over the lazy dog again and again'.split(' ');

const server = http.createServer(async (req, res) => {
	const url = new URL(req.url, 'http://x');

	if (url.pathname === '/v1/models') {
		res.writeHead(200, { 'Content-Type': 'application/json' });
		return res.end(JSON.stringify({ object: 'list', data: [{ id: 'mock-model', object: 'model', max_model_len: 32768, owned_by: 'mock' }] }));
	}
	if (url.pathname === '/version') {
		res.writeHead(200, { 'Content-Type': 'application/json' });
		return res.end(JSON.stringify({ version: 'mock-1.0' }));
	}
	if (url.pathname === '/metrics') {
		res.writeHead(200, { 'Content-Type': 'text/plain' });
		return res.end(
			[
				`vllm:request_prefill_time_seconds_count ${counters.requests}`,
				`vllm:request_prefill_time_seconds_sum ${counters.prefill_s}`,
				`vllm:request_decode_time_seconds_count ${counters.requests}`,
				`vllm:request_decode_time_seconds_sum ${counters.decode_s}`,
				`vllm:request_prefill_kv_computed_tokens_count ${counters.requests}`,
				`vllm:request_prefill_kv_computed_tokens_sum ${counters.computed}`,
				`vllm:prefix_cache_hits_total ${counters.prefixHits}`,
				`vllm:prefix_cache_queries_total ${counters.prefixQueries}`,
				`vllm:generation_tokens_total ${tokens}`,
				`vllm:prompt_tokens_total ${counters.promptTokens}`,
				'vllm:num_requests_running 0',
				'vllm:num_requests_waiting 0',
				'vllm:kv_cache_usage_perc 0.25',
				`vllm:time_to_first_token_seconds_count ${counters.requests}`,
				`vllm:time_to_first_token_seconds_sum ${counters.ttft_s}`,
				`vllm:inter_token_latency_seconds_count ${counters.requests}`,
				`vllm:inter_token_latency_seconds_sum ${counters.itl_s}`,
				`vllm:spec_decode_num_drafts_total ${counters.drafts}`,
				`vllm:spec_decode_num_draft_tokens_total ${counters.draftTokens}`,
				`vllm:spec_decode_num_accepted_tokens_total ${counters.accepted}`,
				'vllm:cache_config_info{block_size="16",num_gpu_blocks="100",kv_cache_size_tokens="1600",enable_prefix_caching="True",gpu_memory_utilization="0.9"} 1',
				''
			].join('\n')
		);
	}

	if (url.pathname !== '/v1/chat/completions') {
		res.writeHead(404, { 'Content-Type': 'application/json' });
		return res.end(JSON.stringify({ error: { message: 'mock: no route ' + url.pathname } }));
	}

	let body = '';
	for await (const c of req) body += c;
	const parsed = JSON.parse(body || '{}');

	// by default this fake cannot see, which is how to exercise the vision probe
	if (body.includes('image_url') && process.env.MOCK_VISION !== '1') {
		res.writeHead(400, { 'Content-Type': 'application/json' });
		return res.end(
			JSON.stringify({
				error: {
					message: "This model's architecture does not support image inputs. Start the server with a multimodal model.",
					type: 'BadRequestError',
					code: 400
				}
			})
		);
	}

	if (req.headers['x-mock-status']) {
		res.writeHead(Number(req.headers['x-mock-status']), { 'Content-Type': 'application/json' });
		return res.end(JSON.stringify({ error: { message: 'mock: deliberate failure' } }));
	}

	// prompt size and reuse follow the body, so a proxy diffing counters sees
	// movement that matches what it streamed
	const prompt = Math.max(12, Math.ceil(JSON.stringify(parsed.messages ?? []).length / 4));
	const key = JSON.stringify(parsed.messages ?? '');
	const cached = seenPrompts.has(key) ? Math.round(prompt * 0.9) : 0;
	seenPrompts.set(key, prompt);

	// No token may arrive before the prefill the counters claim took. A proxy
	// that diffs those counters against its own clock is right to refuse a window
	// where the engine reports more prefill time than the request ever waited, so
	// the mock has to wait like the speed it advertises.
	await sleep(Math.max(20, ((prompt - cached) / PREFILL_TPS) * 1000));

	// "tool_batch": true replays the frame order that broke the webui: vLLM
	// numbers the calls globally, and a stray newline splits them into two
	// batches. See server/adapter.mjs, renumberDeltaToolCalls.
	if (parsed.tool_batch === true && parsed.stream) {
		res.writeHead(200, { 'Content-Type': 'text/event-stream' });
		const frame = (delta) => res.write(`data: ${JSON.stringify({ id: 'mock-tools', model: parsed.model, choices: [{ index: 0, delta, finish_reason: null }] })}\n\n`);
		const call = (index, part) => frame({ tool_calls: [{ index, ...part }] });
		frame({ role: 'assistant', content: '' });
		frame({ reasoning: 'I need two searches' });
		frame({ reasoning: 'one for tech, one for world news' });
		frame({ content: 'Let me look that up. ' });
		call(0, { id: 'call_a', type: 'function', function: { name: 'search_news', arguments: '' } });
		call(0, { function: { arguments: '{"query":"tech' } });
		call(0, { function: { arguments: '"}' } });
		frame({ content: '\n' });
		call(1, { id: 'call_b', type: 'function', function: { name: 'search_news', arguments: '' } });
		call(1, { function: { arguments: '{"query":"world' } });
		call(1, { function: { arguments: ' news"}' } });
		res.write(`data: ${JSON.stringify({ id: 'mock-tools', model: parsed.model, choices: [{ index: 0, delta: {}, finish_reason: 'tool_calls' }] })}\n\n`);
		recordRequest(prompt, prompt - cached, cached, 12);
		res.write(`data: ${JSON.stringify({ id: 'mock-tools', model: parsed.model, choices: [], usage: { prompt_tokens: prompt, completion_tokens: 12, total_tokens: prompt + 12 } })}\n\n`);
		res.write('data: [DONE]\n\n');
		return res.end();
	}

	if (parsed.stream) {
		res.writeHead(200, { 'Content-Type': 'text/event-stream' });
		res.write(`data: ${JSON.stringify({ id: 'mock-1', model: parsed.model, choices: [{ index: 0, delta: { role: 'assistant', content: '' }, finish_reason: null }] })}\n\n`);
		// reasoning first, in vLLM's spelling, to exercise the rename
		res.write(`data: ${JSON.stringify({ id: 'mock-1', model: parsed.model, choices: [{ index: 0, delta: { reasoning: 'thinking hard' }, token_ids: [1, 2] }] })}\n\n`);
		const truncate = parsed.truncate === true;
		const count = truncate ? 3 : WORDS.length;
		for (let i = 0; i < count; i++) {
			await sleep(60);
			res.write(`data: ${JSON.stringify({ id: 'mock-1', model: parsed.model, choices: [{ index: 0, delta: { content: ' ' + WORDS[i % WORDS.length] }, token_ids: [i + 10] }] })}\n\n`);
		}
		if (truncate) {
			req.socket.destroy();
			console.log('mock: truncated the stream on purpose');
			return;
		}
		const gen = count + 1;
		recordRequest(prompt, prompt - cached, cached, gen);
		res.write(`data: ${JSON.stringify({ id: 'mock-1', model: parsed.model, choices: [], usage: { prompt_tokens: prompt, completion_tokens: gen, total_tokens: prompt + gen } })}\n\n`);
		res.write('data: [DONE]\n\n');
		return res.end();
	}

	recordRequest(prompt, prompt - cached, cached, 4);
	res.writeHead(200, { 'Content-Type': 'application/json' });
	res.end(
		JSON.stringify({
			id: 'mock-1',
			model: parsed.model,
			choices: [{ index: 0, message: { role: 'assistant', content: 'mock reply', reasoning: 'mock thought' }, finish_reason: 'stop' }],
			usage: { prompt_tokens: 40, completion_tokens: 4, total_tokens: 44 }
		})
	);
});

server.listen(port, () => console.log(`mock vLLM on http://127.0.0.1:${port}`));

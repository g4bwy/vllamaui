#!/usr/bin/env node
// Minimal stand-in for vLLM, for testing the adapter without a GPU.
//   node tests/mock-vllm.mjs [port]
//
//   GET  /v1/models, /version, /metrics   static answers
//   POST /v1/chat/completions             streams a fixed answer
//
// Query and header switches on /v1/chat/completions:
//   "truncate": true            stop mid-stream, no [DONE]
//   X-Mock-Status: 400          answer with an OpenAI-style error body
import http from 'node:http';

const port = Number(process.argv[2] || 8011);
let tokens = 0;

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
				'vllm:request_prefill_time_seconds_count 5',
				'vllm:request_prefill_time_seconds_sum 1.25',
				'vllm:request_decode_time_seconds_count 5',
				'vllm:request_decode_time_seconds_sum 12.5',
				'vllm:request_prefill_kv_computed_tokens_count 5',
				'vllm:request_prefill_kv_computed_tokens_sum 1000',
				'vllm:prefix_cache_hits_total 400',
				'vllm:prefix_cache_queries_total 1400',
				`vllm:generation_tokens_total ${tokens}`,
				'vllm:prompt_tokens_total 2000',
				'vllm:num_requests_running 0',
				'vllm:num_requests_waiting 0',
				'vllm:kv_cache_usage_perc 0.25',
				'vllm:time_to_first_token_seconds_count 5',
				'vllm:time_to_first_token_seconds_sum 2.5',
				'vllm:inter_token_latency_seconds_count 5',
				'vllm:inter_token_latency_seconds_sum 0.02',
				'vllm:spec_decode_num_drafts_total 100',
				'vllm:spec_decode_num_draft_tokens_total 300',
				'vllm:spec_decode_num_accepted_tokens_total 150',
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

	if (parsed.stream) {
		res.writeHead(200, { 'Content-Type': 'text/event-stream' });
		res.write(`data: ${JSON.stringify({ id: 'mock-1', model: parsed.model, choices: [{ index: 0, delta: { role: 'assistant', content: '' }, finish_reason: null }] })}\n\n`);
		// reasoning first, in vLLM's spelling, to exercise the rename
		res.write(`data: ${JSON.stringify({ id: 'mock-1', model: parsed.model, choices: [{ index: 0, delta: { reasoning: 'thinking hard' }, token_ids: [1, 2] }] })}\n\n`);
		const truncate = parsed.truncate === true;
		const count = truncate ? 3 : WORDS.length;
		for (let i = 0; i < count; i++) {
			await new Promise((r) => setTimeout(r, 60));
			res.write(`data: ${JSON.stringify({ id: 'mock-1', model: parsed.model, choices: [{ index: 0, delta: { content: ' ' + WORDS[i % WORDS.length] }, token_ids: [i + 10] }] })}\n\n`);
			tokens += 1;
		}
		if (truncate) {
			req.socket.destroy();
			console.log('mock: truncated the stream on purpose');
			return;
		}
		res.write(`data: ${JSON.stringify({ id: 'mock-1', model: parsed.model, choices: [], usage: { prompt_tokens: 40, completion_tokens: count + 1, total_tokens: 41 } })}\n\n`);
		res.write('data: [DONE]\n\n');
		return res.end();
	}

	tokens += 4;
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

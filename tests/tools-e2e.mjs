// Agentic round trip: the model must call a real tool, get the result back
// through POST /tools, and answer from it. Fails if no tool call happens.
//   node tests/tools-e2e.mjs            (needs the Go server with --tools)
import { chromium } from '../frontend/node_modules/playwright/index.mjs';

const URL = process.env.UI_URL || 'http://localhost:8085/';
const FILE = process.env.TOOL_FILE || 'go.mod';
const ASK = process.env.TOOL_ASK ||
	`Use the read_file tool to read the file "${FILE}" and tell me the module name declared in it. Answer with the module name only.`;
const TOOL = process.env.TOOL_NAME || 'read_file';
const EXPECT = process.env.TOOL_EXPECT || 'llama-webui/server';
// the answer text is only assertable against a scripted engine; skip with
// TOOL_EXPECT=skip when the model produces live content

const browser = await chromium.launch({ args: ['--no-sandbox', '--disable-dev-shm-usage'] });
const page = await browser.newPage({ viewport: { width: 1400, height: 950 } });

const problems = [];
const toolCalls = [];
const callIds = new Set();
const toolPosts = [];
page.on('pageerror', (e) => problems.push('pageerror: ' + e.message.slice(0, 160)));
page.on('response', async (res) => {
	const u = res.url();
	if (u.endsWith('/tools') && res.request().method() === 'POST') {
		let out = '';
		try {
			out = (await res.text()).slice(0, 240);
		} catch {}
		toolPosts.push({ body: res.request().postData(), status: res.status(), result: out });
	}
	if (u.endsWith('/chat/completions')) {
		try {
			const text = await res.text();
			for (const line of text.split('\n')) {
				if (!line.startsWith('data:')) continue;
				try {
					const c = JSON.parse(line.slice(5));
					for (const tc of c.choices?.[0]?.delta?.tool_calls ?? []) {
						if (tc.function?.name) toolCalls.push(tc.function.name);
						if (tc.id) callIds.add(tc.id);
					}
				} catch {}
			}
		} catch {}
	}
});

await page.goto(URL, { waitUntil: 'networkidle', timeout: 60000 });
const ta = page.locator('textarea').first();
await ta.waitFor({ timeout: 20000 });

// The server group only appears when GET /tools returns entries, so ask the
// same endpoint the panel uses. Clicking the "+" menu instead leaves it open and
// the keystrokes below never reach the composer.
const listed = await page.evaluate(async () => (await fetch('./tools')).json());
const serverTools = Array.isArray(listed) ? listed.filter((t) => t.type === 'server').map((t) => t.tool) : [];
const mcpTools = Array.isArray(listed) ? listed.filter((t) => t.type === 'mcp').map((t) => t.tool) : [];
console.log('GET /tools:', serverTools.length, 'server', serverTools.join(','), '|', mcpTools.length, 'mcp', mcpTools.join(','));
await page.screenshot({ path: 'shots/tools-loaded.png' });

await ta.fill(ASK);
await ta.press('Enter');

const approvals = [];
let prev = '';
let stable = 0;
for (let i = 0; i < 150; i++) {
	await page.waitForTimeout(1000);
	// llama.cpp asks before running a tool, so answering that prompt is part of
	// the flow. Without this the loop sits parked on the dialog.
	const allow = page.getByText('Allow once', { exact: false }).first();
	if (await allow.count().catch(() => 0)) {
		const ok = await allow.click({ timeout: 2000 }).then(() => true).catch(() => false);
		if (ok) {
			approvals.push('Allow once');
			stable = 0;
			continue;
		}
	}
	const txt = await page.locator('body').innerText();
	stable = txt === prev ? stable + 1 : 0;
	prev = txt;
	if (stable >= 8) break;
}
await page.screenshot({ path: 'shots/tools-answer.png', fullPage: true });

const body = await page.locator('body').innerText();
// the tools panel probes the home directory once with file_glob_search; that is
// the client's own discovery, not a call the model asked for
const modelCalls = toolPosts.filter((p) => !/"tool":"file_glob_search"/.test(p.body || ''));
const emptyNamed = toolCalls.filter((n) => !n || !n.trim()).length;
const phantom = modelCalls.filter((p) => {
	const m = /"tool":"([^"]*)"/.exec(p.body || '');
	return !m || m[1] === '' || /"params":\{\s*\}/.test(p.body || '');
}).length;
const gotToolCall = toolCalls.includes(TOOL);
const toolOk = toolPosts.some((p) => p.status === 200);
const answered = body.toLowerCase().includes(EXPECT.toLowerCase());

console.log('model asked to call:', toolCalls.join(',') || '(none)');
console.log('permission prompts answered:', approvals.join(', ') || '(none)');
for (const p of toolPosts) {
	console.log(`POST /tools -> ${p.status}`, String(p.body || '').slice(0, 120));
	console.log('   result:', p.result);
}
console.log('calls the model asked for:', toolCalls.length, '| distinct ids:', callIds.size, '| POSTs the client made:', modelCalls.length);
console.log('tool calls with an empty name:', emptyNamed);
console.log('phantom calls (no tool name or empty params):', phantom);
console.log('tool result reached the UI:', toolOk);
if (EXPECT !== 'skip') console.log('answer contains ' + EXPECT + ':', answered);
console.log('last lines:', JSON.stringify(body.split('\n').map((l) => l.trim()).filter(Boolean).slice(-6)));
console.log('problems:', [...new Set(problems)].join(' | ') || '(none)');
await browser.close();
const expectAnswer = EXPECT !== 'skip';
// ids and posts can differ when the last round is still in flight against a
// live engine, so the hard assertions are the two phantom counters
const clean = emptyNamed === 0 && phantom === 0;
if (modelCalls.length !== callIds.size) console.log('note: distinct ids', callIds.size, 'vs POSTs', modelCalls.length);
process.exit(gotToolCall && toolOk && serverTools.length > 0 && clean && (!expectAnswer || answered) ? 0 : 1);

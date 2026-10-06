// Agentic round trip: the model must call a real tool, get the result back
// through POST /tools, and answer from it. Fails if no tool call happens.
//   node tests/tools-e2e.mjs            (needs the Go server with --tools)
import { chromium } from '../frontend/node_modules/playwright/index.mjs';

const URL = process.env.UI_URL || 'http://localhost:8085/';
const FILE = process.env.TOOL_FILE || 'go.mod';
const ASK = process.env.TOOL_ASK ||
	`Use the read_file tool to read the file "${FILE}" and tell me the module name declared in it. Answer with the module name only.`;
const EXPECT = process.env.TOOL_EXPECT || 'llama-webui/server';

const browser = await chromium.launch({ args: ['--no-sandbox', '--disable-dev-shm-usage'] });
const page = await browser.newPage({ viewport: { width: 1400, height: 950 } });

const problems = [];
const toolCalls = [];
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
const gotToolCall = toolCalls.includes('read_file');
const toolOk = toolPosts.some((p) => p.status === 200);
const answered = body.toLowerCase().includes(EXPECT.toLowerCase());

console.log('model asked to call:', toolCalls.join(',') || '(none)');
console.log('permission prompts answered:', approvals.join(', ') || '(none)');
for (const p of toolPosts) {
	console.log(`POST /tools -> ${p.status}`, String(p.body || '').slice(0, 120));
	console.log('   result:', p.result);
}
console.log('tool result reached the UI:', toolOk);
console.log(`answer contains ${EXPECT}:`, answered);
console.log('last lines:', JSON.stringify(body.split('\n').map((l) => l.trim()).filter(Boolean).slice(-6)));
console.log('problems:', [...new Set(problems)].join(' | ') || '(none)');
await browser.close();
process.exit(gotToolCall && toolOk && answered && serverTools.length > 0 ? 0 : 1);

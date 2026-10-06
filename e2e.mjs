// End-to-end test against a running adapter: drive a chat in a real browser,
// capture live throughput, then open the context gauge and dump the stats block.
//   node e2e.mjs            (expects the adapter on http://localhost:8080)
import { mkdirSync } from 'node:fs';
import { chromium } from './frontend/node_modules/playwright/index.mjs';

const URL = process.env.UI_URL || 'http://localhost:8080/';
const SHOT = process.env.SHOT_DIR || 'shots';
mkdirSync(SHOT, { recursive: true });

const browser = await chromium.launch({ args: ['--no-sandbox', '--disable-dev-shm-usage'] });
const page = await browser.newPage({ viewport: { width: 1400, height: 900 } });

const problems = [];
page.on('pageerror', (e) => problems.push('pageerror: ' + e.message.slice(0, 160)));
page.on('requestfailed', (r) => problems.push('reqfail: ' + r.url() + ' ' + (r.failure()?.errorText || '')));
page.on('response', (r) => {
	if (r.status() >= 400 && !r.url().includes('/v1/stream')) problems.push(`http ${r.status()}: ${r.url()}`);
});

await page.goto(URL, { waitUntil: 'networkidle', timeout: 60000 });
await page.locator('textarea').first().waitFor({ timeout: 20000 });
await page.screenshot({ path: `${SHOT}/01-loaded.png` });

await page.locator('textarea').first().fill('List the ten longest rivers in the world with their approximate lengths, one per line.');
await page.locator('textarea').first().press('Enter');

// catch the live readout while tokens stream: the status line shows t/s from the
// timings object the adapter injects into every chunk
let live = '(no live readout captured)';
for (let i = 0; i < 40; i++) {
	await page.waitForTimeout(400);
	const txt = await page.locator('body').innerText();
	const hit = txt.split('\n').find((l) => /^\s*\d+(\.\d+)?\s*(t\/s|tokens\/s)\s*$/.test(l));
	if (hit) {
		live = hit.trim();
		await page.screenshot({ path: `${SHOT}/02-streaming.png` });
		break;
	}
}

// wait for the finished answer
let prev = '';
let stable = 0;
for (let i = 0; i < 200; i++) {
	await page.waitForTimeout(500);
	const txt = await page.locator('body').innerText();
	stable = txt === prev ? stable + 1 : 0;
	prev = txt;
	if (stable >= 6) break;
}
await page.screenshot({ path: `${SHOT}/03-answer.png`, fullPage: true });

const finalLines = (await page.locator('body').innerText())
	.split('\n')
	.filter((l) => /\d.*\s(t\/s|tokens\/s|tokens)\b/.test(l))
	.slice(0, 10);

// open the context gauge popup
const dial = page.locator('[data-context-gauge-trigger]');
const dialCount = await dial.count();
let popup = '(gauge dial not present)';
if (dialCount) {
	await dial.click();
	await page.waitForTimeout(3500);
	await page.screenshot({ path: `${SHOT}/04-gauge.png`, fullPage: true });
	popup = await page.locator('body').innerText();
	popup = popup.split('\n').slice(0, 400).filter((l, i, a) => l.trim() && a.indexOf(l) === i).join('\n');
}

console.log('live readout during stream:', live);
console.log('per-message throughput lines:');
console.log(finalLines.map((l) => '  ' + l).join('\n') || '  (none)');
console.log('gauge popup:');
console.log(
	popup
		.split('\n')
		.filter((l) => /ENGINE$|engine-wide|KV cache|VRAM|Output|Prompt|Requests|Prefix|Speculative|First token|Context|tokens/i.test(l))
		.map((l) => '  ' + l)
		.slice(0, 20)
		.join('\n')
);
console.log('problems:');
console.log([...new Set(problems)].slice(0, 15).map((p) => '  ' + p).join('\n') || '  (none)');

await browser.close();

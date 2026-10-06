// Attaches a real image through the UI file input and asks what is in it.
// Fails loudly if the image is stripped before it reaches the model.
import { chromium } from '../frontend/node_modules/playwright/index.mjs';

const IMAGE = process.env.TEST_IMAGE || '/tmp/vprobe.png';
const EXPECT = (process.env.TEST_EXPECT || 'MANGO').toUpperCase();
// a scripted backend answers with fixed text, so only the wire is meaningful
const ASSERT_ANSWER = process.env.TEST_EXPECT_ANSWER !== '0';

const browser = await chromium.launch({ args: ['--no-sandbox', '--disable-dev-shm-usage'] });
const page = await browser.newPage({ viewport: { width: 1400, height: 900 } });

const problems = [];
page.on('pageerror', (e) => problems.push('pageerror: ' + e.message.slice(0, 160)));
page.on('response', (r) => {
	if (r.status() >= 400 && !r.url().endsWith('/v1/stream')) problems.push(`http ${r.status()}: ${r.url()}`);
});

await page.goto(process.env.UI_URL || 'http://localhost:8080/', { waitUntil: 'networkidle' });
await page.locator('textarea').first().waitFor();

const props = await page.evaluate(async () => (await fetch('./props')).json());
console.log('props.modalities:', JSON.stringify(props.modalities));

// capture what actually goes over the wire
let sentHadImage = false;
page.on('request', (r) => {
	if (r.url().endsWith('/v1/chat/completions')) {
		const body = r.postData() || '';
		sentHadImage = body.includes('image_url') && body.includes('data:image');
	}
});

const picker = page.locator('input[type="file"]').first();
await picker.setInputFiles(IMAGE);
await page.waitForTimeout(1500);
await page.screenshot({ path: 'shots/07-attached.png' });

const ta = page.locator('textarea').first();
await ta.click();
await ta.type('Read the text printed in this image and name the colored shapes. Answer in one short line.');
await page.screenshot({ path: 'shots/08-with-image.png' });
await ta.press('Enter');

let prev = '';
let stable = 0;
for (let i = 0; i < 160; i++) {
	await page.waitForTimeout(500);
	const txt = await page.locator('body').innerText();
	stable = txt === prev ? stable + 1 : 0;
	prev = txt;
	if (stable >= 8) break;
}
await page.screenshot({ path: 'shots/09-image-answer.png', fullPage: true });

const answer = await page.locator('body').innerText();
const lines = answer.split('\n').filter((l) => l.trim()).slice(-25);
const gotText = answer.toUpperCase().includes(EXPECT);

console.log('request carried an image part:', sentHadImage);
console.log(ASSERT_ANSWER ? `answer mentions ${EXPECT}: ${gotText}` : `answer mention test skipped (scripted backend)`);
console.log('throughput lines:', lines.filter((l) => /t\/s|tokens/i.test(l)).slice(0, 6).join(' | '));
console.log('tail:', JSON.stringify(lines.slice(-6)));
console.log('problems:', [...new Set(problems)].join(' | ') || '(none)');
await browser.close();
process.exit(sentHadImage && (!ASSERT_ANSWER || gotText) ? 0 : 1);

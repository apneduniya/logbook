// Captures the web UI screenshots for the docs site from the local demo
// stack (stack.sh start, after the demo seed). See README.md here.
//
//   node scripts/docs-shots/capture.mjs [name ...]
//
// With names (e.g. web-review-rail), only those are taken. Light theme,
// device scale factor 2, 1280x900 (390x844 for *-phone), UTC. Crops are
// the section's own element plus 12 px of page around it.
import { chromium } from 'playwright-core';
import { mkdirSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';

const API = process.env.DEMO_API_URL ?? 'http://localhost:18480';
const WEB = process.env.DEMO_WEB_URL ?? 'http://localhost:18481';
const ROOT = join(dirname(fileURLToPath(import.meta.url)), '..', '..');
const OUT = process.env.SHOTS_DIR ?? join(ROOT, 'site', 'src', 'assets', 'shots');
const REDIRECT = 'https://program.example/logbook/callback';
const PAD = 12;
const DESKTOP = { width: 1280, height: 900 };
const PHONE = { width: 390, height: 844 };
// A Mac browser, so the install command defaults to macOS · Linux.
const UA =
	'Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/146.0.0.0 Safari/537.36';

const only = new Set(process.argv.slice(2));
const want = (name) => only.size === 0 || only.has(name);
mkdirSync(OUT, { recursive: true });

const browser = await chromium.launch();
const made = [];

async function context(viewport = DESKTOP) {
	return browser.newContext({
		viewport,
		deviceScaleFactor: 2,
		colorScheme: 'light',
		timezoneId: 'UTC',
		locale: 'en-GB',
		userAgent: UA,
		reducedMotion: 'reduce'
	});
}

/** Signs ctx in as username through dev sign-in and sets the display name back. */
async function signIn(ctx, username, displayName) {
	const res = await ctx.request.post(`${API}/auth/dev-login`, { data: { username } });
	if (!res.ok()) throw new Error(`dev-login ${username}: ${res.status()} ${await res.text()}`);
	const { csrf_token: csrf } = await res.json();
	// Dev sign-in sets the display name to the username, as Hack Club's
	// would set the real one; put the demo's back.
	const p = await ctx.request.patch(`${API}/api/v1/me/profile`, {
		data: { display_name: displayName },
		headers: { 'X-CSRF-Token': csrf, Origin: WEB }
	});
	if (!p.ok()) throw new Error(`profile ${username}: ${p.status()} ${await p.text()}`);
	return csrf;
}

async function apiGet(ctx, path) {
	const res = await ctx.request.get(`${API}${path}`, { headers: { Origin: WEB } });
	if (!res.ok()) throw new Error(`GET ${path}: ${res.status()} ${await res.text()}`);
	return res.json();
}

/** Opens path and waits until the page has settled: data loaded, fonts in. */
async function open(page, path, ready) {
	await page.goto(`${WEB}${path}`);
	await page.waitForLoadState('networkidle');
	if (ready) await ready.filter({ visible: true }).first().waitFor();
	await settle(page);
}

async function settle(page) {
	await page.evaluate(() => document.fonts.ready);
	await page.waitForLoadState('networkidle');
	await page.waitForTimeout(300);
	const body = await page.locator('body').innerText();
	if (/Loading|Something went wrong|Couldn’t load|Too many requests/i.test(body)) {
		throw new Error(`${page.url()} shows a loading or error state:\n${body.slice(0, 400)}`);
	}
}

/** The viewport, or the whole page when full. */
async function shot(page, name, { full = false } = {}) {
	await page.evaluate(() => window.scrollTo(0, 0));
	await page.screenshot({ path: join(OUT, `${name}.png`), fullPage: full });
	made.push(name);
}

/** The union of locators' boxes, with PAD of page around it. */
async function crop(page, name, ...locators) {
	let box = null;
	for (const all of locators) {
		// Some sections are in the page twice, one copy hidden at this width.
		const l = all.filter({ visible: true }).first();
		await l.scrollIntoViewIfNeeded();
		const b = await l.evaluate((el) => {
			const r = el.getBoundingClientRect();
			return { x: r.left + window.scrollX, y: r.top + window.scrollY, w: r.width, h: r.height };
		});
		box = box
			? {
					x: Math.min(box.x, b.x),
					y: Math.min(box.y, b.y),
					r: Math.max(box.r, b.x + b.w),
					bt: Math.max(box.bt, b.y + b.h)
				}
			: { x: b.x, y: b.y, r: b.x + b.w, bt: b.y + b.h };
	}
	const width = await page.evaluate(() => document.documentElement.scrollWidth);
	const x = Math.max(0, box.x - PAD);
	const y = Math.max(0, box.y - PAD);
	const clip = { x, y, width: Math.min(width, box.r + PAD) - x, height: box.bt + PAD - y };
	await page.evaluate(() => window.scrollTo(0, 0));
	await page.screenshot({ path: join(OUT, `${name}.png`), fullPage: true, clip });
	made.push(name);
}

/** The panel whose head reads title. */
const panel = (root, title) => {
	const page = typeof root.page === 'function' ? root.page() : root;
	return root
		.locator('section.panel')
		.filter({ has: page.locator('.ph .t', { hasText: new RegExp(`^${title}$`) }) });
};

// The agent's own status page: only from agent-status.sh, which runs the
// agent and sets AGENT_STATUS_URL.
if (process.env.AGENT_STATUS_URL) {
	const ctx = await context();
	const page = await ctx.newPage();
	await page.goto(process.env.AGENT_STATUS_URL);
	await page.waitForLoadState('networkidle');
	await shot(page, 'agent-status-page', { full: true });
	await ctx.close();
	await browser.close();
	console.log(`made ${made.join(', ')}`);
	process.exit(0);
}

// Review links, from Orpheus's own list.
const setup = await context();
await signIn(setup, 'orpheus', 'Orpheus');
const { submissions: subs } = await apiGet(setup, '/api/v1/me/submissions');
const tokenOf = (project) => {
	const s = subs.find((x) => x.project === project && x.review_url);
	if (!s) throw new Error(`no submission of ${project}; run the demo seed first`);
	return new URL(s.review_url).pathname.split('/').pop();
};
const garden = tokenOf('pixel-garden');
const tinyfs = tokenOf('tinyfs');
await setup.close();

// Signed out.
if (want('web-signin')) {
	const ctx = await context();
	const page = await ctx.newPage();
	await open(page, '/', page.getByText('Sign in with Hack Club').first());
	await shot(page, 'web-signin');
	await ctx.close();
}

// Nova: signed in, no device yet.
if (want('web-home-setup') || want('web-install-unix') || want('web-install-windows')) {
	const ctx = await context();
	await signIn(ctx, 'nova', 'Nova');
	const page = await ctx.newPage();
	await open(page, '/', page.getByRole('button', { name: 'Get my install command' }));
	if (want('web-home-setup')) await shot(page, 'web-home-setup');
	const step = page.locator('li.step').first();
	if (want('web-install-unix') || want('web-install-windows')) {
		await page.getByRole('button', { name: 'Get my install command' }).click();
		await step.getByText('Paste it into a terminal.').waitFor();
		await settle(page);
		if (want('web-install-unix')) await crop(page, 'web-install-unix', step);
		if (want('web-install-windows')) {
			await step.getByText('Windows', { exact: true }).click();
			await step.getByText('Paste it into PowerShell.').waitFor();
			await crop(page, 'web-install-windows', step);
		}
	}
	await ctx.close();
}

// Orpheus: ten days of work, a device, three submissions, a grant.
{
	const ctx = await context();
	await signIn(ctx, 'orpheus', 'Orpheus');
	const page = await ctx.newPage();
	if (want('web-home-today')) {
		await open(page, '/', panel(page, 'Today'));
		await shot(page, 'web-home-today', { full: true });
	}
	if (want('web-projects')) {
		await open(page, '/projects', panel(page, 'Projects'));
		await shot(page, 'web-projects', { full: true });
	}
	if (want('web-settings') || want('web-settings-hackatime') || want('web-settings-apps')) {
		await open(page, '/settings', panel(page, 'Computers'));
		// Key prefixes ("20f260e7…"): show only the first 4 characters.
		await page.evaluate(() => {
			for (const el of document.querySelectorAll('.mono')) {
				const m = /^([0-9a-z]{4})[0-9a-z]+…$/i.exec(el.textContent.trim());
				if (m) el.textContent = `${m[1]}…`;
			}
		});
		if (want('web-settings')) {
			await crop(page, 'web-settings', page.locator('header').first(), panel(page, 'Profile'), panel(page, 'Computers'));
		}
		if (want('web-settings-hackatime')) await crop(page, 'web-settings-hackatime', panel(page, 'Hackatime'));
		if (want('web-settings-apps')) await crop(page, 'web-settings-apps', panel(page, 'Apps with access'));
	}
	if (want('web-submissions')) {
		await open(page, '/submissions', panel(page, 'Submissions'));
		await shot(page, 'web-submissions', { full: true });
	}
	await ctx.close();

	if (want('web-home-phone')) {
		const phone = await context(PHONE);
		await signIn(phone, 'orpheus', 'Orpheus');
		const p = await phone.newPage();
		await open(p, '/', panel(p, 'Today'));
		await shot(p, 'web-home-phone');
		await phone.close();
	}
}

// The review page, signed out (anyone with the link).
if (['web-review', 'web-review-hours', 'web-review-cuts', 'web-review-days'].some(want)) {
	const ctx = await context();
	const page = await ctx.newPage();
	await open(page, `/r/${garden}`, panel(page, 'Hours'));
	if (want('web-review')) await shot(page, 'web-review', { full: true });
	if (want('web-review-hours')) await crop(page, 'web-review-hours', panel(page, 'Hours'));
	if (want('web-review-cuts')) await crop(page, 'web-review-cuts', panel(page, 'Cuts and flags'));
	if (want('web-review-days')) await crop(page, 'web-review-days', panel(page, 'Days'));
	await ctx.close();
}
if (want('web-review-phone')) {
	const ctx = await context(PHONE);
	const page = await ctx.newPage();
	await open(page, `/r/${garden}`, panel(page, 'Hours'));
	await shot(page, 'web-review-phone');
	await ctx.close();
}

// Alex, a reviewer of Example Program.
if (['web-review-rail', 'web-review-adjust', 'web-review-decided', 'web-inbox'].some(want)) {
	const ctx = await context();
	await signIn(ctx, 'reviewer-alex', 'Alex');
	const page = await ctx.newPage();
	if (want('web-review-rail') || want('web-review-adjust')) {
		await open(page, `/r/${garden}`, page.getByRole('button', { name: 'Approve' }));
		// The rail stretches to the page's height; crop to its panels.
		const rail = page.locator('.rail');
		if (want('web-review-rail')) {
			await crop(page, 'web-review-rail', panel(rail, 'Trust'), panel(rail, 'Decision'));
		}
		if (want('web-review-adjust')) {
			await page.getByRole('button', { name: 'Adjust' }).filter({ visible: true }).first().click();
			await page.getByText('Approve fewer hours?').filter({ visible: true }).first().waitFor();
			const form = panel(page, 'Decision').filter({ visible: true }).first();
			await form.getByLabel('Hours to approve').fill('3.5');
			await form.getByLabel('Reason').fill('The last half hour of 24 Nov was a tutorial, not the project.');
			await page.waitForTimeout(300);
			await crop(page, 'web-review-adjust', panel(page, 'Decision'));
		}
	}
	if (want('web-review-decided')) {
		await open(page, `/r/${tinyfs}`, panel(page, 'Decision'));
		await crop(page, 'web-review-decided', panel(page, 'Decision'));
	}
	if (want('web-inbox')) {
		const apps = await apiGet(ctx, '/api/v1/me/apps');
		const app = apps.apps?.[0] ?? apps[0];
		await open(page, `/apps/${app.id}`, panel(page, 'Inbox'));
		await crop(page, 'web-inbox', panel(page, 'Inbox'));
	}
	await ctx.close();
}

// Sam, who owns Example Program.
if (['web-apps', 'web-apps-new', 'web-app-detail'].some(want)) {
	const ctx = await context();
	await signIn(ctx, 'reviewer-sam', 'Sam');
	const page = await ctx.newPage();
	if (want('web-apps')) {
		await open(page, '/apps', panel(page, 'Apps'));
		await shot(page, 'web-apps', { full: true });
	}
	if (want('web-apps-new')) {
		await open(page, '/apps/new', panel(page, 'App'));
		await shot(page, 'web-apps-new', { full: true });
	}
	if (want('web-app-detail')) {
		const apps = await apiGet(ctx, '/api/v1/me/apps');
		const app = apps.apps?.[0] ?? apps[0];
		await open(page, `/apps/${app.id}`, panel(page, 'Credentials'));
		await shot(page, 'web-app-detail', { full: true });
	}
	await ctx.close();
}

// Consent: Nova opens Example Program's real /oauth/authorize link.
if (want('web-consent')) {
	const owner = await context();
	await signIn(owner, 'reviewer-sam', 'Sam');
	const apps = await apiGet(owner, '/api/v1/me/apps');
	const app = apps.apps?.[0] ?? apps[0];
	await owner.close();
	const ctx = await context();
	await signIn(ctx, 'nova', 'Nova');
	const page = await ctx.newPage();
	const q = new URLSearchParams({
		client_id: app.client_id,
		redirect_uri: REDIRECT,
		response_type: 'code',
		scope: 'profile read',
		state: 'demo'
	});
	await page.goto(`${API}/oauth/authorize?${q}`);
	await page.waitForURL(`${WEB}/oauth/authorize**`);
	await page.getByRole('button', { name: /Allow/ }).waitFor();
	await settle(page);
	await shot(page, 'web-consent', { full: true });
	await ctx.close();
}

await browser.close();
console.log(`made ${made.length}: ${made.join(', ')}`);

import { describe, expect, it } from 'vitest';
import { ApiError } from './api.ts';
import {
	appError,
	checkAppForm,
	consentProblem,
	emptyForm,
	formOf,
	hasErrors,
	inboxEmpty,
	inboxQuery,
	maskSecret,
	pendingLabel,
	redirectList,
	safeRedirect,
	sentence,
	shortUrl,
	slugify,
	toCreate,
	toPatch,
	urlHost,
	webUrlProblem,
	type AppForm
} from './apps.ts';
import { holdSecret, takeSecret } from './secret.ts';
import type { App } from './types.ts';

const app: App = {
	id: 'a1',
	client_id: 'a1',
	name: 'Stardance',
	slug: 'stardance',
	redirect_uris: ['https://stardance.example/cb'],
	homepage_url: 'https://stardance.example',
	public: true,
	confidential: true,
	role: 'owner',
	owner_username: 'fiona',
	created_at: '2026-11-25T12:00:00Z',
	updated_at: '2026-11-25T12:00:00Z',
	secret_rotated_at: '2026-11-25T12:00:00Z'
};

function form(over: Partial<AppForm> = {}): AppForm {
	return {
		...emptyForm(),
		name: 'Stardance',
		slug: 'stardance',
		redirects: ['https://stardance.example/cb'],
		...over
	};
}

describe('slugify', () => {
	it.each([
		['Stardance', 'stardance'],
		['Save Point!', 'save-point'],
		['  Café  Crème ', 'cafe-creme'],
		['--a--b--', 'a-b'],
		['日本', ''],
		['x'.repeat(39) + ' yz', 'x'.repeat(39)]
	])('%s -> %s', (name, want) => {
		expect(slugify(name)).toBe(want);
	});
});

describe('webUrlProblem', () => {
	it.each([
		'https://stardance.example/auth/hackatime/callback',
		'https://stardance.example:8443/cb?x=1',
		'http://localhost:3000/cb',
		'http://127.0.0.1/cb'
	])('accepts %s', (u) => {
		expect(webUrlProblem(u)).toBeNull();
	});

	it.each([
		['stardance.example/cb', 'starting with https://'],
		['http://stardance.example/cb', 'localhost'],
		['ftp://stardance.example', 'starting with https://'],
		['https://me:pw@stardance.example', 'user name'],
		['https://stardance.example/cb#x', '# part'],
		['https://stardance.example/a b', 'spaces'],
		['https://' + 'a'.repeat(2050), 'too long']
	])('refuses %s', (u, says) => {
		expect(webUrlProblem(u)).toContain(says);
	});
});

describe('checkAppForm', () => {
	it('passes a good form', () => {
		expect(hasErrors(checkAppForm(form(), 'create'))).toBe(false);
	});

	it('names each field to fix', () => {
		const e = checkAppForm(
			form({
				name: ' ',
				slug: 'Star Dance',
				homepage: 'stardance.example',
				redirects: ['https://ok.example/cb', 'http://evil.example/cb']
			}),
			'create'
		);
		expect(e.name).toBeTruthy();
		expect(e.slug).toBeTruthy();
		expect(e.homepage).toBeTruthy();
		expect(e.redirects[0]).toBeNull();
		expect(e.redirects[1]).toContain('https://');
		expect(hasErrors(e)).toBe(true);
	});

	it('wants at least one redirect, ignoring blank fields', () => {
		const e = checkAppForm(form({ redirects: ['', '  '] }), 'create');
		expect(e.redirectList).toBeTruthy();
		expect(e.redirects).toEqual([null, null]);
	});

	it('refuses names that are too long or carry control characters', () => {
		expect(checkAppForm(form({ name: 'x'.repeat(65) }), 'edit').name).toContain('64');
		expect(checkAppForm(form({ name: 'a‮b' }), 'edit').name).toBeTruthy();
		// 64 characters of an astral script count as 64, as the server counts them.
		expect(checkAppForm(form({ name: '😀'.repeat(64) }), 'edit').name).toBeUndefined();
	});

	it('skips the slug when editing', () => {
		expect(checkAppForm(form({ slug: 'NOPE' }), 'edit').slug).toBeUndefined();
	});
});

describe('toCreate and redirectList', () => {
	it('trims, drops blanks and repeats, and leaves out an empty homepage', () => {
		const f = form({
			name: ' Stardance ',
			redirects: [' https://a.example/cb ', '', 'https://a.example/cb', 'https://b.example/cb'],
			listed: false,
			confidential: false
		});
		expect(redirectList(f)).toEqual(['https://a.example/cb', 'https://b.example/cb']);
		expect(toCreate(f)).toEqual({
			name: 'Stardance',
			slug: 'stardance',
			redirect_uris: ['https://a.example/cb', 'https://b.example/cb'],
			public: false,
			confidential: false
		});
		expect(toCreate(form({ homepage: 'https://s.example' })).homepage_url).toBe(
			'https://s.example'
		);
	});
});

describe('toPatch', () => {
	it('is null when nothing changed', () => {
		expect(toPatch(app, formOf(app))).toBeNull();
		expect(toPatch(app, { ...formOf(app), redirects: [...app.redirect_uris, ''] })).toBeNull();
	});

	it('sends only what changed', () => {
		const f = formOf(app);
		f.name = 'Stardance 2';
		f.listed = false;
		f.redirects.push('https://stardance.example/cb2');
		expect(toPatch(app, f)).toEqual({
			name: 'Stardance 2',
			public: false,
			redirect_uris: ['https://stardance.example/cb', 'https://stardance.example/cb2']
		});
	});

	it('can clear the homepage', () => {
		expect(toPatch(app, { ...formOf(app), homepage: ' ' })).toEqual({ homepage_url: '' });
	});
});

describe('safeRedirect', () => {
	const cb = 'https://stardance.example/auth/callback';
	it('allows the request’s own redirect URI with a code or error', () => {
		expect(safeRedirect(`${cb}?code=abc&state=x`, cb)).toBe(`${cb}?code=abc&state=x`);
		expect(safeRedirect(`${cb}?error=access_denied`, cb)).toBe(`${cb}?error=access_denied`);
		expect(safeRedirect('http://localhost:3000/cb?code=1', 'http://localhost:3000/cb')).toBe(
			'http://localhost:3000/cb?code=1'
		);
	});

	it.each([
		['javascript:alert(1)', cb],
		['data:text/html,hi', cb],
		['https://evil.example/auth/callback?code=1', cb],
		['https://stardance.example/other?code=1', cb],
		['not a url', cb],
		[`${cb}?code=1`, 'not a url']
	])('refuses %s', (to, want) => {
		expect(safeRedirect(to, want)).toBeNull();
	});
});

describe('consentProblem', () => {
	it('knows the two link errors only', () => {
		expect(consentProblem('invalid_client')).toBe('invalid_client');
		expect(consentProblem('invalid_redirect_uri')).toBe('invalid_redirect_uri');
		expect(consentProblem('access_denied')).toBeNull();
		expect(consentProblem(null)).toBeNull();
	});
});

describe('inboxQuery', () => {
	it.each([
		['review', { limit: 100, status: 'ready', decision: 'none' }],
		['verifying', { limit: 100, status: 'verifying' }],
		['approved', { limit: 100, decision: 'approved' }],
		['rejected', { limit: 100, decision: 'rejected' }],
		['all', { limit: 100 }]
	] as const)('%s', (f, want) => {
		expect(inboxQuery(f)).toEqual(want);
	});

	it('pages with an offset', () => {
		expect(inboxQuery('all', 100)).toEqual({ limit: 100, offset: 100 });
	});
});

describe('labels', () => {
	it('counts what waits for review', () => {
		expect(pendingLabel(0)).toBe('nothing to review');
		expect(pendingLabel(3)).toBe('3 to review');
		expect(pendingLabel(100)).toBe('100+ to review');
	});

	it('has an empty line per filter', () => {
		expect(inboxEmpty('review')).toBe('( NOTHING TO REVIEW )');
		expect(inboxEmpty('adjusted')).toBe('( NONE ADJUSTED )');
	});

	it('masks a secret to its prefix', () => {
		expect(maskSecret('lbs_abcdef')).toBe(`lbs_${'•'.repeat(24)}`);
		expect(maskSecret('abcdef')).toBe('•'.repeat(24));
	});

	it('shortens addresses', () => {
		expect(urlHost('https://stardance.example:8443/cb')).toBe('stardance.example:8443');
		expect(urlHost('nope')).toBe('nope');
		expect(shortUrl('https://stardance.example/')).toBe('stardance.example');
	});
});

describe('appError', () => {
	it('turns known API messages into sentences', () => {
		expect(appError(new ApiError(409, 'slug is taken'))).toContain('Slugs stay taken');
		expect(appError(new ApiError(404, 'user not found'), { username: 'orpheus' })).toBe(
			'There’s no Logbook user called orpheus.'
		);
		expect(appError(new ApiError(409, 'already a member'), { username: 'orpheus' })).toBe(
			'orpheus is already a member.'
		);
		expect(appError(new ApiError(409, 'you already have 10 apps; delete one first'))).toBe(
			'You already have 10 apps; delete one first.'
		);
	});

	it('keeps the client’s own words for limits and outages', () => {
		expect(appError(new ApiError(429, 'too many requests', 30))).toContain('30 s');
		expect(appError(new ApiError(500, 'boom'))).toContain('our side');
		expect(appError(new Error('x'))).toBe('That didn’t work. Try again.');
	});

	it('writes sentences', () => {
		expect(sentence('not found')).toBe('Not found.');
		expect(sentence('Done!')).toBe('Done!');
		expect(sentence('')).toBe('');
	});
});

describe('one-time secret hand-off', () => {
	it('gives the secret once, to its own app only', () => {
		holdSecret('a1', 'lbs_x');
		expect(takeSecret('a2')).toBeNull();
		expect(takeSecret('a1')).toBe('lbs_x');
		expect(takeSecret('a1')).toBeNull();
	});
});

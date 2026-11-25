// Pure helpers for the apps pages and the consent page (docs/api/programs.md,
// docs/web.md "Apps" and "Consent"): the register/edit form's checks, what a
// change sends, the inbox filters, and where the browser may be sent after
// consent. No I/O, so it is unit-tested; the server checks everything again.

import { ApiError } from './api.ts';
import type { App, AppCreate, AppPatch, InboxQuery } from './types.ts';

/** Loads of an app's pages depend on this, so a save can refresh them. */
export const APPS = 'logbook:apps';

/** The API's limits (internal/server/oauth/validate.go). */
export const MAX_NAME = 64;
export const MAX_REDIRECTS = 5;
const MAX_URL = 2048;
const SLUG = /^[a-z0-9-]{2,40}$/;

/** What the register and edit forms hold, as typed. */
export interface AppForm {
	name: string;
	slug: string;
	homepage: string;
	/** One field per redirect URI; blank fields are dropped. */
	redirects: string[];
	/** Listed as a program people can submit hours to (the API's `public`). */
	listed: boolean;
	/** Has a client secret (the API's `confidential`). Fixed after create. */
	confidential: boolean;
}

/** What to fix, per field. Redirect errors line up with the form's fields. */
export interface AppFormErrors {
	name?: string;
	slug?: string;
	homepage?: string;
	redirects: (string | null)[];
	/** About the list as a whole (none given). */
	redirectList?: string;
}

export function emptyForm(): AppForm {
	return {
		name: '',
		slug: '',
		homepage: '',
		redirects: [''],
		listed: true,
		confidential: true
	};
}

/** The edit form's starting values for an app. */
export function formOf(a: App): AppForm {
	return {
		name: a.name,
		slug: a.slug,
		homepage: a.homepage_url,
		redirects: a.redirect_uris.length ? [...a.redirect_uris] : [''],
		listed: a.public,
		confidential: a.confidential
	};
}

/** A slug suggested from the name: "Save Point!" → "save-point". */
export function slugify(name: string): string {
	return name
		.normalize('NFKD')
		.replace(/[̀-ͯ]/g, '')
		.toLowerCase()
		.replace(/[^a-z0-9]+/g, '-')
		.replace(/^-+|-+$/g, '')
		.slice(0, 40)
		.replace(/-+$/, '');
}

/**
 * null when `raw` is an address the API accepts for a redirect URI or
 * homepage: absolute https, or http on localhost or 127.0.0.1 (any port),
 * with no user info, fragment, spaces or backslashes. Else what to fix.
 */
export function webUrlProblem(raw: string): string | null {
	if (raw.length > MAX_URL) return 'That address is too long.';
	if (/[\s\\]/.test(raw)) return 'An address can’t contain spaces or backslashes.';
	let u: URL;
	try {
		u = new URL(raw);
	} catch {
		return 'Use a full address, starting with https://.';
	}
	if (u.protocol === 'http:') {
		if (u.hostname !== 'localhost' && u.hostname !== '127.0.0.1') {
			return 'Use https://. Plain http:// works only for localhost and 127.0.0.1.';
		}
	} else if (u.protocol !== 'https:') {
		return 'Use a full address, starting with https://.';
	}
	if (!u.host) return 'Use a full address, starting with https://.';
	if (u.username || u.password) return 'Leave out the user name and password.';
	if (raw.includes('#')) return 'Leave out the # part.';
	return null;
}

/** Characters as the server counts them (code points). */
function length(s: string): number {
	return [...s].length;
}

/** Control and bidirectional-override characters, which the API refuses in names. */
// eslint-disable-next-line no-control-regex -- matching control characters is the point
const CONTROL = /[\u0000-\u001f\u007f-\u009f؜‎‏‪-‮⁦-⁩�]/;

/** Checks the form. `create` also checks the slug, which can't change later. */
export function checkAppForm(f: AppForm, mode: 'create' | 'edit'): AppFormErrors {
	const errors: AppFormErrors = { redirects: f.redirects.map(() => null) };
	const name = f.name.trim();
	if (!name) errors.name = 'Give the app a name.';
	else if (length(name) > MAX_NAME) errors.name = `Keep it to ${MAX_NAME} characters.`;
	else if (CONTROL.test(name)) errors.name = 'Use letters, digits and punctuation only.';

	if (mode === 'create' && !SLUG.test(f.slug.trim())) {
		errors.slug = 'Use 2 to 40 lowercase letters, digits and dashes.';
	}

	const home = f.homepage.trim();
	if (home) {
		const p = webUrlProblem(home);
		if (p) errors.homepage = p;
	}

	let given = 0;
	f.redirects.forEach((raw, i) => {
		const r = raw.trim();
		if (!r) return;
		given++;
		errors.redirects[i] = webUrlProblem(r);
	});
	if (given === 0) errors.redirectList = 'Add the address Logbook sends people back to.';
	return errors;
}

export function hasErrors(e: AppFormErrors): boolean {
	return !!(e.name || e.slug || e.homepage || e.redirectList || e.redirects.some((r) => r));
}

/** The redirect URIs as sent: trimmed, blanks and repeats dropped, in order. */
export function redirectList(f: AppForm): string[] {
	const out: string[] = [];
	for (const raw of f.redirects) {
		const r = raw.trim();
		if (r && !out.includes(r)) out.push(r);
	}
	return out;
}

/** The body of POST /api/v1/me/apps. */
export function toCreate(f: AppForm): AppCreate {
	const body: AppCreate = {
		name: f.name.trim(),
		slug: f.slug.trim(),
		redirect_uris: redirectList(f),
		public: f.listed,
		confidential: f.confidential
	};
	const home = f.homepage.trim();
	if (home) body.homepage_url = home;
	return body;
}

/** Only what changed, for PATCH; null when nothing did. */
export function toPatch(a: App, f: AppForm): AppPatch | null {
	const patch: AppPatch = {};
	const name = f.name.trim();
	if (name !== a.name) patch.name = name;
	const home = f.homepage.trim();
	if (home !== a.homepage_url) patch.homepage_url = home;
	const uris = redirectList(f);
	if (uris.length !== a.redirect_uris.length || uris.some((u, i) => u !== a.redirect_uris[i])) {
		patch.redirect_uris = uris;
	}
	if (f.listed !== a.public) patch.public = f.listed;
	return Object.keys(patch).length ? patch : null;
}

/** A one-time secret's stand-in until "Show": its prefix and dots. */
export function maskSecret(secret: string): string {
	const prefix = /^[a-z]+_/.exec(secret)?.[0] ?? '';
	return `${prefix}${'•'.repeat(24)}`;
}

/** "stardance.example" (with a port when there is one), or the input when it isn't a URL. */
export function urlHost(raw: string): string {
	try {
		return new URL(raw).host || raw;
	} catch {
		return raw;
	}
}

/** A homepage as people read it: no scheme, no trailing slash. */
export function shortUrl(raw: string): string {
	return raw.replace(/^https?:\/\//, '').replace(/\/$/, '');
}

/**
 * Where to send the browser after consent, or null when it isn't safe to:
 * only http(s), and only to the request's own redirect URI (same origin and
 * path, with the code or error the server added in the query).
 */
export function safeRedirect(target: string, expected: string): string | null {
	let to: URL;
	let want: URL;
	try {
		to = new URL(target);
		want = new URL(expected);
	} catch {
		return null;
	}
	if (to.protocol !== 'https:' && to.protocol !== 'http:') return null;
	if (to.origin !== want.origin || to.pathname !== want.pathname) return null;
	return to.href;
}

/** The consent page's ?error=, from GET /oauth/authorize's redirect. */
export type ConsentProblem = 'invalid_client' | 'invalid_redirect_uri';

export function consentProblem(code: string | null): ConsentProblem | null {
	return code === 'invalid_client' || code === 'invalid_redirect_uri' ? code : null;
}

// The program's inbox.

export type InboxFilter = 'review' | 'verifying' | 'approved' | 'adjusted' | 'rejected' | 'all';

export const INBOX_FILTERS: { value: InboxFilter; label: string }[] = [
	{ value: 'review', label: 'To review' },
	{ value: 'verifying', label: 'Verifying' },
	{ value: 'approved', label: 'Approved' },
	{ value: 'adjusted', label: 'Adjusted' },
	{ value: 'rejected', label: 'Rejected' },
	{ value: 'all', label: 'All' }
];

/** One page of the inbox. */
export const INBOX_PAGE = 100;

/** The API query for a filter. "To review" is ready with no decision yet. */
export function inboxQuery(filter: InboxFilter, offset = 0): InboxQuery {
	const q: InboxQuery = { limit: INBOX_PAGE };
	if (offset) q.offset = offset;
	if (filter === 'review') return { ...q, status: 'ready', decision: 'none' };
	if (filter === 'verifying') return { ...q, status: 'verifying' };
	if (filter === 'all') return q;
	return { ...q, decision: filter };
}

/** "3 to review", "100+ to review", "nothing to review", from one page of the "To review" filter. */
export function pendingLabel(count: number, limit = INBOX_PAGE): string {
	if (count === 0) return 'nothing to review';
	return `${count >= limit ? `${limit}+` : count} to review`;
}

/** The empty line for a filter, in pixel caps. */
export function inboxEmpty(filter: InboxFilter): string {
	switch (filter) {
		case 'review':
			return '( NOTHING TO REVIEW )';
		case 'verifying':
			return '( NOTHING VERIFYING )';
		case 'all':
			return '( NO SUBMISSIONS YET )';
		default:
			return `( NONE ${filter.toUpperCase()} )`;
	}
}

/** A server message as a sentence: capitalised, with a full stop. */
export function sentence(msg: string): string {
	const t = msg.trim();
	if (!t) return t;
	const s = t[0].toUpperCase() + t.slice(1);
	return /[.!?]$/.test(s) ? s : `${s}.`;
}

/**
 * What a failed apps call says, in the page's words. The API's messages are
 * short and lowercase ("slug is taken"); the known ones get a full sentence.
 */
export function appError(err: unknown, ctx: { username?: string } = {}): string {
	if (!(err instanceof ApiError)) return 'That didn’t work. Try again.';
	if (err.status === 429 || err.status === 0 || err.status >= 500) return err.userMessage;
	switch (err.message) {
		case 'slug is taken':
			return 'That slug is taken. Slugs stay taken even after an app is deleted.';
		case 'user not found':
			return ctx.username
				? `There’s no Logbook user called ${ctx.username}.`
				: 'There’s no Logbook user by that name.';
		case 'already a member':
			return ctx.username ? `${ctx.username} is already a member.` : 'They’re already a member.';
		case 'public clients have no secret':
			return 'A public client has no secret to rotate.';
		case "only the app's owner can do this":
			return 'Only the app’s owner can do this.';
	}
	return sentence(err.message);
}

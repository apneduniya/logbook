// The browser's client for the Logbook API (docs/web.md, "Talking to the API").
// Every call carries the session cookie; state-changing calls also carry the
// CSRF token, which is kept in memory only.

import type {
	ApiKey,
	App,
	AppCreate,
	AppMember,
	AppPatch,
	AppWithSecret,
	ConsentRequest,
	Decision,
	DecisionInput,
	Device,
	Grant,
	HackatimeLink,
	InboxQuery,
	NewApiKey,
	Profile,
	ProfilePatch,
	Project,
	ProjectPatch,
	PublicProfile,
	PublicProgram,
	Review,
	Session,
	SubmissionDetail,
	SubmissionSummary,
	Summary,
	Today
} from './types.ts';

/** An error response from the API, with its status and the server's message. */
export class ApiError extends Error {
	readonly status: number;
	/** Seconds from Retry-After on a 429, else null. */
	readonly retryAfter: number | null;

	constructor(status: number, message: string, retryAfter: number | null = null) {
		super(message);
		this.name = 'ApiError';
		this.status = status;
		this.retryAfter = retryAfter;
	}

	/** A sentence fit to show in the panel that made the call. */
	get userMessage(): string {
		if (this.status === 429) {
			return this.retryAfter
				? `Too many requests. Try again in ${this.retryAfter} s.`
				: 'Too many requests. Try again in a minute.';
		}
		if (this.status === 0) return 'Can’t reach Logbook. Check your connection and try again.';
		if (this.status >= 500) return 'Something went wrong on our side. Try again in a minute.';
		return this.message;
	}
}

type Method = 'GET' | 'POST' | 'PATCH' | 'DELETE';

/** Options for a range summary. Dates are YYYY-MM-DD in the user's time zone. */
export interface SummaryQuery {
	start?: string;
	end?: string;
	project?: string;
}

export class Api {
	readonly origin: string;
	#csrf = '';
	#onUnauthorized: () => void;

	/**
	 * @param origin the API origin (LOGBOOK_API_URL)
	 * @param onUnauthorized called when a signed-in call gets 401 (the session ended)
	 */
	constructor(origin: string, onUnauthorized: () => void) {
		this.origin = origin;
		this.#onUnauthorized = onUnauthorized;
	}

	/** The URL of an API path, for full-page navigations (sign-in, Hackatime link). */
	url(path: string, query?: Record<string, string>): string {
		const u = new URL(path, this.origin);
		for (const [k, v] of Object.entries(query ?? {})) u.searchParams.set(k, v);
		return u.href;
	}

	/**
	 * @param keepSession on a 401, don't end the app's session: the caller
	 *   handles it in place (the review page keeps the reviewer's reason).
	 */
	async #call<T>(
		method: Method,
		path: string,
		body?: unknown,
		signal?: AbortSignal,
		keepSession = false
	): Promise<T> {
		const headers: Record<string, string> = { Accept: 'application/json' };
		if (method !== 'GET') headers['X-CSRF-Token'] = this.#csrf;
		if (body !== undefined) headers['Content-Type'] = 'application/json';

		let res: Response;
		try {
			res = await fetch(this.url(path), {
				method,
				headers,
				body: body === undefined ? undefined : JSON.stringify(body),
				credentials: 'include',
				cache: 'no-store',
				signal
			});
		} catch (err) {
			if (signal?.aborted) throw err;
			throw new ApiError(0, 'network error');
		}

		if (res.status === 204) return undefined as T;
		const data: unknown = await res.json().catch(() => null);
		if (res.ok) return data as T;

		const message =
			data && typeof data === 'object' && 'error' in data && typeof data.error === 'string'
				? data.error
				: `request failed (${res.status})`;
		const retry = Number(res.headers.get('Retry-After'));
		const err = new ApiError(
			res.status,
			message,
			Number.isFinite(retry) && retry > 0 ? retry : null
		);
		// A 401 means the session ended, except on the check that asks whether
		// there is one (GET /api/v1/session answers 401 when signed out).
		if (res.status === 401 && !keepSession && !(method === 'GET' && path === '/api/v1/session')) {
			this.#onUnauthorized();
		}
		throw err;
	}

	// Session (docs/api/auth.md)

	/** The signed-in session, or null when signed out. */
	async session(signal?: AbortSignal): Promise<Session | null> {
		try {
			const s = await this.#call<Session>('GET', '/api/v1/session', undefined, signal);
			this.#csrf = s.csrf_token;
			return s;
		} catch (err) {
			if (err instanceof ApiError && err.status === 401) {
				this.#csrf = '';
				return null;
			}
			throw err;
		}
	}

	async logout(): Promise<void> {
		// Not POST /auth/logout: CORS covers /api/ only (docs/api/auth.md).
		try {
			await this.#call<void>('DELETE', '/api/v1/session');
		} catch (err) {
			// Already signed out (expired or revoked): that's the goal anyway.
			if (!(err instanceof ApiError && err.status === 401)) throw err;
		}
		this.#csrf = '';
	}

	// Stats (docs/api/stats.md)

	today(signal?: AbortSignal): Promise<Today> {
		return this.#call('GET', '/api/v1/me/today', undefined, signal);
	}

	summary(q: SummaryQuery, signal?: AbortSignal): Promise<Summary> {
		const p = new URLSearchParams();
		if (q.start) p.set('start', q.start);
		if (q.end) p.set('end', q.end);
		if (q.project !== undefined) p.set('project', q.project);
		const qs = p.toString();
		return this.#call('GET', `/api/v1/me/summary${qs ? `?${qs}` : ''}`, undefined, signal);
	}

	// Projects

	async projects(signal?: AbortSignal): Promise<Project[]> {
		const r = await this.#call<{ projects: Project[] }>(
			'GET',
			'/api/v1/me/projects',
			undefined,
			signal
		);
		return r.projects;
	}

	project(name: string, signal?: AbortSignal): Promise<Project> {
		return this.#call('GET', `/api/v1/me/projects/${encodeURIComponent(name)}`, undefined, signal);
	}

	updateProject(name: string, patch: ProjectPatch): Promise<Project> {
		return this.#call('PATCH', `/api/v1/me/projects/${encodeURIComponent(name)}`, patch);
	}

	// Profiles

	publicProfile(username: string, signal?: AbortSignal): Promise<PublicProfile> {
		return this.#call(
			'GET',
			`/api/v1/users/${encodeURIComponent(username)}/profile`,
			undefined,
			signal
		);
	}

	profile(signal?: AbortSignal): Promise<Profile> {
		return this.#call('GET', '/api/v1/me/profile', undefined, signal);
	}

	updateProfile(patch: ProfilePatch): Promise<Profile> {
		return this.#call('PATCH', '/api/v1/me/profile', patch);
	}

	// API keys

	async apiKeys(signal?: AbortSignal): Promise<ApiKey[]> {
		const r = await this.#call<{ api_keys: ApiKey[] }>(
			'GET',
			'/api/v1/api_keys',
			undefined,
			signal
		);
		return r.api_keys;
	}

	createApiKey(name: string): Promise<NewApiKey> {
		return this.#call('POST', '/api/v1/api_keys', { name });
	}

	revokeApiKey(id: string): Promise<void> {
		return this.#call('DELETE', `/api/v1/api_keys/${encodeURIComponent(id)}`);
	}

	// Devices (docs/api/devices.md)

	async devices(signal?: AbortSignal): Promise<Device[]> {
		const r = await this.#call<{ devices: Device[] }>(
			'GET',
			'/api/v1/me/devices',
			undefined,
			signal
		);
		return r.devices;
	}

	revokeDevice(id: string): Promise<void> {
		return this.#call('DELETE', `/api/v1/me/devices/${encodeURIComponent(id)}`);
	}

	// Hackatime (docs/api/hackatime.md)

	hackatime(signal?: AbortSignal): Promise<HackatimeLink> {
		return this.#call('GET', '/api/v1/hackatime/link', undefined, signal);
	}

	unlinkHackatime(): Promise<void> {
		return this.#call('DELETE', '/api/v1/hackatime/link');
	}

	// Review links (docs/api/submissions.md). The token is the credential:
	// it goes in the path only, never in a log line or a title.

	/**
	 * The review packet. While the re-check runs (202) it has `submission`
	 * only, with status "verifying". 404 unknown token, 410 withdrawn.
	 */
	review(token: string, signal?: AbortSignal): Promise<Review> {
		return this.#call('GET', `/api/v1/review/${encodeURIComponent(token)}`, undefined, signal);
	}

	/**
	 * Records a program member's decision. A 401 doesn't end the app's
	 * session here, so the page can keep what the reviewer typed.
	 */
	async decide(token: string, input: DecisionInput): Promise<Decision> {
		const r = await this.#call<{ decision: Decision }>(
			'POST',
			`/api/v1/review/${encodeURIComponent(token)}/decision`,
			input,
			undefined,
			true
		);
		return r.decision;
	}

	// The submitter's own submissions

	async mySubmissions(signal?: AbortSignal): Promise<SubmissionSummary[]> {
		const r = await this.#call<{ submissions: SubmissionSummary[] }>(
			'GET',
			'/api/v1/me/submissions',
			undefined,
			signal
		);
		return r.submissions;
	}

	/** @param keepSession see #call: a 401 is the caller's to handle. */
	mySubmission(id: string, signal?: AbortSignal, keepSession = false): Promise<SubmissionDetail> {
		return this.#call(
			'GET',
			`/api/v1/me/submissions/${encodeURIComponent(id)}`,
			undefined,
			signal,
			keepSession
		);
	}

	/** Withdraws before a decision (409 once decided). Withdrawing again is a no-op. */
	withdrawSubmission(id: string): Promise<void> {
		return this.#call('DELETE', `/api/v1/me/submissions/${encodeURIComponent(id)}`);
	}

	// Apps, which are also programs (docs/api/programs.md)

	/**
	 * Live apps the user owns or reviews for, newest first.
	 * @param keepSession see #call: a 401 is the caller's to handle.
	 */
	async apps(signal?: AbortSignal, keepSession = false): Promise<App[]> {
		const r = await this.#call<{ apps: App[] }>(
			'GET',
			'/api/v1/me/apps',
			undefined,
			signal,
			keepSession
		);
		return r.apps;
	}

	app(id: string, signal?: AbortSignal): Promise<App> {
		return this.#call('GET', `/api/v1/me/apps/${encodeURIComponent(id)}`, undefined, signal);
	}

	/** Registers an app. The answer carries `client_secret` once, for a confidential app. */
	createApp(input: AppCreate): Promise<AppWithSecret> {
		return this.#call('POST', '/api/v1/me/apps', input);
	}

	/** Owner only. */
	updateApp(id: string, patch: AppPatch): Promise<App> {
		return this.#call('PATCH', `/api/v1/me/apps/${encodeURIComponent(id)}`, patch);
	}

	/** Owner only. Revokes every token, code, pending request and grant of the app. */
	deleteApp(id: string): Promise<void> {
		return this.#call('DELETE', `/api/v1/me/apps/${encodeURIComponent(id)}`);
	}

	/** Owner only. The new `client_secret` is in the answer, once; the old one stops working. */
	rotateAppSecret(id: string): Promise<AppWithSecret> {
		return this.#call('POST', `/api/v1/me/apps/${encodeURIComponent(id)}/secret`);
	}

	/** Owner first, then reviewers. */
	async appMembers(id: string, signal?: AbortSignal): Promise<AppMember[]> {
		const r = await this.#call<{ members: AppMember[] }>(
			'GET',
			`/api/v1/me/apps/${encodeURIComponent(id)}/members`,
			undefined,
			signal
		);
		return r.members;
	}

	/** Owner only. Adds a reviewer by username (case-insensitive). */
	addAppMember(id: string, username: string): Promise<AppMember> {
		return this.#call('POST', `/api/v1/me/apps/${encodeURIComponent(id)}/members`, { username });
	}

	/** Owner only. The owner can't be removed. */
	removeAppMember(id: string, username: string): Promise<void> {
		return this.#call(
			'DELETE',
			`/api/v1/me/apps/${encodeURIComponent(id)}/members/${encodeURIComponent(username)}`
		);
	}

	/** The program's inbox, newest first. Members only. */
	async appSubmissions(
		id: string,
		q: InboxQuery = {},
		signal?: AbortSignal
	): Promise<SubmissionSummary[]> {
		const p = new URLSearchParams();
		if (q.status) p.set('status', q.status);
		if (q.decision) p.set('decision', q.decision);
		if (q.limit !== undefined) p.set('limit', String(q.limit));
		if (q.offset !== undefined) p.set('offset', String(q.offset));
		const qs = p.toString();
		const r = await this.#call<{ submissions: SubmissionSummary[] }>(
			'GET',
			`/api/v1/me/apps/${encodeURIComponent(id)}/submissions${qs ? `?${qs}` : ''}`,
			undefined,
			signal
		);
		return r.submissions;
	}

	/** Live public programs by name, for the submit picker. No sign-in needed. */
	async programs(signal?: AbortSignal): Promise<PublicProgram[]> {
		const r = await this.#call<{ programs: PublicProgram[] }>(
			'GET',
			'/api/v1/programs',
			undefined,
			signal
		);
		return r.programs;
	}

	// OAuth consent and grants

	/** A pending consent request (404 when unknown, expired, decided or someone else's). */
	consentRequest(id: string, signal?: AbortSignal): Promise<ConsentRequest> {
		return this.#call('GET', `/api/v1/oauth/requests/${encodeURIComponent(id)}`, undefined, signal);
	}

	/** Allows or denies; resolves to where the browser goes next. Single use. */
	async decideConsent(id: string, approve: boolean): Promise<string> {
		const r = await this.#call<{ redirect: string }>(
			'POST',
			`/api/v1/oauth/requests/${encodeURIComponent(id)}`,
			{ approve }
		);
		return r.redirect;
	}

	/** Apps the user has given access, newest first. */
	async grants(signal?: AbortSignal): Promise<Grant[]> {
		const r = await this.#call<{ grants: Grant[] }>('GET', '/api/v1/me/grants', undefined, signal);
		return r.grants;
	}

	revokeGrant(appId: string): Promise<void> {
		return this.#call('DELETE', `/api/v1/me/grants/${encodeURIComponent(appId)}`);
	}

	// Live (Server-Sent Events)

	/** An EventSource for the signed-in user's minute changes. */
	liveMe(): EventSource {
		return new EventSource(this.url('/api/v1/me/live'), { withCredentials: true });
	}

	/** An EventSource for a public profile's changes. */
	livePublic(username: string): EventSource {
		return new EventSource(this.url(`/api/v1/users/${encodeURIComponent(username)}/live`), {
			withCredentials: true
		});
	}
}

// The browser's client for the Logbook API (docs/web.md, "Talking to the API").
// Every call carries the session cookie; state-changing calls also carry the
// CSRF token, which is kept in memory only.

import type {
	ApiKey,
	Device,
	HackatimeLink,
	NewApiKey,
	Profile,
	ProfilePatch,
	Project,
	ProjectPatch,
	PublicProfile,
	Session,
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

	async #call<T>(method: Method, path: string, body?: unknown, signal?: AbortSignal): Promise<T> {
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
		if (res.status === 401 && !(method === 'GET' && path === '/api/v1/session')) {
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

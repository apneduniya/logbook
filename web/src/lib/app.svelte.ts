// App-wide state: the API client, the signed-in session and the live-update
// stream. The app is client-only (ssr = false, docs/web.md), so one module
// instance per browser tab is safe; there is no server sharing it.

import { goto, invalidate } from '$app/navigation';
import { resolve } from '$app/paths';
import { LOGBOOK_API_URL } from '$app/env/public';
import { Api, ApiError } from './api.ts';
import { isLocalPath } from './paths.ts';
import type { Device, Session } from './types.ts';

/** How long to wait after a live event before refetching, so bursts merge. */
const LIVE_DEBOUNCE_MS = 5000;
/** After a failed background reload, how long until the next try. */
const RELOAD_RETRY_MS = 30_000;

/** How long to wait before reopening a live stream the server refused. */
const LIVE_RETRY_MS = 30_000;

/** Activity this recent shows as "tracking now". */
const LIVE_WINDOW_MS = 3 * 60 * 1000;

export class AppState {
	readonly api: Api;
	/** undefined while loading, null when signed out. */
	session = $state<Session | null | undefined>(undefined);
	/** Set when the session check itself failed (network, 5xx). */
	sessionError = $state<string | null>(null);
	/** The user's devices, for the agent status in the header; null until loaded. */
	devices = $state<Device[] | null>(null);
	/** Latest known activity (ms since epoch), from live events and loaded data. */
	lastActivity = $state<number | null>(null);
	/** A clock that ticks every 30 s, so "tracking now" expires on its own. */
	now = $state(Date.now());

	/** True after a background reload failed and the page shows older data; cleared by the next success. */
	stale = $state(false);
	/**
	 * True while a background reload (see reload()) is running, so the home's
	 * load can keep showing what it has if a request fails; it sets
	 * reloadFailed when it does.
	 */
	background = false;
	reloadFailed = false;

	#source: EventSource | null = null;
	#retry: ReturnType<typeof setTimeout> | null = null;
	#timer: ReturnType<typeof setTimeout> | null = null;
	#missed = false;
	#reloading: Promise<boolean> | null = null;
	#next: Promise<boolean> | null = null;

	constructor(origin: string) {
		this.api = new Api(origin, () => this.#sessionEnded());
	}

	get user() {
		return this.session?.user ?? null;
	}

	get timezone(): string {
		return this.session?.user.timezone || 'UTC';
	}

	/** What to greet the user by: the first word of their name, else their username. */
	get firstName(): string {
		const u = this.user;
		return u ? u.display_name.split(' ')[0] || u.username : '';
	}

	/** True while there has been activity in the last few minutes. */
	get live(): boolean {
		return this.lastActivity !== null && this.now - this.lastActivity < LIVE_WINDOW_MS;
	}

	/** The newest last_seen_at of the user's active devices, or null. */
	get agentLastSeen(): string | null {
		let best: string | null = null;
		for (const d of this.devices ?? []) {
			if (d.revoked_at || !d.last_seen_at) continue;
			if (!best || d.last_seen_at > best) best = d.last_seen_at;
		}
		return best;
	}

	/** Record activity at an RFC 3339 instant if it is newer than what we know. */
	sawActivity(instant: string | null | undefined): void {
		if (!instant) return;
		const t = Date.parse(instant);
		if (Number.isFinite(t) && (this.lastActivity === null || t > this.lastActivity)) {
			this.lastActivity = t;
		}
	}

	async loadDevices(): Promise<void> {
		const session = this.session;
		try {
			const devices = await this.api.devices();
			// Drop an answer that arrives after sign-out (or a new sign-in).
			if (this.session === session) this.devices = devices;
		} catch {
			// The header just shows no agent status; pages that need devices load them themselves.
		}
	}

	async loadSession(): Promise<void> {
		try {
			this.session = await this.api.session();
			this.sessionError = null;
		} catch (err) {
			this.session = null;
			this.sessionError = err instanceof ApiError ? err.userMessage : 'Can’t reach Logbook.';
		}
		if (this.session) void this.loadDevices();
		this.#syncStream();
	}

	/**
	 * Full-page navigation to Hack Club sign-in. Comes back to `next`, else to
	 * the page's own ?next= (set by pages that need a session), else here.
	 * The API checks the path again.
	 */
	signIn(next?: string): void {
		// A one-off read of the address bar, not reactive state.
		// eslint-disable-next-line svelte/prefer-svelte-reactivity
		const asked = new URLSearchParams(location.search).get('next');
		const target = next ?? (asked && isLocalPath(asked) ? asked : location.pathname);
		location.href = this.api.url('/auth/login', { next: target });
	}

	async signOut(): Promise<void> {
		await this.api.logout();
		this.#signedOut();
	}

	/**
	 * Keeps the live stream open while signed in and the tab is visible, and
	 * refreshes every load that depends on ME when minutes change. Call once
	 * from the root layout; returns the cleanup.
	 */
	startLive(): () => void {
		const onChange = () => this.#syncStream();
		document.addEventListener('visibilitychange', onChange);
		const tick = setInterval(() => (this.now = Date.now()), 30_000);
		this.#syncStream();
		return () => {
			document.removeEventListener('visibilitychange', onChange);
			clearInterval(tick);
			this.#close();
		};
	}

	#syncStream(): void {
		const want = !!this.session && document.visibilityState === 'visible';
		if (want && !this.#source && !this.#retry) {
			const src = this.api.liveMe();
			let opened = false;
			src.addEventListener('minutes', () => {
				this.lastActivity = Date.now();
				this.#refresh(LIVE_DEBOUNCE_MS);
			});
			// The server doesn't replay what changed while EventSource was
			// reconnecting (it ends streams every 30 min), so refresh after each
			// reconnect.
			src.addEventListener('ready', () => {
				if (opened || this.#missed) this.#refresh(0);
				opened = true;
				this.#missed = false;
			});
			// Closed for good: the server refused a reconnect (expired session,
			// stream limit). Recheck the session, and retry later if it is fine.
			src.addEventListener('error', () => {
				if (src.readyState === EventSource.CLOSED) void this.#streamRefused();
			});
			this.#source = src;
		} else if (!want && this.#source) {
			this.#close();
			this.#missed = !!this.session;
		}
	}

	#refresh(delay: number): void {
		if (this.#timer) return;
		this.#timer = setTimeout(() => {
			this.#timer = null;
			// A failed reload leaves the page stale (app.stale); try again later.
			void this.reload().then((ok) => {
				if (!ok && this.session) this.#refresh(RELOAD_RETRY_MS);
			});
		}, delay);
	}

	/**
	 * Reloads the signed-in pages for fresh data nobody asked for (live
	 * updates, import progress). A failure keeps the page as it is instead of
	 * showing the error page, and sets `stale`. Resolves true once fresh data
	 * is on screen.
	 *
	 * Only one runs at a time. A call while one runs gets one more reload after
	 * it (shared by every such caller), because the running one may have
	 * started before the change that asked for this.
	 */
	reload(): Promise<boolean> {
		if (!this.#reloading) {
			this.#reloading = this.#runReload().finally(() => {
				this.#reloading = null;
			});
			return this.#reloading;
		}
		this.#next ??= this.#reloading.then(() => {
			this.#next = null;
			return this.reload();
		});
		return this.#next;
	}

	async #runReload(): Promise<boolean> {
		this.background = true;
		this.reloadFailed = false;
		try {
			await invalidate(ME);
		} catch {
			this.reloadFailed = true;
		} finally {
			this.background = false;
		}
		this.stale = this.reloadFailed;
		return !this.reloadFailed;
	}

	async #streamRefused(): Promise<void> {
		this.#close();
		// Changes during the outage are lost; refresh once the stream is back.
		this.#missed = true;
		try {
			if (!(await this.api.session())) {
				this.#sessionEnded();
				return;
			}
		} catch {
			// Offline or a server error: retry the stream later all the same.
		}
		this.#retry = setTimeout(() => {
			this.#retry = null;
			this.#syncStream();
		}, LIVE_RETRY_MS);
	}

	#close(): void {
		this.#source?.close();
		this.#source = null;
	}

	#signedOut(): void {
		this.session = null;
		this.devices = null;
		this.lastActivity = null;
		this.#close();
		if (this.#timer) clearTimeout(this.#timer);
		if (this.#retry) clearTimeout(this.#retry);
		this.#timer = this.#retry = null;
		this.#missed = false;
		this.stale = false;
	}

	/** The session expired or was revoked mid-use: show the signed-out home. */
	#sessionEnded(): void {
		if (this.session === null) return;
		this.#signedOut();
		void goto(resolve('/'), { refreshAll: true });
	}
}

/** Loads of the signed-in user's data call depends(ME) so live events can refresh them. */
export const ME = 'logbook:me';

/** Public profile loads depend on this; their own live stream invalidates it. */
export const PROFILE = 'logbook:profile';

/** The review page's load depends on this; it polls while verifying and reloads after a stale decision. */
export const REVIEW = 'logbook:review';

export const app = new AppState(LOGBOOK_API_URL);

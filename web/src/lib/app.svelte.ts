// App-wide state: the API client, the signed-in session and the live-update
// stream. The app is client-only (ssr = false, docs/web.md), so one module
// instance per browser tab is safe; there is no server sharing it.

import { invalidate } from '$app/navigation';
import { LOGBOOK_API_URL } from '$app/env/public';
import { Api, ApiError } from './api.ts';
import { isLocalPath } from './paths.ts';
import type { Device, Session } from './types.ts';

/** How long to wait after a live event before refetching, so bursts merge. */
const LIVE_DEBOUNCE_MS = 5000;

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

	#source: EventSource | null = null;
	#timer: ReturnType<typeof setTimeout> | null = null;
	#missed = false;

	constructor(origin: string) {
		this.api = new Api(origin, () => this.#signedOut());
	}

	get user() {
		return this.session?.user ?? null;
	}

	get timezone(): string {
		return this.session?.user.timezone || 'UTC';
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
		try {
			this.devices = await this.api.devices();
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
		if (want && !this.#source) {
			const src = this.api.liveMe();
			src.addEventListener('minutes', () => {
				this.lastActivity = Date.now();
				this.#refresh(LIVE_DEBOUNCE_MS);
			});
			this.#source = src;
			// Events were missed while the tab was hidden: refresh once now.
			if (this.#missed) this.#refresh(0);
			this.#missed = false;
		} else if (!want && this.#source) {
			this.#close();
			this.#missed = !!this.session;
		}
	}

	#refresh(delay: number): void {
		if (this.#timer) return;
		this.#timer = setTimeout(() => {
			this.#timer = null;
			void invalidate(ME);
		}, delay);
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
	}
}

/** Loads of the signed-in user's data call depends(ME) so live events can refresh them. */
export const ME = 'logbook:me';

/** Public profile loads depend on this; their own live stream invalidates it. */
export const PROFILE = 'logbook:profile';

export const app = new AppState(LOGBOOK_API_URL);

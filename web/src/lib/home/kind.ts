// Which home a signed-in user sees (docs/web.md, "Words").

import type { HackatimeLink, Summary, Today } from '#lib/types.ts';

export type HomeKind = 'first-run' | 'history' | 'dashboard';

/**
 * `dashboard` once a device has registered (counted time exists, even if 0).
 * Before that, `history` when there is time at the keyboard in the last year
 * to show (usually imported from Hackatime), else `first-run`.
 */
export function homeKind(today: Today, year: Summary): HomeKind {
	if (today.credited_seconds !== null) return 'dashboard';
	return year.total_seconds > 0 ? 'history' : 'first-run';
}

/** True while a Hackatime import is still bringing history in. */
export function importing(link: HackatimeLink): boolean {
	const status = link.linked ? link.import?.status : undefined;
	return status === 'pending' || status === 'running';
}

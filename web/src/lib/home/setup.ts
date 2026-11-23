// The two waits on the set-up screens (first run and history). Each returns a
// stop function, so a component can return it from an $effect.

import { invalidate } from '$app/navigation';
import { app, ME } from '#lib/app.svelte.ts';
import { poll } from '#lib/poll.ts';
import type { HackatimeLink } from '#lib/types.ts';
import { importing } from './kind.ts';

/**
 * After an install command is made: hold background reloads (they could swap
 * the screen and lose the command), and every 5 s look for the agent's first
 * upload, then load the dashboard.
 */
export function waitForDevice(): () => void {
	const release = app.hold();
	const stop = poll(5000, async (alive) => {
		const devices = await app.api.devices();
		if (!alive() || devices.length === 0) return false;
		app.devices = devices;
		release();
		await invalidate(ME);
		return true;
	});
	return () => {
		stop();
		release();
	};
}

const recentDone = (l: HackatimeLink) => l.linked && !!l.import?.recent_done;

/**
 * While a Hackatime import runs: every 15 s, fetch only the link's status and
 * hand it to `onLink`, so the progress shown stays current. A failed check is
 * retried. The home reloads (in the background, see app.reload) only when it
 * has something new to show: the last 30 days landed, or the import ended.
 * Until that reload succeeds, it is tried again on the next tick.
 */
export function watchImport(
	start: HackatimeLink,
	onLink: (link: HackatimeLink) => void
): () => void {
	let shown = start;
	return poll(15000, async (alive) => {
		const link = await app.api.hackatime();
		if (!alive()) return true;
		onLink(link);
		const changed = recentDone(link) !== recentDone(shown) || !importing(link);
		let ok = true;
		if (changed) {
			ok = await app.reload();
			if (ok) shown = link;
		}
		return ok && !importing(link);
	});
}

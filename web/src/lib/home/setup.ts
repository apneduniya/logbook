// The two waits on the set-up screens (first run and history). Each returns a
// stop function, so a component can return it from an $effect.

import { invalidate } from '$app/navigation';
import { app, ME } from '#lib/app.svelte.ts';
import { poll } from '#lib/poll.ts';
import type { HackatimeLink } from '#lib/types.ts';
import { importing } from './kind.ts';

/** After an install command is made: every 5 s, look for the agent's first upload, then reload into the dashboard. */
export function waitForDevice(): () => void {
	return poll(5000, async () => {
		const devices = await app.api.devices();
		if (devices.length === 0) return false;
		app.devices = devices;
		await invalidate(ME);
		return true;
	});
}

const recentDone = (l: HackatimeLink) => l.linked && !!l.import?.recent_done;

/**
 * While a Hackatime import runs: every 15 s, fetch only the link's status and
 * hand it to `onLink`, so the progress shown stays current. A failed check is
 * simply retried. The whole home reloads only when it has something new to
 * show (the last 30 days landed, or the import ended), and only if
 * `canReload()` says so: a reload can swap first run for the history view,
 * which would drop an install command the user hasn't copied yet.
 */
export function watchImport(
	start: HackatimeLink,
	onLink: (link: HackatimeLink) => void,
	canReload: () => boolean
): () => void {
	let shown = start;
	return poll(15000, async () => {
		const link = await app.api.hackatime();
		onLink(link);
		const changed = recentDone(link) !== recentDone(shown) || !importing(link);
		if (changed && canReload()) {
			shown = link;
			await invalidate(ME);
		}
		return !importing(link);
	});
}

// The two waits on the set-up screens (first run and history). Each returns a
// stop function, so a component can return it from an $effect.

import { invalidate } from '$app/navigation';
import { app, ME } from '#lib/app.svelte.ts';
import { poll } from '#lib/poll.ts';

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

/**
 * While a Hackatime import runs: reload the home every 15 s, so new history
 * and the import's progress show without a refresh. The component re-runs
 * this from its $effect with the fresh link, and stops once the import ends.
 */
export function refreshWhileImporting(): () => void {
	return poll(15000, async () => {
		await invalidate(ME);
		return false;
	});
}

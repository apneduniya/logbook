// A small repeating check for the set-up screens: waiting for the agent's
// first upload, or for a Hackatime import to bring history in.

/**
 * Runs `check` every `ms` until it resolves to true or the returned function
 * is called. A failed check (network, 5xx) is retried on the next tick, and
 * checks never overlap.
 */
export function poll(ms: number, check: () => Promise<boolean>): () => void {
	let stopped = false;
	let busy = false;
	const t = setInterval(() => {
		if (busy || stopped) return;
		busy = true;
		check()
			.then((done) => {
				if (done) stop();
			})
			.catch(() => {
				// Retried on the next tick.
			})
			.finally(() => {
				busy = false;
			});
	}, ms);
	function stop() {
		stopped = true;
		clearInterval(t);
	}
	return stop;
}

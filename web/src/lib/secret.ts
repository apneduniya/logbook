// Hands an app's new client secret from the register page to the app's page,
// once. It lives in memory only (never in storage, history state or the URL,
// docs/web.md "Security"), and the first read takes it, so a reload, a back
// navigation or another app's page never sees it again.

let held: { id: string; secret: string } | null = null;

/** Keeps `secret` for the next takeSecret(id). Replaces anything held. */
export function holdSecret(id: string, secret: string): void {
	held = { id, secret };
}

/** The secret held for app `id`, once; null after that or for another app. */
export function takeSecret(id: string): string | null {
	if (!held || held.id !== id) return null;
	const { secret } = held;
	held = null;
	return secret;
}

import { app } from '#lib/app.svelte.ts';
import type { LayoutLoad } from './$types';

// A client-only app served from index.html (docs/web.md): the session cookie
// lives on the API's origin, so only the browser can know who is signed in.
export const ssr = false;
export const prerender = false;

export const load: LayoutLoad = async () => {
	// The session is checked once per page load; sign-in and sign-out reload or
	// navigate with refreshAll.
	if (app.session === undefined) await app.loadSession();
	return {};
};

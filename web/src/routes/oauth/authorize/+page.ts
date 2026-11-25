import { app } from '#lib/app.svelte.ts';
import { ApiError } from '#lib/api.ts';
import { consentProblem } from '#lib/apps.ts';
import { loadFailed } from '#lib/load.ts';
import type { PageLoad } from './$types';

// The consent page (docs/api/programs.md, GET /oauth/authorize). The API
// sends the browser here with ?request=<id> for a pending request, or with
// ?error= when the app's link itself is broken (then nobody is redirected).
export const load: PageLoad = async ({ url, parent }) => {
	await parent();
	const problem = consentProblem(url.searchParams.get('error'));
	if (problem) return { state: 'broken' as const, problem };

	const id = url.searchParams.get('request');
	if (!id) return { state: 'missing' as const };
	if (!app.session) return { state: 'signedout' as const };

	try {
		return { state: 'ask' as const, id, request: await app.api.consentRequest(id) };
	} catch (err) {
		// Unknown, expired, decided or someone else's: the API doesn't say which.
		if (err instanceof ApiError && err.status === 404) return { state: 'expired' as const };
		loadFailed(err);
	}
};

import { error } from '@sveltejs/kit';
import { app, PROFILE } from '#lib/app.svelte.ts';
import { ApiError } from '#lib/api.ts';
import { loadFailed } from '#lib/load.ts';
import type { PageLoad } from './$types';

export const load: PageLoad = async ({ params, depends, parent }) => {
	depends(PROFILE);
	await parent();
	const own = app.user?.username.toLowerCase() === params.username.toLowerCase();
	try {
		return { profile: await app.api.publicProfile(params.username), own };
	} catch (err) {
		if (err instanceof ApiError && err.status === 404) {
			// Your own private profile is a 404 to everyone, you included.
			if (own && app.user && !app.user.profile_public) return { profile: null, own };
			error(404, 'No public profile with that name.');
		}
		loadFailed(err);
	}
};

import { redirect } from '@sveltejs/kit';
import { app, ME } from '#lib/app.svelte.ts';
import { loadFailed } from '#lib/load.ts';
import type { PageLoad } from './$types';

export const load: PageLoad = async ({ depends, url, parent }) => {
	await parent();
	if (!app.session) redirect(307, `/?next=${encodeURIComponent(url.pathname)}`);
	depends(ME);
	try {
		const [profile, keys, devices, hackatime, grants] = await Promise.all([
			app.api.profile(),
			app.api.apiKeys(),
			app.api.devices(),
			app.api.hackatime(),
			app.api.grants()
		]);
		app.devices = devices;
		return { profile, keys, devices, hackatime, grants };
	} catch (err) {
		loadFailed(err);
	}
};

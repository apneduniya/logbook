import { redirect } from '@sveltejs/kit';
import { app, ME } from '#lib/app.svelte.ts';
import type { PageLoad } from './$types';

export const load: PageLoad = async ({ depends, url }) => {
	if (!app.session) redirect(307, `/?next=${encodeURIComponent(url.pathname)}`);
	depends(ME);
	const [profile, keys, devices, hackatime] = await Promise.all([
		app.api.profile(),
		app.api.apiKeys(),
		app.api.devices(),
		app.api.hackatime()
	]);
	app.devices = devices;
	return { profile, keys, devices, hackatime };
};

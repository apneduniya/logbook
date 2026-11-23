import { redirect } from '@sveltejs/kit';
import { app, ME } from '#lib/app.svelte.ts';
import { loadFailed } from '#lib/load.ts';
import type { PageLoad } from './$types';

export const load: PageLoad = async ({ depends, url, parent }) => {
	await parent();
	if (!app.session) redirect(307, `/?next=${encodeURIComponent(url.pathname)}`);
	depends(ME);
	try {
		const projects = await app.api.projects();
		for (const p of projects) app.sawActivity(p.last_activity);
		return { projects };
	} catch (err) {
		loadFailed(err);
	}
};

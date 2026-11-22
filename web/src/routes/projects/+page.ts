import { redirect } from '@sveltejs/kit';
import { app, ME } from '#lib/app.svelte.ts';
import type { PageLoad } from './$types';

export const load: PageLoad = async ({ depends, url }) => {
	if (!app.session) redirect(307, `/?next=${encodeURIComponent(url.pathname)}`);
	depends(ME);
	const projects = await app.api.projects();
	for (const p of projects) app.sawActivity(p.last_activity);
	return { projects };
};

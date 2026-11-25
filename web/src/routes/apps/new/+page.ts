import { redirect } from '@sveltejs/kit';
import { app } from '#lib/app.svelte.ts';
import type { PageLoad } from './$types';

export const load: PageLoad = async ({ url, parent }) => {
	await parent();
	if (!app.session) redirect(307, `/?next=${encodeURIComponent(url.pathname)}`);
	return {};
};

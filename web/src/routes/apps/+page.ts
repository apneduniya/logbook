import { redirect } from '@sveltejs/kit';
import { app } from '#lib/app.svelte.ts';
import { APPS } from '#lib/apps.ts';
import { loadFailed } from '#lib/load.ts';
import type { PageLoad } from './$types';

// The apps the user owns or reviews for (docs/api/programs.md). Each row's
// count of submissions waiting is loaded by the page, after the list shows.
export const load: PageLoad = async ({ depends, url, parent }) => {
	await parent();
	if (!app.session) redirect(307, `/?next=${encodeURIComponent(url.pathname)}`);
	depends(APPS);
	try {
		return { apps: await app.api.apps() };
	} catch (err) {
		loadFailed(err);
	}
};

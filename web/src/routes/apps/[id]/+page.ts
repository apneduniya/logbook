import { error, redirect } from '@sveltejs/kit';
import { app } from '#lib/app.svelte.ts';
import { ApiError } from '#lib/api.ts';
import { APPS } from '#lib/apps.ts';
import { loadFailed } from '#lib/load.ts';
import type { PageLoad } from './$types';

// One app's record page. Members only: the API answers 404 to anyone else,
// without saying whether the app exists. The inbox loads itself.
export const load: PageLoad = async ({ params, depends, url, parent }) => {
	await parent();
	if (!app.session) redirect(307, `/?next=${encodeURIComponent(url.pathname)}`);
	depends(APPS);
	try {
		const [a, members] = await Promise.all([app.api.app(params.id), app.api.appMembers(params.id)]);
		return { app: a, members };
	} catch (err) {
		if (err instanceof ApiError && (err.status === 404 || err.status === 400)) {
			error(404, 'There’s no app here, or you aren’t one of its members.');
		}
		loadFailed(err);
	}
};

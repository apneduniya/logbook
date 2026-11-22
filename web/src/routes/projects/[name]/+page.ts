import { error, redirect } from '@sveltejs/kit';
import { app, ME } from '#lib/app.svelte.ts';
import { ApiError } from '#lib/api.ts';
import { addDays, todayIn } from '#lib/format.ts';
import type { PageLoad } from './$types';

export const load: PageLoad = async ({ params, depends, url }) => {
	if (!app.session) redirect(307, `/?next=${encodeURIComponent(url.pathname)}`);
	depends(ME);
	const end = todayIn(app.timezone);
	try {
		const [project, summary] = await Promise.all([
			app.api.project(params.name),
			app.api.summary({ start: addDays(end, -29), end, project: params.name })
		]);
		app.sawActivity(project.last_activity);
		return { project, summary };
	} catch (err) {
		if (err instanceof ApiError && (err.status === 404 || err.status === 400)) {
			error(404, 'No project with that name.');
		}
		throw err;
	}
};

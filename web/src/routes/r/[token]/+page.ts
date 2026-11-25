import { app, REVIEW } from '#lib/app.svelte.ts';
import { ApiError } from '#lib/api.ts';
import { loadFailed } from '#lib/load.ts';
import { viewerOf } from '#lib/review.ts';
import type { App, Review } from '#lib/types.ts';
import type { PageLoad } from './$types';

// The review link (ADR 0020, docs/api/submissions.md). Public: the token is
// the credential, so it works signed out. Signed in, the page also asks which
// programs the viewer belongs to, because the review endpoint doesn't say
// whether they may decide (docs/web.md, "Review page").
export const load: PageLoad = async ({ params, parent, depends }) => {
	depends(REVIEW);
	await parent();

	let review: Review;
	try {
		review = await app.api.review(params.token);
	} catch (err) {
		if (err instanceof ApiError && err.status === 404) return { state: 'missing' as const };
		if (err instanceof ApiError && err.status === 410) return { state: 'withdrawn' as const };
		loadFailed(err);
	}

	const username = app.user?.username ?? null;
	const first = viewerOf(review.submission, username, null);
	let apps: App[] | null = null;
	let appsError: string | null = null;
	let failure: string | null = null;

	// A 401 here is handled in place (keepSession), so an ended session never
	// sends a reviewer away from the link: they just can't decide yet.
	if (first.kind === 'other' && review.submission.program) {
		try {
			apps = await app.api.apps(undefined, true);
		} catch (err) {
			appsError =
				err instanceof ApiError && err.status === 401
					? 'Your sign-in ended. Sign in again to decide.'
					: err instanceof ApiError
						? err.userMessage
						: 'Couldn’t check your programs.';
		}
	}
	// Only the submitter sees why their submission failed.
	if (first.kind === 'submitter' && review.submission.status === 'failed') {
		try {
			failure = (await app.api.mySubmission(review.submission.id, undefined, true)).error ?? null;
		} catch {
			// The page still says it failed; the reason is a courtesy.
		}
	}

	return {
		state: 'ok' as const,
		review,
		viewer: viewerOf(review.submission, username, apps),
		appsError,
		failure
	};
};

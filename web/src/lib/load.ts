import { error } from '@sveltejs/kit';
import { ApiError } from './api.ts';

/**
 * Rethrows a failed API call from a load as a page error that carries the
 * API's user-safe message (e.g. "Too many requests. Try again in 30 s.").
 * Anything else (SvelteKit's own redirects and errors) passes through.
 */
export function loadFailed(err: unknown): never {
	if (err instanceof ApiError) error(err.status === 0 ? 503 : err.status, err.userMessage);
	throw err;
}

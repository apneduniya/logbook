import { defineEnvVars } from '@sveltejs/kit/env';
import process from 'node:process';
import { building } from '$app/env';

// The API origin the browser calls (ADR 0011). Inlined at build time.
export const variables = defineEnvVars({
	LOGBOOK_API_URL: {
		public: true,
		static: true,
		description: 'API origin, e.g. https://api.logbook.boogles.dev (no path, no trailing slash)',
		schema: (value) => {
			// Required for builds; dev, checks and tests default to a local API.
			// Vitest runs the build-time config too, so it is exempted by name.
			if (!value && building && !process.env.VITEST)
				throw new Error('LOGBOOK_API_URL is required (see docs/web.md)');
			const raw = value || 'http://localhost:8080';
			let url: URL;
			try {
				url = new URL(raw);
			} catch {
				throw new Error(`LOGBOOK_API_URL is not a URL: ${raw}`);
			}
			const local = ['localhost', '127.0.0.1', '[::1]'].includes(url.hostname);
			if (url.protocol !== 'https:' && !(local && url.protocol === 'http:')) {
				throw new Error('LOGBOOK_API_URL must be https (http only for localhost)');
			}
			if (url.pathname !== '/' || url.search || url.hash) {
				throw new Error('LOGBOOK_API_URL must be an origin with no path');
			}
			return url.origin;
		}
	}
});

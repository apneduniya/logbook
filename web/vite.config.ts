import process from 'node:process';
import tailwindcss from '@tailwindcss/vite';
import { defineConfig } from 'vitest/config';
import adapter from '@sveltejs/adapter-static';
import { sveltekit } from '@sveltejs/kit/vite';

type KitOptions = NonNullable<Parameters<typeof sveltekit>[0]>;
type Directives = NonNullable<NonNullable<KitOptions['csp']>['directives']>;
type HostSource = NonNullable<Directives['connect-src']>[number];

export default defineConfig(({ command }) => {
	// The API origin, also validated in src/env.ts. Only builds get a CSP: the dev
	// server needs inline scripts and a websocket for hot reload.
	const api = (process.env.LOGBOOK_API_URL ?? '') as HostSource;
	const csp =
		command === 'build'
			? {
					// Hashes for SvelteKit's inline boot script, written into a <meta> tag of
					// the generated pages. frame-ancestors can't go in a meta tag; vercel.json
					// sends it as a header with the other security headers (docs/web.md).
					mode: 'hash' as const,
					directives: {
						'default-src': ['self' as const],
						'script-src': ['self' as const],
						// Svelte's style: directives and the charts' computed sizes are inline styles.
						'style-src': ['self' as const, 'unsafe-inline' as const],
						'img-src': ['self' as const, 'data:' as const],
						'font-src': ['self' as const],
						'connect-src': ['self' as const, api],
						'form-action': ['self' as const, api],
						'base-uri': ['none' as const],
						'object-src': ['none' as const]
					}
				}
			: undefined;

	return {
		plugins: [
			tailwindcss(),
			sveltekit({
				compilerOptions: {
					// Force runes mode for the project, except for libraries. Can be removed in svelte 6.
					runes: ({ filename }) =>
						filename.split(/[/\\]/).includes('node_modules') ? undefined : true
				},
				// A static SPA (docs/web.md): every route renders in the browser from 200.html,
				// which vercel.json serves for any path that isn't a file.
				adapter: adapter({ fallback: '200.html' }),
				csp
			})
		],
		server: { port: 5173, strictPort: true },
		test: {
			expect: { requireAssertions: true },
			projects: [
				{
					extends: './vite.config.ts',
					test: {
						name: 'unit',
						environment: 'node',
						include: ['src/**/*.{test,spec}.{js,ts}'],
						exclude: ['src/**/*.svelte.{test,spec}.{js,ts}']
					}
				}
			]
		}
	};
});

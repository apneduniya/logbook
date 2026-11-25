// @ts-check
import starlight from '@astrojs/starlight';
import { defineConfig } from 'astro/config';
import starlightImageZoom from 'starlight-image-zoom';
import starlightLinksValidator from 'starlight-links-validator';
import starlightSidebarTopics from 'starlight-sidebar-topics';

import { topics } from './topics.mjs';

export default defineConfig({
	site: 'https://docs.logbook.boogles.dev',
	integrations: [
		starlight({
			title: 'Logbook docs',
			description: 'How to use Logbook, the coding time tracker for Hack Club.',
			social: [{ icon: 'github', label: 'GitHub', href: 'https://github.com/apneduniya/logbook' }],
			editLink: { baseUrl: 'https://github.com/apneduniya/logbook/edit/main/site/' },
			lastUpdated: true,
			customCss: ['./src/styles/fonts.css', './src/styles/theme.css'],
			head: [
				{
					tag: 'link',
					attrs: {
						rel: 'preload',
						href: '/fonts/geist-latin-wght-normal.woff2',
						as: 'font',
						type: 'font/woff2',
						crossorigin: '',
					},
				},
			],
			expressiveCode: {
				styleOverrides: {
					borderRadius: '0',
					borderColor: 'var(--lb-rule)',
					codeFontFamily: "'Geist Mono', monospace",
					uiFontFamily: "'Geist', sans-serif",
					frames: { shadowColor: 'transparent', frameBoxShadowCssValue: 'none' },
				},
			},
			plugins: [
				starlightSidebarTopics(topics, { exclude: ['/'] }),
				starlightImageZoom(),
				// Fails the build on a broken internal link or #anchor.
				starlightLinksValidator(),
			],
		}),
	],
});

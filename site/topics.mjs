// One sidebar topic per audience. Each topic lists its pages in reading order.
// To add a page: create src/content/docs/<topic>/<slug>.md, then add the slug here.

/** @param {string} dir @param {string[]} slugs */
const pages = (dir, slugs) => slugs.map((slug) => (slug === 'index' ? `${dir}` : `${dir}/${slug}`));

export const topics = [
	{
		id: 'users',
		label: 'I code',
		link: '/users/',
		icon: 'laptop',
		items: pages('users', [
			'index',
			'install',
			'check-it-works',
			'coming-from-hackatime',
			'editors-and-ai-tools',
			'dashboard',
			'what-counts',
			'submit',
			'privacy',
			'troubleshooting',
			'uninstall',
			'faq',
		]),
	},
	{
		id: 'reviewers',
		label: 'I review submissions',
		link: '/reviewers/',
		icon: 'document',
		items: pages('reviewers', [
			'index',
			'open-a-review-link',
			'read-the-hours',
			'evidence-and-trust',
			'days-and-ai-sessions',
			'decide',
			'inbox',
			'paste-into-airtable',
			'faq',
		]),
	},
	{
		id: 'programs',
		label: 'I run a program',
		link: '/programs/',
		icon: 'puzzle',
		items: pages('programs', [
			'index',
			'register-an-app',
			'add-reviewers',
			'switch-from-hackatime',
			'oauth',
			'api-reference',
			'receive-submissions',
			'faq',
		]),
	},
	{
		id: 'fraud-squad',
		label: "I'm on Fraud Squad",
		link: '/fraud-squad/',
		icon: 'magnifier',
		items: pages('fraud-squad', [
			'index',
			'get-an-admin-key',
			'admin-api',
			'verify-a-bundle',
			'audit-log',
			'faq',
		]),
	},
];

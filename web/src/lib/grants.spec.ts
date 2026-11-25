import { describe, expect, it } from 'vitest';
import { grantsSentence, scopeWords } from './grants.ts';

describe('scopeWords', () => {
	it.each([
		[['profile'], 'profile'],
		[['read'], 'activity'],
		[['profile', 'read'], 'profile and activity'],
		[['profile', 'read', 'later'], 'profile, activity and later'],
		[['profile', 'profile'], 'profile'],
		[[], 'nothing']
	])('%j -> %s', (scopes, want) => {
		expect(scopeWords(scopes)).toBe(want);
	});
});

describe('grantsSentence', () => {
	it.each([
		[0, 'No app can read your Logbook data.'],
		[1, 'You’ve given one app access.'],
		[2, 'You’ve given 2 apps access.']
	])('%i', (n, want) => {
		expect(grantsSentence(n)).toBe(want);
	});
});

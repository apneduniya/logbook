import { describe, expect, it } from 'vitest';
import { evidenceShort, reviewPath, statusWord } from './submissions.ts';
import type { Decision } from './types.ts';

const program = { slug: 'terra', name: 'Terra' };
const decision: Decision = {
	value: 'rejected',
	approved_seconds: null,
	reason: 'r',
	by: 'x',
	at: '2026-11-25T00:00:00Z'
};

describe('statusWord', () => {
	it.each([
		[{ status: 'verifying', decision: null, program }, 'VERIFYING'],
		[{ status: 'failed', decision: null, program }, 'FAILED'],
		[{ status: 'withdrawn', decision: null, program }, 'WITHDRAWN'],
		[{ status: 'ready', decision: null, program }, 'UNDECIDED'],
		[{ status: 'ready', decision: null, program: null }, 'NO PROGRAM'],
		[{ status: 'ready', decision, program }, 'REJECTED']
	] as const)('%j -> %s', (s, want) => {
		expect(statusWord(s)).toBe(want);
	});
});

describe('evidenceShort', () => {
	it('says verified when nothing differs', () => {
		expect(evidenceShort({ verified: 2431, mismatch: 0, unbacked: 0 })).toBe('2,431 min verified');
	});
	it('names only the counts above zero', () => {
		expect(evidenceShort({ verified: 9, mismatch: 2, unbacked: 1234 })).toBe(
			'2 differ · 1,234 unbacked'
		);
		expect(evidenceShort({ verified: 9, mismatch: 0, unbacked: 3 })).toBe('3 unbacked');
	});
});

describe('reviewPath', () => {
	it('keeps the path of a review link', () => {
		expect(reviewPath('https://logbook.boogles.dev/r/AbC-_123456789012345678')).toBe(
			'/r/AbC-_123456789012345678'
		);
	});
	it.each([
		[null],
		['not a url'],
		['https://logbook.boogles.dev/settings'],
		['https://logbook.boogles.dev/r/abc/extra'],
		['https://logbook.boogles.dev/r/a%2Fb']
	])('refuses %j', (url) => {
		expect(reviewPath(url)).toBeNull();
	});
});

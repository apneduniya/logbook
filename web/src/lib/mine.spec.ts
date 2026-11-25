import { describe, expect, it } from 'vitest';
import { canCopyLink, canWithdraw, filterCounts, matchesFilter, verifyingKey } from './mine.ts';
import type { Decision } from './types.ts';

const decision: Decision = {
	value: 'approved',
	approved_seconds: null,
	reason: '',
	by: 'reviewer',
	at: '2026-11-25T00:00:00Z'
};
const url = 'https://logbook.boogles.dev/r/AbCdEfGhIjKlMnOpQrStUv';

const verifying = { status: 'verifying', decision: null, review_url: url } as const;
const ready = { status: 'ready', decision: null, review_url: url } as const;
const decided = { status: 'ready', decision, review_url: url } as const;
const failed = { status: 'failed', decision: null, review_url: url } as const;
const withdrawn = { status: 'withdrawn', decision: null, review_url: null } as const;

describe('matchesFilter', () => {
	it.each([
		[verifying, ['all', 'verifying']],
		[ready, ['all', 'ready']],
		[decided, ['all', 'decided']],
		[failed, ['all']],
		[withdrawn, ['all']]
	] as const)('%j is under %j', (s, want) => {
		const got = (['all', 'verifying', 'ready', 'decided'] as const).filter((f) =>
			matchesFilter(s, f)
		);
		expect(got).toEqual(want);
	});
});

describe('filterCounts', () => {
	it('counts each segment', () => {
		expect(filterCounts([verifying, ready, ready, decided, failed, withdrawn])).toEqual({
			all: 6,
			verifying: 1,
			ready: 2,
			decided: 1
		});
	});
	it('is all zeros for no submissions', () => {
		expect(filterCounts([])).toEqual({ all: 0, verifying: 0, ready: 0, decided: 0 });
	});
});

describe('verifyingKey', () => {
	it('names the submissions being checked, in a stable order', () => {
		expect(
			verifyingKey([
				{ id: 'b', status: 'verifying' },
				{ id: 'c', status: 'ready' },
				{ id: 'a', status: 'verifying' }
			])
		).toBe('a b');
	});
	it('is empty when none is', () => {
		expect(verifyingKey([{ id: 'a', status: 'failed' }])).toBe('');
	});
});

describe('row actions', () => {
	it.each([
		[verifying, true, true],
		[ready, true, true],
		[decided, false, true],
		[failed, true, false],
		[withdrawn, false, false]
	] as const)('%j: withdraw %s, copy %s', (s, withdraw, copy) => {
		expect(canWithdraw(s)).toBe(withdraw);
		expect(canCopyLink(s)).toBe(copy);
	});
});

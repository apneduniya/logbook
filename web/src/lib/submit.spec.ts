import { describe, expect, it } from 'vitest';
import {
	defaultSubmitRange,
	lastSealedDay,
	rangeProblem,
	shellQuote,
	submitCommand
} from './submit.ts';

// 25 Nov 2026, 01:30 in UTC: already the 25th in UTC, still the 24th in the Americas.
const now = new Date('2026-11-25T01:30:00Z');

describe('defaultSubmitRange', () => {
	it('is the 30 UTC days ending yesterday', () => {
		expect(lastSealedDay(now)).toBe('2026-11-24');
		expect(defaultSubmitRange(now)).toEqual({ from: '2026-10-26', to: '2026-11-24' });
	});
	it('follows the UTC day, not the local one', () => {
		expect(lastSealedDay(new Date('2026-11-24T23:59:00Z'))).toBe('2026-11-23');
	});
});

describe('rangeProblem', () => {
	it.each([
		['2026-11-01', '2026-11-24', null],
		['2026-11-24', '2026-11-24', null],
		['2026-08-25', '2026-11-24', null],
		['', '2026-11-24', 'Pick a first and a last day.'],
		['2026-02-30', '2026-03-02', 'Pick a first and a last day.'],
		['2026-11-10', '2026-11-09', 'The last day is before the first.'],
		[
			'2026-11-01',
			'2026-11-25',
			'Today’s hours are sealed after midnight UTC, so end on 2026-11-24 or earlier.'
		],
		['2026-08-24', '2026-11-24', 'That’s 93 days. Submit at most 92 at a time.']
	])('%s to %s', (from, to, want) => {
		expect(rangeProblem(from, to, now)).toBe(want);
	});
});

describe('shellQuote', () => {
	it.each([
		['terra-bot', 'terra-bot', 'terra-bot'],
		['apps/web_2.0', 'apps/web_2.0', 'apps/web_2.0'],
		['my game', `'my game'`, `'my game'`],
		[`it's`, `'it'\\''s'`, `'it''s'`],
		['$HOME;rm', `'$HOME;rm'`, `'$HOME;rm'`],
		['a,b', `'a,b'`, `'a,b'`],
		['café', `'café'`, `'café'`],
		['it’s', `'it’s'`, `'it’’s'`]
	])('%s', (s, unix, windows) => {
		expect(shellQuote(s, 'unix')).toBe(unix);
		expect(shellQuote(s, 'windows')).toBe(windows);
	});
});

describe('submitCommand', () => {
	const base = { project: 'terra-bot', from: '2026-11-01', to: '2026-11-24' };
	it('fills in the project and range', () => {
		expect(submitCommand(base, 'unix')).toBe(
			'logbook submit terra-bot --from 2026-11-01 --to 2026-11-24'
		);
	});
	it('adds the program when there is one', () => {
		expect(submitCommand({ ...base, program: 'terra' }, 'unix')).toBe(
			'logbook submit terra-bot --from 2026-11-01 --to 2026-11-24 --program terra'
		);
	});
	it('quotes names for the shell', () => {
		expect(submitCommand({ ...base, project: `Sam's game` }, 'windows')).toBe(
			`logbook submit 'Sam''s game' --from 2026-11-01 --to 2026-11-24`
		);
	});
	it('puts a name that looks like a flag after --', () => {
		expect(submitCommand({ ...base, project: '-x' }, 'unix')).toBe(
			'logbook submit --from 2026-11-01 --to 2026-11-24 -- -x'
		);
	});
});

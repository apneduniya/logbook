import { describe, expect, it } from 'vitest';
import { minuteClock, minuteKind, stripRuns, stripTotals, utcMinuteOfDay } from './strip.ts';

describe('stripRuns', () => {
	it('merges equal minutes into [from, to) runs', () => {
		expect(stripRuns('yyyaaii')).toEqual([
			{ kind: 'y', from: 0, to: 3 },
			{ kind: 'a', from: 3, to: 5 },
			{ kind: 'i', from: 5, to: 7 }
		]);
	});
	it('keeps every kind of the alphabet apart', () => {
		expect(stripRuns('yacium').map((r) => r.kind)).toEqual(['y', 'a', 'c', 'i', 'u', 'm']);
	});
	it('reads anything outside the alphabet as nothing, never counted', () => {
		expect(stripRuns('y?xi')).toEqual([
			{ kind: 'y', from: 0, to: 1 },
			{ kind: 'i', from: 1, to: 4 }
		]);
	});
	it('gives no runs for an empty strip', () => {
		expect(stripRuns('')).toEqual([]);
	});
	it('covers a whole day exactly', () => {
		const day = 'i'.repeat(600) + 'y'.repeat(240) + 'm' + 'i'.repeat(599);
		const runs = stripRuns(day);
		expect(runs).toHaveLength(4);
		expect(runs.at(-1)).toEqual({ kind: 'i', from: 841, to: 1440 });
		expect(runs.reduce((s, r) => s + r.to - r.from, 0)).toBe(1440);
	});
});

describe('stripTotals', () => {
	it('counts minutes per kind', () => {
		expect(stripTotals('yyaucmi!')).toEqual({ y: 2, a: 1, c: 1, i: 2, u: 1, m: 1 });
	});
});

describe('minuteKind', () => {
	it.each([
		['y', 'y'],
		['m', 'm'],
		['Y', 'i'],
		['', 'i']
	])('%j -> %s', (ch, want) => {
		expect(minuteKind(ch)).toBe(want);
	});
});

describe('minuteClock', () => {
	it.each([
		[0, '00:00'],
		[62, '01:02'],
		[14 * 60 + 32, '14:32'],
		[1440, '24:00'],
		[-3, '00:00']
	])('%i -> %s', (m, want) => {
		expect(minuteClock(m)).toBe(want);
	});
});

describe('utcMinuteOfDay', () => {
	it('reads the UTC minute, whatever the offset', () => {
		expect(utcMinuteOfDay('2026-11-03T14:32:59Z')).toBe(14 * 60 + 32);
		expect(utcMinuteOfDay('2026-11-03T20:02:00+05:30')).toBe(14 * 60 + 32);
	});
});

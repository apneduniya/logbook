import { describe, expect, it } from 'vitest';
import {
	dayWindow,
	minuteClock,
	minuteKind,
	stripRuns,
	stripTotals,
	utcMinuteOfDay,
	windowHours
} from './strip.ts';

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

describe('dayWindow', () => {
	const at = (from: number, to: number) =>
		'i'.repeat(from) + 'y'.repeat(to - from) + 'i'.repeat(1440 - to);

	it('rounds the active minutes out to whole hours with half an hour clear', () => {
		// 10:00–11:15 on one day, 10:20–10:40 on another: 09:00–12:00.
		expect(dayWindow([at(600, 675), at(620, 640)])).toEqual({ start: 540, end: 720 });
	});
	it('includes marks such as commits outside the active minutes', () => {
		expect(dayWindow([at(600, 675)], [800])).toEqual({ start: 540, end: 840 });
	});
	it('stays inside the day', () => {
		expect(dayWindow([at(10, 1435)])).toEqual({ start: 0, end: 1440 });
	});
	it('is at least three hours, so a day never turns into per-minute cells', () => {
		expect(dayWindow([at(5, 20)])).toEqual({ start: 0, end: 180 });
		expect(dayWindow([at(1430, 1435)])).toEqual({ start: 1260, end: 1440 });
	});
	it('is the whole day when nothing happened', () => {
		expect(dayWindow(['i'.repeat(1440)])).toEqual({ start: 0, end: 1440 });
		expect(dayWindow([])).toEqual({ start: 0, end: 1440 });
	});
});

describe('windowHours', () => {
	it('marks every hour of a short window', () => {
		expect(windowHours({ start: 540, end: 720 })).toEqual([540, 600, 660, 720]);
	});
	it('thins the marks on a wide window', () => {
		expect(windowHours({ start: 0, end: 1440 })).toEqual([0, 360, 720, 1080, 1440]);
	});
});

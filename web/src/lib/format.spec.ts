import { describe, expect, it } from 'vitest';
import {
	addDays,
	ago,
	clock,
	dayLabel,
	duration,
	percent,
	rangeLabel,
	todayIn,
	zoneAbbr
} from './format.ts';

describe('duration', () => {
	it.each([
		[0, '0m'],
		[59, '0m'],
		[60, '1m'],
		[36 * 60, '36m'],
		[91 * 60, '1h 31m'],
		[5 * 3600 + 2 * 60, '5h 02m'],
		[212 * 3600 + 5 * 60, '212h'],
		[-5, '0m'],
		[null, '0m']
	])('%s s -> %s', (s, want) => {
		expect(duration(s)).toBe(want);
	});
	it('keeps minutes past 100h when long', () => {
		expect(duration(212 * 3600 + 5 * 60, { long: true })).toBe('212h 05m');
	});
});

describe('dates', () => {
	it('labels days', () => {
		expect(dayLabel('2026-11-19')).toBe('Thu 19 Nov');
	});
	it('adds days across months and years', () => {
		expect(addDays('2026-11-30', 1)).toBe('2026-12-01');
		expect(addDays('2026-01-01', -1)).toBe('2025-12-31');
	});
	it('labels ranges', () => {
		expect(rangeLabel('2026-11-13', '2026-11-19')).toBe('13 to 19 Nov');
		expect(rangeLabel('2026-10-28', '2026-11-03')).toBe('28 Oct to 3 Nov');
		expect(rangeLabel('2025-12-28', '2026-01-03')).toBe('28 Dec 2025 to 3 Jan 2026');
	});
	it('finds today in a time zone', () => {
		const at = new Date('2026-11-19T20:00:00Z');
		expect(todayIn('Asia/Kolkata', at)).toBe('2026-11-20');
		expect(todayIn('UTC', at)).toBe('2026-11-19');
		expect(todayIn('Not/AZone', at)).toBe('2026-11-19');
	});
	it('formats clock times in a zone', () => {
		expect(clock('2026-11-19T13:47:00Z', 'Asia/Kolkata')).toBe('19:17');
	});
	it('abbreviates zones', () => {
		expect(zoneAbbr('Asia/Kolkata')).toBe('IST');
		expect(zoneAbbr('UTC')).toBe('UTC');
	});
});

describe('ago', () => {
	const now = new Date('2026-11-19T12:00:00Z');
	it.each([
		['2026-11-19T11:59:30Z', 'just now'],
		['2026-11-19T11:56:00Z', '4 min ago'],
		['2026-11-19T09:00:00Z', '3 h ago'],
		['2026-11-17T12:00:00Z', '2 days ago']
	])('%s', (t, want) => {
		expect(ago(t, now)).toBe(want);
	});
});

describe('percent', () => {
	it('handles zero', () => {
		expect(percent(1, 0)).toBe(0);
		expect(percent(1, 3)).toBe(33);
	});
});

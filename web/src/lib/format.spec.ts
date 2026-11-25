import { describe, expect, it } from 'vitest';
import {
	addDays,
	ago,
	clock,
	dateRange,
	dayLabel,
	duration,
	editorName,
	hours,
	percent,
	rangeLabel,
	thousands,
	todayIn,
	utcRange,
	utcStamp,
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

describe('editorName', () => {
	it('merges the names plugins use for one editor', () => {
		expect(editorName('code')).toBe('VS Code');
		expect(editorName('vscode')).toBe('VS Code');
		expect(editorName('claude-code')).toBe('Claude Code');
		expect(editorName('gpt')).toBe('gpt');
	});
});

describe('percent', () => {
	it('handles zero', () => {
		expect(percent(1, 0)).toBe(0);
		expect(percent(1, 3)).toBe(33);
	});
});

describe('hours', () => {
	it.each([
		[44640, '12.4 h'],
		[61200, '17.0 h'],
		[0, '0.0 h'],
		[179, '0.0 h'],
		[180, '0.1 h'],
		[-60, '0.0 h']
	])('%i s -> %s', (s, want) => {
		expect(hours(s)).toBe(want);
	});
});

describe('thousands', () => {
	it('groups with commas', () => {
		expect(thousands(2431)).toBe('2,431');
		expect(thousands(12)).toBe('12');
		expect(thousands(1234567)).toBe('1,234,567');
	});
});

describe('dateRange', () => {
	it.each([
		['2026-11-03', '2026-11-09', '3–9 Nov 2026'],
		['2026-10-28', '2026-11-03', '28 Oct–3 Nov 2026'],
		['2025-12-28', '2026-01-03', '28 Dec 2025–3 Jan 2026'],
		['2026-11-03', '2026-11-03', '3 Nov 2026']
	])('%s to %s -> %s', (a, b, want) => {
		expect(dateRange(a, b)).toBe(want);
	});
});

describe('utcStamp', () => {
	it('prints the instant in UTC, whatever its offset', () => {
		expect(utcStamp('2026-11-25T19:32:00+05:30')).toBe('25 Nov 2026, 14:02 UTC');
	});
});

describe('utcRange', () => {
	it.each([
		['2026-11-03T15:02:00Z', '2026-11-03T15:20:00Z', '03 Nov 15:02–15:20'],
		['2026-11-03T23:50:00Z', '2026-11-04T00:10:00Z', '03 Nov 23:50–04 Nov 00:10'],
		['2026-11-03T23:00:00Z', '2026-11-04T00:00:00Z', '03 Nov 23:00–24:00'],
		['2026-11-03T10:00:00Z', '2026-11-05T10:00:00Z', '03 Nov 10:00–05 Nov 10:00']
	])('%s to %s -> %s', (a, b, want) => {
		expect(utcRange(a, b)).toBe(want);
	});
});

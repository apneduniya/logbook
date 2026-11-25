import { describe, expect, it } from 'vitest';
import { checkReason, MAX_REASON, parseHours, reasonLength } from './reason.ts';

describe('checkReason', () => {
	it('asks for a reason when blank', () => {
		expect(checkReason('   ')).toMatch(/Write a reason/);
	});
	it('accepts 1 to 500 characters after trimming', () => {
		expect(checkReason(' x ')).toBeNull();
		expect(checkReason('a'.repeat(MAX_REASON))).toBeNull();
		expect(checkReason('a'.repeat(MAX_REASON + 1))).toMatch(/500 characters \(this is 501\)/);
	});
	it('counts characters as the server does, not UTF-16 units', () => {
		expect(reasonLength('🙂🙂')).toBe(2);
		expect(checkReason('🙂'.repeat(MAX_REASON))).toBeNull();
	});
});

describe('parseHours', () => {
	const counted = 12.4 * 3600; // 44,640 s

	it.each([
		['11.5', 41400],
		['11,5', 41400],
		[' 11.5 h ', 41400],
		['0', 0],
		['.5', 1800],
		['12.39', Math.round(12.39 * 3600)]
	])('%j -> %i s', (text, want) => {
		expect(parseHours(text, counted)).toEqual({ seconds: want, error: null });
	});

	it('needs a number', () => {
		expect(parseHours('', counted).error).toMatch(/Enter the hours/);
		expect(parseHours('eleven', counted).error).toMatch(/as a number/);
		expect(parseHours('-1', counted).error).toMatch(/as a number/);
	});

	it('must be below the counted time', () => {
		expect(parseHours('12.4', counted).error).toBe(
			'Enter less than 12.4 h. To accept all of it, approve instead.'
		);
		expect(parseHours('20', counted).seconds).toBeNull();
	});
});

import { describe, expect, it } from 'vitest';
import { isLocalPath } from './paths.ts';

describe('isLocalPath', () => {
	it.each([
		['/', true],
		['/projects?range=30d', true],
		['//evil.example', false],
		['/\\evil.example', false],
		['https://evil.example', false],
		['projects', false],
		['/a\nb', false],
		['', false]
	])('%j -> %s', (p, want) => {
		expect(isLocalPath(p)).toBe(want);
	});
});

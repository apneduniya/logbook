import { describe, expect, it } from 'vitest';
import type { HackatimeLink, Summary, Today } from '#lib/types.ts';
import { homeKind, importing } from './kind.ts';

const summary = (total: number, credited: number | null): Summary => ({
	start: '2025-11-23',
	end: '2026-11-22',
	timezone: 'UTC',
	total_seconds: total,
	labels: { you: 0, agent: 0, idle: 0 },
	credited_seconds: credited,
	credited_labels: null,
	multi_device_minutes: 0,
	projects: [],
	languages: [],
	editors: [],
	categories: [],
	days: []
});
const today = (credited: number | null): Today => ({
	...summary(0, credited),
	streak: { current: 0, longest: 0 }
});

describe('homeKind', () => {
	it.each([
		['a device, even with nothing counted', today(0), summary(0, 0), 'dashboard'],
		['a device and history', today(120), summary(9000, 120), 'dashboard'],
		['no device, imported history', today(null), summary(3600, null), 'history'],
		['no device, nothing yet', today(null), summary(0, null), 'first-run']
	] as const)('%s', (_, t, y, want) => {
		expect(homeKind(t, y)).toBe(want);
	});
});

describe('importing', () => {
	const link = (status?: 'pending' | 'running' | 'done' | 'failed'): HackatimeLink => ({
		linked: true,
		hackatime_user_id: 1,
		trust_level: 'green',
		trust_checked_at: '',
		linked_at: '',
		import: status && {
			status,
			from_date: '',
			to_date: '',
			next_date: '',
			recent_from: '',
			recent_done: false,
			days_total: 1,
			days_done: 0,
			heartbeats_imported: 0,
			last_error: '',
			updated_at: ''
		}
	});
	it('is true only while an import is pending or running', () => {
		expect(importing({ linked: false })).toBe(false);
		expect(importing(link())).toBe(false);
		expect(importing(link('pending'))).toBe(true);
		expect(importing(link('running'))).toBe(true);
		expect(importing(link('done'))).toBe(false);
		expect(importing(link('failed'))).toBe(false);
	});
});

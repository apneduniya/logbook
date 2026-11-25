import { describe, expect, it } from 'vitest';
import { ApiError } from './api.ts';
import {
	decisionError,
	evidenceOf,
	maskReviewLinks,
	packetView,
	submissionMeta,
	trustOf,
	viewerOf
} from './review.ts';
import type { App, Packet, Review } from './types.ts';

const TOKEN = 'AbCdEfGhIjKlMnOpQrStUv';

function packet(over: Partial<Packet> = {}): Packet {
	return {
		rules_version: 'v1',
		counted_seconds: 44640,
		trust: {
			at_submission: 'green',
			now: 'green',
			checked_at: '2026-11-25T09:00:00Z',
			hackatime_linked: true,
			hackatime_linked_now: true,
			hackatime_account_changed: false
		},
		raw_seconds: 61200,
		raw_ai_seconds: 9000,
		one_third_rule_seconds: 55200,
		minutes: { verified: 2431, mismatch: 0, unbacked: 0 },
		devices: [{ name: 'laptop', os: 'windows', gaps: ['2026-11-04'] }],
		made_of: { editing: 21000, with_ai: 17640, other: 6000 },
		cuts: [
			{
				code: 'idle',
				rule: 'Nobody was at the keyboard and no AI agent was working',
				seconds: 9000,
				ranges: [
					['2026-11-03T15:02:00Z', '2026-11-03T15:20:00Z'],
					['2026-11-03T23:50:00Z', '2026-11-04T00:10:00Z']
				]
			}
		],
		flags: [
			{
				code: 'low_signal',
				rule: 'Credited without an OS input signal, from editor activity alone',
				minutes: 12,
				ranges: [['2026-11-04T10:00:00Z', '2026-11-04T10:12:00Z']]
			}
		],
		days: [
			{
				day: '2026-11-03',
				strip: 'i'.repeat(1440),
				counted_seconds: 14400,
				raw_seconds: 18000,
				commits: [
					{
						at: '2026-11-03T14:32:10Z',
						hash: '1a2b3c4',
						files: 3,
						added: 1200,
						removed: 14,
						url: 'https://github.com/o/r/commit/1a2b3c4d'
					}
				]
			},
			{
				day: '2026-11-04',
				strip: 'y'.repeat(1440),
				counted_seconds: 0,
				raw_seconds: 0,
				commits: null
			}
		],
		ai_sessions: [
			{
				harness: 'claude-code',
				model: 'claude-x',
				from: '2026-11-03T14:00:00Z',
				to: '2026-11-03T15:40:00Z',
				prompts: 14,
				turns: 14,
				counted_seconds: 3300,
				agent_alone_seconds: 600
			}
		],
		mismatches: [],
		justification: `Tracked with Logbook (rules v1). Review: https://logbook.boogles.dev/r/${TOKEN}`,
		raw_rows_deleted_after: '2027-11-25',
		...over
	};
}

function review(over: Partial<Review['submission']> = {}, p: Packet | null = packet()): Review {
	return {
		submission: {
			id: 'sub-1',
			project: 'terra-bot',
			from: '2026-11-01',
			to: '2026-11-20',
			status: 'ready',
			user: { username: 'fiona', display_name: 'Fiona Park', hackatime_id: 4321 },
			program: { slug: 'terra', name: 'Terra' },
			created_at: '2026-11-21T08:05:00Z',
			version: 2,
			...over
		},
		packet: p,
		decision: null
	};
}

describe('packetView', () => {
	const v = packetView(packet());

	it('gives the three figures in programs’ one-decimal hours', () => {
		expect(v.figures).toEqual({ counted: '12.4 h', keyboard: '17.0 h', oneThird: '15.3 h' });
	});

	it('turns cuts and flags into line items with UTC ranges', () => {
		expect(v.cuts).toEqual([
			{
				code: 'idle',
				rule: 'Nobody was at the keyboard and no AI agent was working',
				amount: '2h 30m',
				ranges: ['03 Nov 15:02–15:20', '03 Nov 23:50–04 Nov 00:10']
			}
		]);
		expect(v.flags[0].amount).toBe('12 min');
		expect(v.flags[0].ranges).toEqual(['04 Nov 10:00–10:12']);
	});

	it('builds one day row per day, with commit ticks and evidence gaps', () => {
		expect(v.days.map((d) => [d.label, d.counted, d.keyboard, d.gaps])).toEqual([
			['Tue 3 Nov', '4h 00m', '5h 00m', []],
			['Wed 4 Nov', '0m', '0m', ['laptop']]
		]);
		expect(v.days[0].ticks).toEqual([
			{
				at: 14 * 60 + 32,
				text: '1a2b3c4 · +1,200 −14 · 3 files · 14:32',
				label: 'Commit 1a2b3c4 at 14:32 UTC: 1,200 lines added, 14 removed, 3 files',
				href: 'https://github.com/o/r/commit/1a2b3c4d'
			}
		]);
		expect(v.days[1].ticks).toEqual([]);
	});

	it('never links a commit outside GitHub', () => {
		const p = packet();
		p.days![0].commits![0].url = 'https://evil.example/commit';
		expect(packetView(p).days[0].ticks[0].href).toBeUndefined();
	});

	it('names AI tools as people write them', () => {
		expect(v.sessions).toEqual([
			{
				harness: 'Claude Code',
				model: 'claude-x',
				when: '03 Nov 14:00–15:40',
				prompts: 14,
				counted: '55m',
				alone: '10m'
			}
		]);
	});

	it('copies the justification in full but shows its link masked', () => {
		expect(v.justification?.copy).toContain(TOKEN);
		expect(v.justification?.display).not.toContain(TOKEN);
		expect(v.justification?.display).toContain('/r/AbCd••••');
	});

	it('reads null lists from Go as empty', () => {
		const empty = packetView(
			packet({
				cuts: null,
				flags: null,
				days: null,
				ai_sessions: null,
				mismatches: null,
				devices: null,
				justification: undefined
			})
		);
		expect([empty.cuts, empty.flags, empty.days, empty.sessions, empty.mismatches]).toEqual([
			[],
			[],
			[],
			[],
			[]
		]);
		expect(empty.justification).toBeNull();
	});

	it('shows a mismatch as the device’s row against the server’s', () => {
		const m = packetView(
			packet({
				minutes: { verified: 10, mismatch: 1, unbacked: 0 },
				mismatches: [
					{
						minute: '2026-11-03T14:21:00Z',
						device: { label: 'you', credited: 60, cuts: null },
						server: { label: 'idle', credited: 0, cuts: ['idle'] }
					}
				]
			})
		).mismatches;
		expect(m).toEqual([
			{ when: '03 Nov 14:21–14:22', device: 'you · 60 s', server: 'idle · 0 s · cut: idle' }
		]);
	});
});

describe('evidenceOf', () => {
	it('is VERIFIED with the packet’s own sentence when every minute checks out', () => {
		const e = evidenceOf(packet());
		expect(e.clean).toBe(true);
		expect(e.tag).toBe('VERIFIED');
		expect(e.sentence).toBe('All 2,431 minutes re-computed on our server from signed device logs.');
	});
	it('counts what differs or is unbacked', () => {
		const e = evidenceOf(packet({ minutes: { verified: 1000, mismatch: 2, unbacked: 1234 } }));
		expect(e.clean).toBe(false);
		expect(e.tag).toBe('2 DIFFER · 1,234 UNBACKED');
		expect(e.total).toBe(2236);
		expect(e.sentence).toMatch(/^1,000 of 2,236 minutes re-computed/);
	});
	it('leaves a zero count out of the tag', () => {
		expect(evidenceOf(packet({ minutes: { verified: 5, mismatch: 0, unbacked: 3 } })).tag).toBe(
			'3 UNBACKED'
		);
	});
});

describe('submissionMeta', () => {
	it('lists who, what and when, with rules, devices and retention once ready', () => {
		expect(submissionMeta(review()).map((r) => [r.k, r.v])).toEqual([
			['Submitter', 'Fiona Park (fiona)'],
			['Hackatime', 'user 4321'],
			['Dates', '1–20 Nov 2026, UTC days'],
			['Program', 'Terra'],
			['Submitted', '21 Nov 2026, 08:05 UTC'],
			['Rules', 'v1'],
			['Devices', 'laptop (windows)'],
			['Raw logs kept until', '25 Nov 2027']
		]);
	});
	it('states each fact once: no version row, no unlinked Hackatime, no repeated name', () => {
		const user = { username: 'alice', display_name: 'Alice', hackatime_id: null };
		const rows = submissionMeta(review({ user }));
		expect(rows.map((r) => r.k)).not.toContain('Version');
		expect(rows.map((r) => r.k)).not.toContain('Hackatime');
		expect(rows[0]).toEqual({ k: 'Submitter', v: 'Alice' });
		expect(submissionMeta(review({ user: { ...user, display_name: '' } }))[0].v).toBe('alice');
	});
	it('has no packet rows while verifying', () => {
		const rows = submissionMeta(review({ status: 'verifying' }, null));
		expect(rows.map((r) => r.k)).not.toContain('Rules');
	});
	it('never prints the token', () => {
		expect(JSON.stringify(submissionMeta(review()))).not.toContain(TOKEN);
	});
});

describe('trustOf', () => {
	const stamp = (s: string) => s;

	it('has no warnings for a linked account in good standing', () => {
		expect(trustOf(packet().trust, stamp).warnings).toEqual([]);
	});
	it('warns when the account changed, and not also about unlinking', () => {
		const t = trustOf(
			{ ...packet().trust, hackatime_linked_now: true, hackatime_account_changed: true },
			stamp
		);
		expect(t.warnings).toHaveLength(1);
		expect(t.warnings[0]).toMatch(/different Hackatime account/);
	});
	it('warns when unlinked since and when banned now', () => {
		const t = trustOf(
			{
				...packet().trust,
				now: 'red',
				hackatime_linked_now: false,
				hackatime_account_changed: false
			},
			stamp
		);
		expect(t.warnings.join(' ')).toMatch(/unlinked Hackatime/);
		expect(t.warnings.join(' ')).toMatch(/banned now/);
	});
	it('names the level only where it has a meaning, never blue as trusted', () => {
		const t = trustOf({ ...packet().trust, at_submission: 'blue', now: 'red' }, stamp);
		expect(t.rows.map((r) => [r.k, r.v])).toEqual([
			['At submission', 'blue'],
			['Now', 'red · banned'],
			['Checked', '2026-11-25T09:00:00Z'],
			['Linked now', 'yes']
		]);
	});
	it('says when Hackatime wasn’t linked at submission, with no placeholder level now', () => {
		const t = trustOf(
			{ at_submission: 'blue', now: 'blue', hackatime_linked: false, hackatime_linked_now: false },
			stamp
		);
		expect(t.rows).toEqual([
			{ k: 'At submission', v: 'not linked' },
			{ k: 'Linked now', v: 'no' }
		]);
		expect(t.warnings).toEqual([
			'Hackatime wasn’t linked when this was submitted, so there is no account to check.'
		]);
	});
});

describe('maskReviewLinks', () => {
	it('masks every review link but leaves other text alone', () => {
		expect(maskReviewLinks(`a https://x.dev/r/${TOKEN} b http://localhost:5173/r/${TOKEN}.`)).toBe(
			'a https://x.dev/r/AbCd•••• b http://localhost:5173/r/AbCd••••.'
		);
		expect(maskReviewLinks('https://github.com/o/r')).toBe('https://github.com/o/r');
	});
});

describe('viewerOf', () => {
	const sub = review().submission;
	const app = (slug: string, role: App['role']) => ({ slug, role }) as App;

	it('is signed out without a user', () => {
		expect(viewerOf(sub, null, null)).toEqual({ kind: 'signed-out' });
	});
	it('is the submitter, even when they are a member too', () => {
		expect(viewerOf(sub, 'FIONA', [app('terra', 'owner')])).toEqual({ kind: 'submitter' });
	});
	it('is a member of the submission’s program by slug', () => {
		expect(viewerOf(sub, 'rev', [app('other', 'owner'), app('terra', 'reviewer')])).toEqual({
			kind: 'member',
			role: 'reviewer'
		});
	});
	it('is someone else without a matching app, or without a program', () => {
		expect(viewerOf(sub, 'rev', [app('other', 'owner')])).toEqual({ kind: 'other' });
		expect(viewerOf(sub, 'rev', null)).toEqual({ kind: 'other' });
		expect(viewerOf({ ...sub, program: null }, 'rev', [app('terra', 'owner')])).toEqual({
			kind: 'other'
		});
	});
});

describe('decisionError', () => {
	it('offers a reload when the packet was checked again', () => {
		const d = decisionError(
			new ApiError(
				409,
				'the submission was checked again since you read it (a part was added); reload it'
			)
		);
		expect(d.stale).toBe(true);
	});
	it('passes the server’s other sentences through, capitalised', () => {
		expect(
			decisionError(new ApiError(403, 'only the program’s owner and reviewers can decide'))
		).toEqual({ message: 'Only the program’s owner and reviewers can decide.', stale: false });
		expect(
			decisionError(
				new ApiError(
					409,
					'the submitter’s Hackatime account is banned now, so the hours can only be rejected'
				)
			).stale
		).toBe(false);
	});
	it('explains an ended sign-in, a withdrawal and a rate limit', () => {
		expect(decisionError(new ApiError(401, 'unauthorized')).message).toMatch(/Sign in again/);
		expect(decisionError(new ApiError(410, 'gone')).message).toMatch(/withdrew/);
		expect(decisionError(new ApiError(429, 'x', 30)).message).toBe(
			'Too many requests. Try again in 30 s.'
		);
	});
});

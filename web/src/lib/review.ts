// The review page's view model: a packet (docs/api/submissions.md) turned
// into the lines the page prints, plus who is looking and how a decision's
// errors read. Pure: no I/O, so it is unit-tested.

import { ApiError } from './api.ts';
import {
	dateRange,
	dayLabel,
	duration,
	editorName,
	hours,
	thousands,
	utcRange,
	utcStamp
} from './format.ts';
import { minuteClock, utcMinuteOfDay, type Tick } from './strip.ts';
import type {
	App,
	AppRole,
	Packet,
	PacketCredit,
	PacketDay,
	PacketTrust,
	Review,
	TimeRange,
	TrustLevel
} from './types.ts';

/** A key/value line of a meta block. */
export interface MetaRow {
	k: string;
	v: string;
	/** A number, date or code (mono); names and words stay sans. */
	mono?: boolean;
}

export interface DayView {
	day: string;
	label: string;
	counted: string;
	keyboard: string;
	strip: string;
	ticks: Tick[];
	/** Devices with no signed record for this day. */
	gaps: string[];
}

/** An invoice line: a rule in plain words, its amount, and where it happened. */
export interface LineItem {
	code: string;
	rule: string;
	amount: string;
	ranges: string[];
}

export interface Evidence {
	total: number;
	verified: number;
	mismatch: number;
	unbacked: number;
	/** No minute differs or is unbacked. */
	clean: boolean;
	/** The packet's own sentence (as in the justification). */
	sentence: string;
	/** "VERIFIED", or "2 DIFFER · 1,234 UNBACKED". */
	tag: string;
}

export interface TrustView {
	rows: MetaRow[];
	/** Sentences to read before deciding, e.g. why approving would be refused. */
	warnings: string[];
}

export interface PacketView {
	figures: { counted: string; keyboard: string; oneThird: string };
	evidence: Evidence;
	madeOf: { key: 'editing' | 'with_ai' | 'other'; label: string; seconds: number }[];
	cuts: LineItem[];
	flags: LineItem[];
	days: DayView[];
	sessions: {
		harness: string;
		model: string;
		when: string;
		prompts: number;
		counted: string;
		alone: string;
	}[];
	mismatches: { when: string; device: string; server: string }[];
	trust: TrustView;
	/** The justification: copied in full, shown with the review link masked. */
	justification: { copy: string; display: string } | null;
}

/**
 * The SUBMISSION block: who, what, when, under which rules. Each fact once:
 * the version is in the panel's head, and Hackatime only when there is an
 * account to name (TRUST says the rest).
 */
export function submissionMeta(review: Review): MetaRow[] {
	const s = review.submission;
	const p = review.packet ?? null;
	const { display_name: name, username } = s.user;
	const who =
		name && name.toLowerCase() !== username.toLowerCase()
			? `${name} (${username})`
			: name || username;
	const rows: MetaRow[] = [{ k: 'Submitter', v: who }];
	if (s.user.hackatime_id !== null) {
		rows.push({ k: 'Hackatime', v: `user ${s.user.hackatime_id}`, mono: true });
	}
	rows.push(
		{ k: 'Dates', v: `${dateRange(s.from, s.to)}, UTC days`, mono: true },
		{ k: 'Program', v: s.program ? s.program.name : 'none' },
		{ k: 'Submitted', v: utcStamp(s.created_at), mono: true }
	);
	if (p) {
		rows.push({ k: 'Rules', v: p.rules_version, mono: true });
		const devices = (p.devices ?? []).map((d) => (d.os ? `${d.name} (${d.os})` : d.name));
		if (devices.length) rows.push({ k: 'Devices', v: devices.join(', ') });
		if (p.raw_rows_deleted_after) {
			rows.push({
				k: 'Raw logs kept until',
				v: dateRange(p.raw_rows_deleted_after, p.raw_rows_deleted_after),
				mono: true
			});
		}
	}
	return rows;
}

export function evidenceOf(p: Packet): Evidence {
	const { verified, mismatch, unbacked } = p.minutes;
	const total = verified + mismatch + unbacked;
	const clean = mismatch === 0 && unbacked === 0;
	const sentence = clean
		? `All ${thousands(total)} minutes re-computed on our server from signed device logs.`
		: `${thousands(verified)} of ${thousands(total)} minutes re-computed on our server from signed device logs; ${thousands(mismatch)} differ from the device’s result and ${thousands(unbacked)} are not yet backed by evidence.`;
	const parts: string[] = [];
	if (mismatch) parts.push(`${thousands(mismatch)} DIFFER`);
	if (unbacked) parts.push(`${thousands(unbacked)} UNBACKED`);
	return {
		total,
		verified,
		mismatch,
		unbacked,
		clean,
		sentence,
		tag: clean ? 'VERIFIED' : parts.join(' · ')
	};
}

function ranges(rs: TimeRange[] | null | undefined): string[] {
	return (rs ?? []).map(([a, b]) => utcRange(a, b));
}

function plural(n: number, one: string, many = `${one}s`): string {
	return `${thousands(n)} ${n === 1 ? one : many}`;
}

/** The commits of one day as strip ticks. */
export function commitTicks(day: PacketDay): Tick[] {
	return (day.commits ?? []).map((c) => {
		const at = utcMinuteOfDay(c.at);
		const time = minuteClock(at);
		const files = plural(c.files, 'file');
		return {
			at,
			text: `${c.hash} · +${thousands(c.added)} −${thousands(c.removed)} · ${files} · ${time}`,
			label: `Commit ${c.hash} at ${time} UTC: ${plural(c.added, 'line')} added, ${thousands(c.removed)} removed, ${files}`,
			// The API only sets GitHub links; check again before rendering one (docs/web.md, Security).
			href: c.url && c.url.startsWith('https://github.com/') ? c.url : undefined
		};
	});
}

function creditText(c: PacketCredit): string {
	const cuts = c.cuts ?? [];
	return `${c.label} · ${c.credited} s${cuts.length ? ` · cut: ${cuts.join(', ')}` : ''}`;
}

/**
 * Hackatime's level in its own colour word. Only red has a meaning the page
 * can name; blue is Hackatime's unscored level, never "trusted".
 */
function level(l: TrustLevel): string {
	return l === 'red' ? 'red · banned' : l;
}

export function trustOf(t: PacketTrust, checkedLabel = utcStamp): TrustView {
	const rows: MetaRow[] = [
		{ k: 'At submission', v: t.hackatime_linked ? level(t.at_submission) : 'not linked' }
	];
	// With nothing linked now, the server's level is a placeholder: no row.
	if (t.hackatime_linked_now) rows.push({ k: 'Now', v: t.now ? level(t.now) : 'unknown' });
	if (t.checked_at) rows.push({ k: 'Checked', v: checkedLabel(t.checked_at), mono: true });
	rows.push({
		k: 'Linked now',
		v: t.hackatime_linked_now === undefined ? 'unknown' : t.hackatime_linked_now ? 'yes' : 'no'
	});

	const warnings: string[] = [];
	if (t.hackatime_account_changed) {
		warnings.push(
			'The submitter has linked a different Hackatime account since submitting, so “now” is that account’s level. The hours can only be rejected until they link the original again.'
		);
	} else if (t.hackatime_linked && t.hackatime_linked_now === false) {
		warnings.push(
			'The submitter has unlinked Hackatime since submitting. The hours can only be rejected until they link it again.'
		);
	}
	if (t.now === 'red') {
		warnings.push('Hackatime marks this account as banned now, so the hours can only be rejected.');
	}
	if (!t.hackatime_linked) {
		warnings.push(
			'Hackatime wasn’t linked when this was submitted, so there is no account to check.'
		);
	}
	return { rows, warnings };
}

/** "Review: https://…/r/Ab3x••••": a review link never prints its token in full. */
export function maskReviewLinks(text: string): string {
	return text.replace(
		/(https?:\/\/[^\s/]+\/r\/)([A-Za-z0-9_-]+)/g,
		(_, base: string, token: string) => `${base}${token.slice(0, 4)}••••`
	);
}

export function packetView(p: Packet): PacketView {
	const devices = p.devices ?? [];
	return {
		figures: {
			counted: hours(p.counted_seconds),
			keyboard: hours(p.raw_seconds),
			oneThird: hours(p.one_third_rule_seconds)
		},
		evidence: evidenceOf(p),
		madeOf: [
			{ key: 'editing', label: 'Editing', seconds: p.made_of.editing },
			{ key: 'with_ai', label: 'With AI', seconds: p.made_of.with_ai },
			{ key: 'other', label: 'Other', seconds: p.made_of.other }
		],
		cuts: (p.cuts ?? []).map((c) => ({
			code: c.code,
			rule: c.rule,
			amount: duration(c.seconds),
			ranges: ranges(c.ranges)
		})),
		flags: (p.flags ?? []).map((f) => ({
			code: f.code,
			rule: f.rule,
			amount: plural(f.minutes, 'min', 'min'),
			ranges: ranges(f.ranges)
		})),
		days: (p.days ?? []).map((d) => ({
			day: d.day,
			label: dayLabel(d.day),
			counted: duration(d.counted_seconds),
			keyboard: duration(d.raw_seconds),
			strip: d.strip,
			ticks: commitTicks(d),
			gaps: devices.filter((dev) => (dev.gaps ?? []).includes(d.day)).map((dev) => dev.name)
		})),
		sessions: (p.ai_sessions ?? []).map((s) => ({
			harness: editorName(s.harness),
			model: s.model,
			when: utcRange(s.from, s.to),
			prompts: s.prompts,
			counted: duration(s.counted_seconds),
			alone: duration(s.agent_alone_seconds)
		})),
		mismatches: (p.mismatches ?? []).map((m) => ({
			when: utcRange(m.minute, new Date(Date.parse(m.minute) + 60_000).toISOString()),
			device: creditText(m.device),
			server: creditText(m.server)
		})),
		trust: trustOf(p.trust),
		justification: p.justification
			? { copy: p.justification, display: maskReviewLinks(p.justification) }
			: null
	};
}

/** Who is reading a review link, which decides what the DECISION panel offers. */
export type Viewer =
	| { kind: 'signed-out' }
	| { kind: 'submitter' }
	| { kind: 'member'; role: AppRole }
	| { kind: 'other' };

/**
 * The review endpoint doesn't say whether the viewer may decide, so the page
 * asks GET /api/v1/me/apps: a member owns or reviews for the live app whose
 * slug is the submission's program. The submitter can never decide, even
 * when they are a member too. The server checks again on every decision.
 */
export function viewerOf(
	sub: Review['submission'],
	username: string | null,
	apps: App[] | null
): Viewer {
	if (username === null) return { kind: 'signed-out' };
	if (username.toLowerCase() === sub.user.username.toLowerCase()) return { kind: 'submitter' };
	const app = sub.program ? apps?.find((a) => a.slug === sub.program?.slug) : undefined;
	return app ? { kind: 'member', role: app.role } : { kind: 'other' };
}

function sentence(s: string): string {
	const t = s.trim();
	if (!t) return t;
	const head = t[0].toUpperCase() + t.slice(1);
	return /[.!?]$/.test(head) ? head : `${head}.`;
}

/**
 * What a failed decision says, by the decision controls. `stale` means the
 * packet changed since the page loaded, so the page offers a reload.
 */
export function decisionError(err: unknown): { message: string; stale: boolean } {
	if (!(err instanceof ApiError)) return { message: 'That didn’t work. Try again.', stale: false };
	if (err.status === 401) {
		return { message: 'Your sign-in ended. Sign in again to record a decision.', stale: false };
	}
	if (err.status === 409 && /checked again/i.test(err.message)) {
		return {
			message:
				'This submission was checked again since you read it. Reload to see the new numbers, then decide.',
			stale: true
		};
	}
	if (err.status === 410) {
		return { message: 'The submitter withdrew this submission.', stale: false };
	}
	return { message: sentence(err.userMessage), stale: false };
}

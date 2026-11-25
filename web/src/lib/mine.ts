// Pure helpers for the user's own submissions list (/submissions): the
// status filter, its counts, and which actions a row offers. The rows
// themselves are SubmissionRow (#lib/submissions.ts has their words).

import type { SubmissionSummary } from './types.ts';

/** The filter segments, as in docs/web.md: All · Verifying · Ready · Decided. */
export type SubmissionFilter = 'all' | 'verifying' | 'ready' | 'decided';

export const FILTERS: { value: SubmissionFilter; label: string }[] = [
	{ value: 'all', label: 'All' },
	{ value: 'verifying', label: 'Verifying' },
	{ value: 'ready', label: 'Ready' },
	{ value: 'decided', label: 'Decided' }
];

type Row = Pick<SubmissionSummary, 'status' | 'decision' | 'review_url'>;

/**
 * Whether a submission belongs under a filter. "Ready" is re-checked and
 * waiting for a decision; "Decided" has one. Failed and withdrawn
 * submissions show under All only.
 */
export function matchesFilter(s: Row, f: SubmissionFilter): boolean {
	switch (f) {
		case 'all':
			return true;
		case 'verifying':
			return s.status === 'verifying';
		case 'ready':
			return s.status === 'ready' && !s.decision;
		case 'decided':
			return !!s.decision;
	}
}

/** How many submissions each filter holds, for the segment labels. */
export function filterCounts(list: Row[]): Record<SubmissionFilter, number> {
	const n = { all: 0, verifying: 0, ready: 0, decided: 0 };
	for (const s of list) {
		for (const f of FILTERS) if (matchesFilter(s, f.value)) n[f.value]++;
	}
	return n;
}

/**
 * Withdraw is offered until a decision is recorded (the API refuses it
 * after, docs/api/submissions.md). A failed submission can be withdrawn
 * too: that deletes the rows it uploaded.
 */
export function canWithdraw(s: Row): boolean {
	return s.status !== 'withdrawn' && !s.decision;
}

/** A link worth pasting into a program's form: not withdrawn (no link) and not failed (nothing to review). */
export function canCopyLink(s: Row): boolean {
	return !!s.review_url && s.status !== 'failed';
}

/**
 * Which submissions are being checked, as one comparable string ("" for
 * none). The list reloads when it changes: one finished, or a new one came.
 */
export function verifyingKey(list: Pick<SubmissionSummary, 'id' | 'status'>[]): string {
	return list
		.filter((s) => s.status === 'verifying')
		.map((s) => s.id)
		.sort()
		.join(' ');
}

/** The sentence shown when a filter holds nothing. */
export function emptyFilterSentence(f: Exclude<SubmissionFilter, 'all'>): string {
	return {
		verifying: 'Nothing is being checked right now.',
		ready: 'Nothing is waiting for a decision.',
		decided: 'No program has decided on your hours yet.'
	}[f];
}

// Pure helpers for submission lists (SubmissionRow): what a row's status,
// evidence and link say. docs/api/submissions.md has the fields.

import { thousands } from './format.ts';
import type { MinuteCounts, SubmissionSummary } from './types.ts';

/** The status glyph, drawn in ink (never a coloured pill). */
export const STATUS_GLYPH: Record<SubmissionSummary['status'], string> = {
	verifying: '○',
	ready: '●',
	failed: '✕',
	withdrawn: '—'
};

/** The row's state in words, for the decision column when there is no stamp. */
export function statusWord(s: Pick<SubmissionSummary, 'status' | 'decision' | 'program'>): string {
	if (s.status === 'verifying') return 'VERIFYING';
	if (s.status === 'failed') return 'FAILED';
	if (s.status === 'withdrawn') return 'WITHDRAWN';
	if (s.decision) return s.decision.value.toUpperCase();
	return s.program ? 'UNDECIDED' : 'NO PROGRAM';
}

/** "2,431 min verified", or "2 differ · 1,234 unbacked". */
export function evidenceShort(m: MinuteCounts): string {
	if (m.mismatch === 0 && m.unbacked === 0) return `${thousands(m.verified)} min verified`;
	const parts: string[] = [];
	if (m.mismatch) parts.push(`${thousands(m.mismatch)} differ`);
	if (m.unbacked) parts.push(`${thousands(m.unbacked)} unbacked`);
	return parts.join(' · ');
}

/**
 * The in-app path of a review link ("/r/<token>"), or null for anything
 * else. The API gives the full URL on the web origin; the app navigates by
 * path so the link never leaves the site.
 */
export function reviewPath(reviewUrl: string | null): string | null {
	if (!reviewUrl) return null;
	try {
		const path = new URL(reviewUrl).pathname;
		return /^\/r\/[A-Za-z0-9_-]+$/.test(path) ? path : null;
	} catch {
		return null;
	}
}

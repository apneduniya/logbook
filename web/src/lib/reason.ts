// Checks for ReasonConfirm's fields: a decision's reason and adjusted hours
// (docs/api/submissions.md, decision rules). The server checks again.

import { hours } from './format.ts';

/** A reason is trimmed, then 1 to 500 characters. */
export const MAX_REASON = 500;

/** Characters as the server counts them (code points), after trimming. */
export function reasonLength(text: string): number {
	return [...text.trim()].length;
}

/** null when the reason is fine, else what to fix. */
export function checkReason(text: string): string | null {
	const n = reasonLength(text);
	if (n === 0) return 'Write a reason. The submitter sees it.';
	if (n > MAX_REASON) return `Keep it to ${MAX_REASON} characters (this is ${n}).`;
	return null;
}

/**
 * Parses hours typed as "11.5", "11,5" or "11.5 h" into whole seconds,
 * which must be at least 0 and below `below` seconds (the counted time).
 */
export function parseHours(
	text: string,
	below: number
): { seconds: number; error: null } | { seconds: null; error: string } {
	const t = text.trim().replace(',', '.');
	const m = /^(\d+(?:\.\d*)?|\.\d+)\s*h?$/i.exec(t);
	if (!t) return { seconds: null, error: 'Enter the hours to approve.' };
	if (!m) return { seconds: null, error: 'Enter hours as a number, like 11.5.' };
	const seconds = Math.round(Number(m[1]) * 3600);
	if (seconds >= below) {
		return {
			seconds: null,
			error: `Enter less than ${hours(below)}. To accept all of it, approve instead.`
		};
	}
	return { seconds, error: null };
}

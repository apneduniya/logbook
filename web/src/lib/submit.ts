// The `logbook submit` command a project page shows (docs/agent/submit.md):
// its default range, the range checks the agent makes before it calls the
// server, and quoting the project name for the user's shell. Pure: "now" is
// passed in.

import { addDays, isoDate } from './format.ts';

/** Which shell the command is pasted into; only quoting differs. */
export type Shell = 'unix' | 'windows';

/** The agent refuses longer ranges ("submit at most 92 days at a time"). */
export const MAX_SUBMIT_DAYS = 92;

/** How many days the project page fills in by default, as its "Last 30 days" panel. */
const DEFAULT_DAYS = 30;

/** The last UTC day the agent accepts: today's evidence is sealed after midnight UTC. */
export function lastSealedDay(now: Date = new Date()): string {
	return addDays(isoDate(now), -1);
}

/** The default range: the 30 UTC days ending yesterday. */
export function defaultSubmitRange(now: Date = new Date()): { from: string; to: string } {
	const to = lastSealedDay(now);
	return { from: addDays(to, -(DEFAULT_DAYS - 1)), to };
}

/** Whether s is a real YYYY-MM-DD day. */
function isDay(s: string): boolean {
	if (!/^\d{4}-\d{2}-\d{2}$/.test(s)) return false;
	const d = new Date(`${s}T00:00:00Z`);
	return !Number.isNaN(d.getTime()) && isoDate(d) === s;
}

/**
 * Why the agent would refuse this range, as a sentence, or null when it
 * would take it. The same checks and limits as `logbook submit` step 1.
 */
export function rangeProblem(from: string, to: string, now: Date = new Date()): string | null {
	if (!isDay(from) || !isDay(to)) return 'Pick a first and a last day.';
	if (to < from) return 'The last day is before the first.';
	const last = lastSealedDay(now);
	if (to > last) {
		return `Today’s hours are sealed after midnight UTC, so end on ${last} or earlier.`;
	}
	const days =
		Math.round((Date.parse(`${to}T00:00:00Z`) - Date.parse(`${from}T00:00:00Z`)) / 864e5) + 1;
	if (days > MAX_SUBMIT_DAYS) {
		return `That’s ${days} days. Submit at most ${MAX_SUBMIT_DAYS} at a time.`;
	}
	return null;
}

/** Characters that need no quoting in a POSIX shell or PowerShell. */
const PLAIN = /^[A-Za-z0-9._/-]+$/;

/**
 * One argument, quoted for the shell when it needs it. Single quotes in
 * both: POSIX ends the quote around a ' ('\''), PowerShell doubles it (and
 * treats the typographic single quotes as quotes too).
 */
export function shellQuote(s: string, shell: Shell): string {
	if (PLAIN.test(s)) return s;
	if (shell === 'windows') return `'${s.replace(/['‘’‚‛]/g, '$&$&')}'`;
	return `'${s.replaceAll("'", `'\\''`)}'`;
}

export interface SubmitArgs {
	project: string;
	from: string;
	to: string;
	/** A program's slug, or empty for none. */
	program?: string;
}

/**
 * The command to run: `logbook submit <project> --from D1 --to D2
 * [--program <slug>]`. A project name starting with "-" goes after `--`, so
 * the agent's flag parser doesn't read it as a flag.
 */
export function submitCommand(a: SubmitArgs, shell: Shell): string {
	const flags = [`--from ${a.from}`, `--to ${a.to}`];
	if (a.program) flags.push(`--program ${shellQuote(a.program, shell)}`);
	const project = shellQuote(a.project, shell);
	return a.project.startsWith('-')
		? `logbook submit ${flags.join(' ')} -- ${project}`
		: `logbook submit ${project} ${flags.join(' ')}`;
}

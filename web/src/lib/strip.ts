// A minute strip: one character per minute, in the review packet's alphabet
// (docs/api/submissions.md, `strip`). Pure helpers for MinuteStrip.

/** The packet's minute kinds. */
export type MinuteKind = 'y' | 'a' | 'c' | 'i' | 'u' | 'm';

/** What each kind means, in the words the UI uses (web/DESIGN.md, "Words"). */
export const KIND_NAMES: Record<MinuteKind, string> = {
	y: 'counted',
	a: 'agent alone',
	c: 'cut',
	i: 'idle or nothing',
	u: 'counted, not backed by evidence',
	m: 'differs from the device'
};

/** A marker above a strip, such as a commit. */
export interface Tick {
	/** The minute (index into the strip) it falls in. */
	at: number;
	/** The detail line shown on hover, focus or tap: "1a2b3c4 · +120 −14 · 3 files · 14:32". */
	text: string;
	/** The tick's accessible name, a sentence. */
	label: string;
	/** Where the detail links to (outside the site), if anywhere. */
	href?: string;
}

/** A run of equal minutes: [from, to) as indexes into the strip. */
export interface Run {
	kind: MinuteKind;
	from: number;
	to: number;
}

/** The kind of one strip character. */
export function minuteKind(ch: string): MinuteKind {
	// Anything outside the alphabet reads as nothing, never as counted.
	return ch === 'y' || ch === 'a' || ch === 'c' || ch === 'u' || ch === 'm' ? ch : 'i';
}

/** Merges a strip into runs of equal minutes, in order. */
export function stripRuns(strip: string): Run[] {
	const runs: Run[] = [];
	for (let i = 0; i < strip.length; i++) {
		const kind = minuteKind(strip[i]);
		const last = runs[runs.length - 1];
		if (last && last.kind === kind) last.to = i + 1;
		else runs.push({ kind, from: i, to: i + 1 });
	}
	return runs;
}

/** Minutes of each kind in a strip. */
export function stripTotals(strip: string): Record<MinuteKind, number> {
	const out: Record<MinuteKind, number> = { y: 0, a: 0, c: 0, i: 0, u: 0, m: 0 };
	for (const ch of strip) out[minuteKind(ch)]++;
	return out;
}

/** "14:02" for a minute of the day (0 to 1440; 1440 is "24:00"). */
export function minuteClock(minute: number): string {
	const m = Math.max(0, Math.round(minute));
	return `${String(Math.floor(m / 60)).padStart(2, '0')}:${String(m % 60).padStart(2, '0')}`;
}

/** The minute of the UTC day an RFC 3339 instant falls in. */
export function utcMinuteOfDay(instant: string): number {
	const d = new Date(instant);
	return d.getUTCHours() * 60 + d.getUTCMinutes();
}

/** A stretch of the day in minutes: [start, end). */
export interface Window {
	start: number;
	end: number;
}

const DAY = 1440;
/** Shorter windows would switch MinuteStrip to per-minute cells. */
const MIN_WINDOW = 180;
/** Minutes kept clear either side of the activity. */
const MARGIN = 30;

/**
 * The one window every day's strip shares: from the earliest to the latest
 * minute that isn't idle (or carries a mark, such as a commit), with at
 * least half an hour either side, rounded out to whole hours; at least three
 * hours and never more than the day. 10:00–11:15 gives 09:00–12:00. With
 * nothing in it, the whole day.
 */
export function dayWindow(strips: string[], marks: number[] = []): Window {
	let first = DAY;
	let last = -1;
	for (const s of strips) {
		for (let i = 0; i < s.length; i++) {
			if (minuteKind(s[i]) === 'i') continue;
			first = Math.min(first, i);
			last = Math.max(last, i);
		}
	}
	for (const m of marks) {
		first = Math.min(first, m);
		last = Math.max(last, m);
	}
	if (last < 0) return { start: 0, end: DAY };
	let start = Math.max(0, Math.floor((first - MARGIN) / 60) * 60);
	let end = Math.min(DAY, Math.ceil((last + 1 + MARGIN) / 60) * 60);
	if (end - start < MIN_WINDOW) {
		end = Math.min(DAY, start + MIN_WINDOW);
		start = Math.max(0, end - MIN_WINDOW);
	}
	return { start, end };
}

/** Hour marks for a window's scale: every hour, or every 2, 3 or 6 when it's wide. */
export function windowHours({ start, end }: Window): number[] {
	const span = (end - start) / 60;
	const step = span <= 8 ? 1 : span <= 14 ? 2 : span <= 18 ? 3 : 6;
	const out: number[] = [];
	for (let h = Math.ceil(start / 60); h * 60 <= end; h++) {
		if (h % step === 0) out.push(h * 60);
	}
	return out;
}

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

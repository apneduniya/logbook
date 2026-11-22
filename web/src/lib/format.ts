// Pure formatting helpers. Durations come from the API in seconds; dates are
// YYYY-MM-DD local dates in the user's time zone.

/** "0m", "36m", "1h 31m", "212h" (with long: false, hours only past 100h). */
export function duration(
	seconds: number | null | undefined,
	opts: { long?: boolean } = {}
): string {
	const total = Math.max(0, Math.floor((seconds ?? 0) / 60));
	const h = Math.floor(total / 60);
	const m = total % 60;
	if (h === 0) return `${m}m`;
	if (h >= 100 && !opts.long) return `${h}h`;
	return `${h}h ${String(m).padStart(2, '0')}m`;
}

/** Parse a YYYY-MM-DD local date as a UTC midnight Date, for calendar maths. */
export function parseDate(date: string): Date {
	const [y, m, d] = date.split('-').map(Number);
	return new Date(Date.UTC(y, m - 1, d));
}

/** Format a UTC-midnight Date back to YYYY-MM-DD. */
export function isoDate(d: Date): string {
	return d.toISOString().slice(0, 10);
}

/** The date `days` after (or before, if negative) a YYYY-MM-DD date. */
export function addDays(date: string, days: number): string {
	const d = parseDate(date);
	d.setUTCDate(d.getUTCDate() + days);
	return isoDate(d);
}

/** Today's YYYY-MM-DD in an IANA time zone. */
export function todayIn(timeZone: string, now: Date = new Date()): string {
	// en-CA formats as YYYY-MM-DD.
	return new Intl.DateTimeFormat('en-CA', {
		timeZone: safeZone(timeZone),
		year: 'numeric',
		month: '2-digit',
		day: '2-digit'
	}).format(now);
}

const WEEKDAYS = ['Sun', 'Mon', 'Tue', 'Wed', 'Thu', 'Fri', 'Sat'];
const MONTHS = ['Jan', 'Feb', 'Mar', 'Apr', 'May', 'Jun', 'Jul', 'Aug', 'Sep', 'Oct', 'Nov', 'Dec'];

/** "Thu 19 Nov" */
export function dayLabel(date: string): string {
	const d = parseDate(date);
	return `${WEEKDAYS[d.getUTCDay()]} ${d.getUTCDate()} ${MONTHS[d.getUTCMonth()]}`;
}

/** "Thu 19" */
export function shortDay(date: string): string {
	const d = parseDate(date);
	return `${WEEKDAYS[d.getUTCDay()]} ${d.getUTCDate()}`;
}

/** "13 to 19 Nov", or "28 Oct to 3 Nov" across months, with the year when it differs. */
export function rangeLabel(start: string, end: string): string {
	const a = parseDate(start);
	const b = parseDate(end);
	const sameYear = a.getUTCFullYear() === b.getUTCFullYear();
	const sameMonth = sameYear && a.getUTCMonth() === b.getUTCMonth();
	const left = sameMonth
		? `${a.getUTCDate()}`
		: `${a.getUTCDate()} ${MONTHS[a.getUTCMonth()]}${sameYear ? '' : ` ${a.getUTCFullYear()}`}`;
	return `${left} to ${b.getUTCDate()} ${MONTHS[b.getUTCMonth()]}${sameYear ? '' : ` ${b.getUTCFullYear()}`}`;
}

/** "6 Nov" */
export function dayMonth(date: string): string {
	const d = parseDate(date);
	return `${d.getUTCDate()} ${MONTHS[d.getUTCMonth()]}`;
}

/** Month abbreviation for a date. */
export function monthName(date: string): string {
	return MONTHS[parseDate(date).getUTCMonth()];
}

/** "19:17" for an RFC 3339 instant, in a time zone. */
export function clock(instant: string, timeZone: string): string {
	return new Intl.DateTimeFormat('en-GB', {
		timeZone: safeZone(timeZone),
		hour: '2-digit',
		minute: '2-digit',
		hourCycle: 'h23'
	}).format(new Date(instant));
}

/** "Thu 19 Nov, 19:17" for an RFC 3339 instant, in a time zone. */
export function instantLabel(instant: string, timeZone: string): string {
	return `${dayLabel(todayIn(timeZone, new Date(instant)))}, ${clock(instant, timeZone)}`;
}

/** "just now", "4 min ago", "3 h ago", "2 days ago". */
export function ago(instant: string, now: Date = new Date()): string {
	const s = Math.max(0, (now.getTime() - new Date(instant).getTime()) / 1000);
	if (s < 90) return 'just now';
	if (s < 3600) return `${Math.round(s / 60)} min ago`;
	if (s < 48 * 3600) return `${Math.round(s / 3600)} h ago`;
	return `${Math.round(s / 86400)} days ago`;
}

/** Short zone name shown in footers: "IST", "UTC", "GMT+2". */
export function zoneAbbr(timeZone: string, now: Date = new Date()): string {
	const parts = new Intl.DateTimeFormat('en-IN', {
		timeZone: safeZone(timeZone),
		timeZoneName: 'short'
	}).formatToParts(now);
	return parts.find((p) => p.type === 'timeZoneName')?.value ?? timeZone;
}

/** Whole-number percent of part in whole, 0 when whole is 0. */
export function percent(part: number, whole: number): number {
	return whole > 0 ? Math.round((part / whole) * 100) : 0;
}

/** Editor names as people write them. Unknown names pass through. */
export function editorName(name: string): string {
	const known: Record<string, string> = {
		vscode: 'VS Code',
		godot: 'Godot',
		intellij: 'IntelliJ',
		neovim: 'Neovim',
		vim: 'Vim',
		emacs: 'Emacs',
		cursor: 'Cursor',
		zed: 'Zed',
		sublime: 'Sublime Text',
		xcode: 'Xcode',
		androidstudio: 'Android Studio',
		webstorm: 'WebStorm',
		pycharm: 'PyCharm'
	};
	return known[name.toLowerCase()] ?? name;
}

function safeZone(timeZone: string): string {
	try {
		new Intl.DateTimeFormat('en', { timeZone });
		return timeZone;
	} catch {
		return 'UTC';
	}
}

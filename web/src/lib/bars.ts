// Bars for the DayBars chart, from summary days.

import { dayLabel, dayMonth, shortDay } from '#lib/format.ts';
import type { Day } from '#lib/types.ts';

export interface Bar {
	key: string;
	label: string;
	title: string;
	keyboard: number;
	counted: number;
	today: boolean;
}

/** One bar per day; the last day is today. */
export function dailyBars(days: Day[]): Bar[] {
	return days.map((d, i) => ({
		key: d.date,
		label: shortDay(d.date),
		title: dayLabel(d.date),
		keyboard: d.seconds,
		counted: d.credited_seconds,
		today: i === days.length - 1
	}));
}

/** One bar per 7 days, counting back from the last day. */
export function weeklyBars(days: Day[]): Bar[] {
	const out: Bar[] = [];
	for (let end = days.length; end > 0; end -= 7) {
		const chunk = days.slice(Math.max(0, end - 7), end);
		out.unshift({
			key: chunk[0].date,
			label: dayMonth(chunk[0].date),
			title: `Week of ${dayMonth(chunk[0].date)}`,
			keyboard: chunk.reduce((s, d) => s + d.seconds, 0),
			counted: chunk.reduce((s, d) => s + d.credited_seconds, 0),
			today: end === days.length
		});
	}
	return out;
}

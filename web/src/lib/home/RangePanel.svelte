<!--
  The chosen range by day (7d, 30d) or by week (year): counted time in ink
  inside a hatched box for time at the keyboard. Then best day, daily average
  and the previous period.
-->
<script lang="ts">
	import { resolve } from '$app/paths';
	import { goto } from '$app/navigation';
	import { dayLabel, duration, percent, rangeLabel } from '#lib/format.ts';
	import type { Day, Summary } from '#lib/types.ts';
	import Panel from '#lib/components/Panel.svelte';
	import Segmented from '#lib/components/Segmented.svelte';
	import DayBars from '#lib/components/DayBars.svelte';
	import { dailyBars, weeklyBars } from '#lib/bars.ts';
	import type { Range } from '#lib/types.ts';

	let { range, summary, year }: { range: Range; summary: Summary; year: Summary } = $props();

	const titles: Record<Range, string> = {
		'7d': 'Last 7 days',
		'30d': 'Last 30 days',
		year: 'Last year'
	};

	const bars = $derived(range === 'year' ? weeklyBars(summary.days) : dailyBars(summary.days));
	const counted = $derived(summary.credited_seconds ?? 0);

	const best = $derived(
		summary.days.reduce<Day | null>(
			(b, d) => (!b || d.credited_seconds > b.credited_seconds ? d : b),
			null
		)
	);
	const average = $derived(summary.days.length ? counted / summary.days.length : 0);

	// The period before this one, from the year's days (none for the year itself).
	const previous = $derived.by(() => {
		if (range === 'year') return null;
		const n = summary.days.length;
		const prior = year.days.slice(-2 * n, -n);
		if (prior.length < n) return null;
		return {
			counted: prior.reduce((s, d) => s + d.credited_seconds, 0),
			label: rangeLabel(prior[0].date, prior[prior.length - 1].date)
		};
	});
	const change = $derived(
		previous && previous.counted > 0 ? percent(counted - previous.counted, previous.counted) : null
	);

	function pick(r: Range) {
		void goto(r === '7d' ? resolve('/') : resolve(`/?range=${r}`), { reset: false, replace: true });
	}
</script>

<Panel title={titles[range]} meta={rangeLabel(summary.start, summary.end)}>
	<div class="grid gap-5">
		<div class="flex flex-wrap items-center justify-between gap-3">
			<div class="text-base">
				<span class="mono">{duration(counted)}</span> <span class="mute">counted of</span>
				<span class="mono">{duration(summary.total_seconds)}</span>
				<span class="mute">at the keyboard</span>
			</div>
			<Segmented
				label="Range"
				value={range}
				onchange={pick}
				options={[
					{ value: '7d', label: '7d' },
					{ value: '30d', label: '30d' },
					{ value: 'year', label: 'Year' }
				]}
			/>
		</div>

		<div>
			<DayBars
				{bars}
				label="Counted time {range === 'year' ? 'per week' : 'per day'}, {rangeLabel(
					summary.start,
					summary.end
				)}"
			/>
		</div>

		<div class="stats">
			<div>
				<div class="lbl">Best day</div>
				<div class="mono mt-1 text-lg">{duration(best?.credited_seconds ?? 0)}</div>
				<div class="mute">
					{best && best.credited_seconds > 0 ? dayLabel(best.date) : 'none yet'}
				</div>
			</div>
			<div>
				<div class="lbl">Daily average</div>
				<div class="mono mt-1 text-lg">{duration(average)}</div>
				<div class="mute">counted, over {summary.days.length} days</div>
			</div>
			{#if previous}
				<div>
					<div class="lbl">Previous {summary.days.length} days</div>
					<div class="mono mt-1 text-lg">{duration(previous.counted)}</div>
					<div class="mute">
						{previous.label}{#if change !== null}{` · now ${Math.abs(change)}% ${change >= 0 ? 'more' : 'less'}`}{/if}
					</div>
				</div>
			{/if}
		</div>
	</div>
</Panel>

<style>
	.stats {
		display: grid;
		grid-template-columns: repeat(3, minmax(0, 1fr));
		gap: 16px;
		padding-top: 16px;
		border-top: 1px solid var(--color-rule);
	}
	@media (max-width: 560px) {
		.stats {
			grid-template-columns: 1fr 1fr;
		}
	}
</style>

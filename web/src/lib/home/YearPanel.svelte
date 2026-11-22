<!--
  The last 365 days as a grid of dots, one column per week (Monday first),
  shaded by time at the keyboard. Today is outlined in the accent; the peak
  day gets a ring.
-->
<script lang="ts">
	import { dayLabel, duration, monthName, parseDate } from '#lib/format.ts';
	import type { Day } from '#lib/types.ts';
	import Panel from '#lib/components/Panel.svelte';

	let { days }: { days: Day[] } = $props();

	// On narrow screens the grid scrolls; start at the recent end.
	let scroller = $state<HTMLElement | null>(null);
	$effect(() => {
		if (scroller && days.length) scroller.scrollLeft = scroller.scrollWidth;
	});

	// Monday = 0 … Sunday = 6
	const weekday = (date: string) => (parseDate(date).getUTCDay() + 6) % 7;
	const offset = $derived(days.length ? weekday(days[0].date) : 0);
	const columns = $derived(Math.ceil((offset + days.length) / 7));
	const total = $derived(days.reduce((s, d) => s + d.seconds, 0));
	const peak = $derived(
		days.reduce<Day | null>((b, d) => (d.seconds > (b?.seconds ?? 0) ? d : b), null)
	);

	function level(s: number): number {
		const m = s / 60;
		return m === 0 ? 0 : m < 30 ? 1 : m < 90 ? 2 : m < 180 ? 3 : 4;
	}

	// A month label over the first column whose week holds that month's 1st…7th.
	const months = $derived.by(() => {
		const out: { col: number; name: string }[] = [];
		const seen: Record<string, true> = {};
		days.forEach((d, i) => {
			const date = parseDate(d.date);
			const col = Math.floor((i + offset) / 7);
			const key = `${date.getUTCFullYear()}-${date.getUTCMonth()}`;
			if (date.getUTCDate() <= 7 && !seen[key] && col > 0 && col < columns - 2) {
				seen[key] = true;
				out.push({ col, name: monthName(d.date) });
			}
		});
		return out;
	});
</script>

<Panel title="Year" meta="{duration(total)} at the keyboard">
	<div class="scroll" bind:this={scroller}>
		<div class="year" style:--cols={columns}>
			<span></span>
			<div class="months lbl" aria-hidden="true">
				{#each months as m (m.col)}
					<span style:grid-column="{m.col + 1} / span 3">{m.name}</span>
				{/each}
			</div>
			<div class="wd lbl" aria-hidden="true">
				<span></span><span>Tue</span><span></span><span>Thu</span><span></span><span>Sat</span><span
				></span>
			</div>
			<div
				class="grid-days"
				role="img"
				aria-label="Time at the keyboard per day for the last year: {duration(total)} in total{peak
					? `, most on ${dayLabel(peak.date)} (${duration(peak.seconds)})`
					: ''}"
			>
				{#each { length: offset }, i (i)}<span></span>{/each}
				{#each days as d, i (d.date)}
					<span
						class="dot l{level(d.seconds)}"
						class:today={i === days.length - 1}
						class:peak={peak && d.date === peak.date}
						title="{dayLabel(d.date)} · {duration(d.seconds)}"
					></span>
				{/each}
			</div>
		</div>
	</div>
	<div class="mt-3.5 flex flex-wrap items-center justify-between gap-3">
		<span class="lbl">
			{#if peak}Peak: <span class="text-ink">{duration(peak.seconds)}</span> on {dayLabel(
					peak.date
				)} ·
			{/if}hover a day for its time
		</span>
		<span class="lbl flex items-center gap-[5px]" aria-hidden="true">
			less {#each [0, 1, 2, 3, 4] as l (l)}<i class="dot key l{l}"></i>{/each} more
		</span>
	</div>
</Panel>

<style>
	.scroll {
		overflow-x: auto;
	}
	.year {
		display: grid;
		grid-template-columns: 32px 1fr;
		gap: 8px;
		min-width: 720px;
	}
	.months,
	.grid-days {
		display: grid;
		grid-template-columns: repeat(var(--cols), 1fr);
		gap: 3px;
	}
	.months {
		height: 16px;
	}
	.wd {
		display: grid;
		grid-template-rows: repeat(7, 14px);
		gap: 3px;
		line-height: 14px;
	}
	.grid-days {
		grid-template-rows: repeat(7, 14px);
		grid-auto-flow: column;
		justify-items: center;
		align-items: center;
	}
	.dot {
		width: 12px;
		height: 12px;
		border-radius: 50%;
		display: block;
	}
	.dot.key {
		width: 10px;
		height: 10px;
	}
	.l0 {
		background: var(--color-fill);
	}
	.l1 {
		background: color-mix(in srgb, var(--color-ink) 25%, var(--color-fill));
	}
	.l2 {
		background: color-mix(in srgb, var(--color-ink) 48%, var(--color-fill));
	}
	.l3 {
		background: color-mix(in srgb, var(--color-ink) 72%, var(--color-fill));
	}
	.l4 {
		background: var(--color-ink);
	}
	.dot.peak {
		box-shadow:
			0 0 0 2px var(--color-ground),
			0 0 0 3px var(--color-ink-2);
	}
	.dot.today {
		outline: 1px solid var(--color-accent);
		outline-offset: 2px;
	}
</style>

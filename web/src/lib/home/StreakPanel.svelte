<!--
  Streak: days in a row with at least 15 counted minutes (docs/api/stats.md),
  and the last 14 days as squares, today outlined in the accent.
-->
<script lang="ts">
	import { dayMonth, duration } from '#lib/format.ts';
	import type { Day } from '#lib/types.ts';
	import Panel from '#lib/components/Panel.svelte';

	const QUALIFY = 15 * 60;

	let { current, longest, days }: { current: number; longest: number; days: Day[] } = $props();

	const last14 = $derived(days.slice(-14));
	const hit = $derived(last14.filter((d) => d.credited_seconds >= QUALIFY).length);
</script>

<Panel title="Streak" meta="15m counted a day">
	<div class="flex flex-1 flex-col justify-between gap-[18px] pt-1.5">
		<div class="flex items-baseline gap-2.5">
			<span class="px fig">{current}</span>
			<span class="text-lg text-ink-2">{current === 1 ? 'day' : 'days'}</span>
		</div>
		<div>
			<div
				class="cells"
				role="img"
				aria-label="Last 14 days: {hit} {hit === 1 ? 'day' : 'days'} reached 15 counted minutes"
			>
				{#each last14 as d, i (d.date)}
					<i
						class:on={d.credited_seconds >= QUALIFY}
						class:some={d.credited_seconds > 0 && d.credited_seconds < QUALIFY}
						class:today={i === last14.length - 1}
						title="{dayMonth(d.date)} · {duration(d.credited_seconds)} counted"
					></i>
				{/each}
			</div>
			{#if last14.length}
				<div class="lbl mt-2 flex justify-between">
					<span>{dayMonth(last14[0].date)}</span><span>today</span>
				</div>
			{/if}
		</div>
		<div class="mute">
			Best: <span class="text-ink">{longest} {longest === 1 ? 'day' : 'days'}</span>
		</div>
	</div>
</Panel>

<style>
	.fig {
		font-size: var(--text-2xl);
		line-height: 0.9;
	}
	.cells {
		display: grid;
		grid-template-columns: repeat(14, 1fr);
		gap: 4px;
	}
	.cells i {
		height: 22px;
		background: var(--color-fill);
	}
	.cells i.some {
		background: color-mix(in srgb, var(--color-ink) 25%, var(--color-fill));
	}
	.cells i.on {
		background: var(--color-ink-2);
	}
	.cells i.today.on {
		background: var(--color-ink);
	}
	.cells i.today {
		outline: 1px solid var(--color-accent);
		outline-offset: 2px;
	}
</style>

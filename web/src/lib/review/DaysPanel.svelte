<!--
  The review page's DAYS panel: one row per UTC day of the range, counted
  and at-the-keyboard time on the right, the day's 1,440 minutes as a strip
  under them, and commits as ticks above it. One scale and one legend for
  every strip, at the top.
-->
<script lang="ts">
	import type { DayView } from '#lib/review.ts';
	import { KIND_NAMES, stripTotals, type MinuteKind } from '#lib/strip.ts';
	import MinuteStrip from '#lib/components/MinuteStrip.svelte';
	import Panel from '#lib/components/Panel.svelte';

	let { days }: { days: DayView[] } = $props();

	const ORDER: MinuteKind[] = ['y', 'a', 'c', 'i', 'u', 'm'];
	// The legend lists only the kinds these strips use (cut and idle share a swatch).
	const legend = $derived.by(() => {
		const used: Record<MinuteKind, number> = { y: 0, a: 0, c: 0, i: 0, u: 0, m: 0 };
		for (const d of days) {
			const t = stripTotals(d.strip);
			for (const k of ORDER) used[k] += t[k];
		}
		return ORDER.filter((k) => used[k] > 0 && !(k === 'i' && used.c > 0)).map((k) => ({
			kind: k,
			name: k === 'c' ? 'cut or idle' : KIND_NAMES[k]
		}));
	});
	const scale = ['00:00', '06:00', '12:00', '18:00', '24:00'];
</script>

<Panel title="Days" meta="{days.length} {days.length === 1 ? 'day' : 'days'} · UTC">
	<div class="lbl legend">
		{#each legend as l (l.kind)}
			<span><i class="sw mk-{l.kind}"></i>{l.name}</span>
		{/each}
	</div>
	<div class="lbl scale" aria-hidden="true">
		{#each scale as s, i (s)}<span style:left="{i * 25}%">{s}</span>{/each}
	</div>
	<ol class="days" role="list">
		{#each days as d (d.day)}
			<li class="day">
				<div class="line">
					<span class="mono">{d.label}</span>
					<span class="mono nums">
						{d.counted} <span class="mute">counted</span> · {d.keyboard}
						<span class="mute">at the keyboard</span>
					</span>
				</div>
				{#if d.gaps.length}
					<p class="lbl">No signed record from {d.gaps.join(', ')} this day.</p>
				{/if}
				<MinuteStrip strip={d.strip} label="{d.label}, minute by minute (UTC)" ticks={d.ticks} />
			</li>
		{/each}
	</ol>
</Panel>

<style>
	.legend {
		display: flex;
		flex-wrap: wrap;
		gap: 6px 20px;
	}
	/* Each time sits over its point of the strip; the ends stay inside the panel. */
	.scale {
		position: relative;
		height: 22px;
		margin-top: 14px;
		border-bottom: 1px solid var(--color-rule);
	}
	.scale span {
		position: absolute;
		top: 0;
		transform: translateX(-50%);
	}
	.scale span:first-child {
		transform: none;
	}
	.scale span:last-child {
		transform: translateX(-100%);
	}
	.days {
		list-style: none;
		padding: 0;
	}
	.day {
		display: grid;
		gap: 6px;
		padding: 14px 0 10px;
		border-bottom: 1px solid var(--color-rule);
	}
	.day:last-child {
		border-bottom: 0;
		padding-bottom: 0;
	}
	.line {
		display: flex;
		justify-content: space-between;
		align-items: baseline;
		gap: 4px 16px;
		flex-wrap: wrap;
		font-size: var(--text-sm);
	}
	.nums {
		font-size: var(--text-xs);
	}
</style>

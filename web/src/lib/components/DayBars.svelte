<!--
  Bars of counted time (ink) inside a hatched box for time at the keyboard.
  Seven or fewer bars get a label and value each; more get start, middle and
  end labels.
-->
<script lang="ts">
	import type { Bar } from '#lib/bars.ts';
	import { dayMonth, duration } from '#lib/format.ts';

	let { bars, label }: { bars: Bar[]; label: string } = $props();

	const peak = $derived(Math.max(1, ...bars.map((b) => Math.max(b.keyboard, b.counted))));
	const dense = $derived(bars.length > 7);
</script>

<div class="chart" class:dense style:--n={bars.length} role="img" aria-label={label}>
	{#each bars as b (b.key)}
		{@const top = Math.max(b.keyboard, b.counted)}
		<div
			class="col"
			title="{b.title} · {duration(b.counted)} counted, {duration(b.keyboard)} at the keyboard"
		>
			{#if top > 0}
				<div class="box" style:height="{(top / peak) * 100}%">
					<div class="ink" style:height="{(b.counted / top) * 100}%"></div>
				</div>
			{:else}
				<div class="zero"></div>
			{/if}
		</div>
	{/each}
</div>
{#if !dense}
	<div class="labels" style:--n={bars.length}>
		{#each bars as b (b.key)}
			<div>
				<div class="lbl" class:text-ink={b.today}>{b.label}</div>
				<div class="mono" class:lbl={b.counted === 0}>{duration(b.counted)}</div>
			</div>
		{/each}
	</div>
{:else if bars.length}
	<div class="lbl ends">
		<span>{dayMonth(bars[0].key)}</span><span>
			{dayMonth(bars[Math.floor(bars.length / 2)].key)}
		</span><span>
			{bars[bars.length - 1].today ? 'today' : dayMonth(bars[bars.length - 1].key)}
		</span>
	</div>
{/if}
<div class="lbl legend">
	<span><i class="sw bg-ink"></i>counted</span>
	<span><i class="sw hatch"></i>at the keyboard, not counted</span>
</div>

<style>
	.chart,
	.labels {
		display: grid;
		grid-template-columns: repeat(var(--n), minmax(0, 1fr));
		gap: 16px;
	}
	.chart {
		height: 180px;
		align-items: end;
	}
	.chart.dense {
		gap: 3px;
	}
	.col {
		height: 100%;
		display: flex;
		align-items: flex-end;
	}
	.box {
		width: 100%;
		max-width: 44px;
		min-height: 2px;
		background: var(--hatch);
		display: flex;
		align-items: flex-end;
	}
	.ink {
		width: 100%;
		background: var(--color-ink);
	}
	.zero {
		width: 100%;
		max-width: 44px;
		height: 1px;
		background: var(--color-rule);
	}
	.labels {
		margin-top: 12px;
	}
	.ends {
		display: flex;
		justify-content: space-between;
		margin-top: 8px;
	}
	.legend {
		display: flex;
		flex-wrap: wrap;
		gap: 8px 20px;
		margin-top: 14px;
	}
	@media (max-width: 560px) {
		.chart,
		.labels {
			gap: 6px;
		}
		.labels .mono {
			font-size: var(--text-xs);
		}
	}
</style>

<!--
  Bars of counted time (ink) inside a hatched box for time at the keyboard.
  Seven or fewer bars get a label and value each; more get start, middle and
  end labels. Without counting (no device yet), the bars show time at the
  keyboard alone and never mention counted time.
-->
<script lang="ts">
	import type { Bar } from '#lib/bars.ts';
	import { dayMonth, duration } from '#lib/format.ts';

	let {
		bars,
		label,
		counting = true
	}: { bars: Bar[]; label: string; counting?: boolean } = $props();

	const value = (b: Bar) => (counting ? b.counted : b.keyboard);
	const tip = (b: Bar) =>
		counting
			? `${b.title} · ${duration(b.counted)} counted, ${duration(b.keyboard)} at the keyboard`
			: `${b.title} · ${duration(b.keyboard)} at the keyboard`;

	const peak = $derived(Math.max(1, ...bars.map((b) => Math.max(b.keyboard, b.counted))));
	const dense = $derived(bars.length > 7);
</script>

<div class="chart" class:dense style:--n={bars.length} role="img" aria-label={label}>
	{#each bars as b (b.key)}
		{@const top = Math.max(b.keyboard, b.counted)}
		<div class="col" title={tip(b)}>
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
{#if dense}
	<!-- Dense charts show their numbers only on hover; this table gives them to screen readers. -->
	<table class="sr-only">
		<caption>{label}</caption>
		<thead>
			<tr>
				<th>Period</th>
				{#if counting}<th>Counted</th>{/if}<th>At the keyboard</th>
			</tr>
		</thead>
		<tbody>
			{#each bars as b (b.key)}
				<tr>
					<td>{b.title}</td>
					{#if counting}<td>{duration(b.counted)}</td>{/if}<td>{duration(b.keyboard)}</td>
				</tr>
			{/each}
		</tbody>
	</table>
{/if}
{#if !dense}
	<div class="labels" style:--n={bars.length}>
		{#each bars as b (b.key)}
			<div>
				<div class="lbl" class:text-ink={b.today}>{b.label}</div>
				<div class="mono" class:lbl={value(b) === 0}>{duration(value(b))}</div>
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
	{#if counting}
		<span><i class="sw bg-ink"></i>counted</span>
		<span><i class="sw hatch"></i>at the keyboard, not counted</span>
	{:else}
		<span><i class="sw hatch"></i>at the keyboard</span>
	{/if}
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

<!--
  Today: counted time as the hero figure, then how the agent's minutes split
  between you, the agent working alone, and idle (docs/web.md, "Words").
-->
<script lang="ts">
	import { dayLabel, duration } from '#lib/format.ts';
	import type { Today } from '#lib/types.ts';
	import Panel from '#lib/components/Panel.svelte';

	let { today }: { today: Today } = $props();

	const labels = $derived(today.credited_labels ?? { you: 0, agent: 0, idle: 0 });
	const counted = $derived(today.credited_seconds ?? 0);
	const tracked = $derived(labels.you + labels.agent + labels.idle);
	// Cuts that aren't agent or idle (rubber-stamping, loops): "you" minutes that earned less than 60 s.
	const cut = $derived(Math.max(0, labels.you - counted));

	// "The agent saw you working for 36m. It ran on its own for 5m while you
	// were away, and there were 3m with no input; neither counts."
	type Part = string | { b: string };
	const sentence = $derived.by((): Part[] => {
		const out: Part[] = ['The agent saw you working for ', { b: duration(labels.you) }, '.'];
		const away: Part[][] = [];
		if (labels.agent) {
			away.push(['it ran on its own for ', { b: duration(labels.agent) }, ' while you were away']);
		}
		if (labels.idle) away.push(['there were ', { b: duration(labels.idle) }, ' with no input']);
		if (away.length) {
			const joined = away.flatMap((p, i) => (i ? [', and ', ...p] : p));
			const first = joined[0] as string;
			out.push(' ', first[0].toUpperCase() + first.slice(1), ...joined.slice(1));
			out.push(away.length > 1 ? '; neither counts.' : '; that doesn’t count.');
		}
		if (cut >= 60) out.push(' Another ', { b: duration(cut) }, ' was cut by the rules.');
		return out;
	});
</script>

<Panel title="Today" meta={dayLabel(today.end)}>
	<div class="grid gap-[22px] pt-1.5">
		<div class="flex items-baseline gap-3.5">
			<span class="px hero">{duration(counted)}</span>
			<span class="text-lg text-ink-2">counted</span>
		</div>

		{#if tracked === 0}
			<p class="mute text-base">
				Nothing yet today. Counted time shows up here a few minutes after you start working.
			</p>
		{:else}
			<p class="mute max-w-[56ch] text-base">
				{#each sentence as part, i (i)}{#if typeof part === 'string'}{part}{:else}<b>
							{part.b}
						</b>{/if}{/each}
			</p>
			<div>
				<div
					class="split"
					role="img"
					aria-label="You {duration(labels.you)}, agent alone {duration(
						labels.agent
					)}, idle {duration(labels.idle)}"
				>
					{#if labels.you}<span class="bg-ink" style:flex={labels.you}></span>{/if}
					{#if labels.agent}<span class="hatch2" style:flex={labels.agent}></span>{/if}
					{#if labels.idle}<span class="bg-fill" style:flex={labels.idle}></span>{/if}
				</div>
				<div class="legend">
					<span>
						<i class="sw bg-ink"></i>You <span class="mono mute">{duration(labels.you)}</span>
					</span>
					<span>
						<i class="sw hatch2"></i>Agent alone
						<span class="mono mute">{duration(labels.agent)}</span>
					</span>
					<span>
						<i class="sw idle-sw"></i>Idle <span class="mono mute">{duration(labels.idle)}</span>
					</span>
				</div>
			</div>
		{/if}
		<p class="hint">
			Editor plugins logged {duration(today.total_seconds)} at the keyboard today.
		</p>
	</div>
</Panel>

<style>
	.hero {
		font-size: var(--text-3xl);
		line-height: 0.9;
		letter-spacing: -0.03em;
	}
	b {
		font-weight: 400;
		color: var(--color-ink);
	}
	.split {
		display: flex;
		height: 14px;
		gap: 2px;
		background: none;
	}
	.hatch2 {
		background: repeating-linear-gradient(135deg, var(--color-ink-2) 0 2px, transparent 2px 4px);
	}
	.idle-sw {
		background: var(--color-fill);
		outline: 1px solid var(--color-ink-3);
	}
	.legend {
		display: flex;
		flex-wrap: wrap;
		gap: 8px 24px;
		margin-top: 12px;
	}
	@media (max-width: 480px) {
		.hero {
			font-size: 64px;
		}
	}
</style>

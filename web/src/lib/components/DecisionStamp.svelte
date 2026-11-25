<!--
  A recorded decision as a stamp: the verdict in pixel caps on an inverted
  ink block (APPROVED, ADJUSTED 11.1 h, REJECTED), never a coloured pill and
  never the accent (web/DESIGN.md). The large form adds who decided, when
  (UTC) and the reason; the small one is the stamp alone, for list rows.

  Props: decision (#lib/types.ts Decision), size ('lg' | 'sm', default 'lg').
-->
<script lang="ts">
	import { hours, utcStamp } from '#lib/format.ts';
	import type { Decision } from '#lib/types.ts';

	let { decision, size = 'lg' }: { decision: Decision; size?: 'lg' | 'sm' } = $props();

	// Caps written out, not text-transform, so the unit stays "h".
	const word = $derived(
		decision.value === 'adjusted'
			? `ADJUSTED ${hours(decision.approved_seconds ?? 0)}`
			: decision.value === 'approved'
				? 'APPROVED'
				: 'REJECTED'
	);
</script>

{#if size === 'sm'}
	<span class="stamp sm">{word}</span>
{:else}
	<div class="grid justify-items-start gap-3">
		<span class="stamp">{word}</span>
		<p class="lbl">
			by {decision.by ?? 'a deleted account'} · {utcStamp(decision.at)}
		</p>
		{#if decision.reason}
			<div class="grid gap-1">
				<span class="lbl">Reason</span>
				<p class="reason">{decision.reason}</p>
			</div>
		{/if}
	</div>
{/if}

<style>
	.stamp {
		display: inline-block;
		font-family: var(--font-pixel);
		font-variant-numeric: tabular-nums;
		font-size: var(--text-lg);
		line-height: 1;
		letter-spacing: 0.06em;
		color: var(--color-ground);
		background: var(--color-ink);
		padding: 8px 12px 7px;
		white-space: nowrap;
	}
	.stamp.sm {
		font-size: var(--text-xs);
		letter-spacing: 0.08em;
		padding: 3px 6px 2px;
	}
	.reason {
		max-width: 60ch;
		overflow-wrap: anywhere;
		white-space: pre-line;
	}
</style>

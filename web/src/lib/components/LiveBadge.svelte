<!--
  "Tracking now": the one use of the accent. A tick spinner that steps in 8
  frames, still under reduced motion. Doop component "LiveBadge".
-->
<script lang="ts">
	let { label = 'tracking now' }: { label?: string } = $props();
	const ticks = [
		[8, 1.5, 8, 4.5, 1],
		[12.6, 3.4, 10.5, 5.5, 0.85],
		[14.5, 8, 11.5, 8, 0.7],
		[12.6, 12.6, 10.5, 10.5, 0.55],
		[8, 14.5, 8, 11.5, 0.42],
		[3.4, 12.6, 5.5, 10.5, 0.3],
		[1.5, 8, 4.5, 8, 0.2],
		[3.4, 3.4, 5.5, 5.5, 0.12]
	];
</script>

<span class="live" role="status">
	<svg class="spin" viewBox="0 0 16 16" width="14" height="14" aria-hidden="true">
		<g stroke="currentColor" stroke-width="1.4" stroke-linecap="round">
			{#each ticks as [x1, y1, x2, y2, o], i (i)}
				<line {x1} {y1} {x2} {y2} opacity={o} />
			{/each}
		</g>
	</svg>
	<span>{label}</span>
</span>

<style>
	.live {
		display: inline-flex;
		align-items: center;
		gap: 8px;
		font-family: var(--font-pixel);
		font-size: var(--text-xs);
		letter-spacing: 0.08em;
		text-transform: uppercase;
		color: var(--color-accent);
		white-space: nowrap;
	}
	.spin {
		animation: spin 1s steps(8) infinite;
	}
	@keyframes spin {
		to {
			transform: rotate(360deg);
		}
	}
	@media (prefers-reduced-motion: reduce) {
		.spin {
			animation: none;
		}
	}
</style>

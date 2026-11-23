<!--
  A 5×5 mirrored pixel identicon drawn from the username, used instead of
  remote avatar images (docs/web.md, CSP).
-->
<script lang="ts">
	let { seed, size = 20 }: { seed: string; size?: number } = $props();

	// FNV-1a: a stable 32-bit hash; one bit per cell of the left three columns.
	const cells = $derived.by(() => {
		let h = 0x811c9dc5;
		for (const ch of seed) {
			h ^= ch.codePointAt(0) ?? 0;
			h = Math.imul(h, 0x01000193) >>> 0;
		}
		const out: [number, number][] = [];
		for (let y = 0; y < 5; y++) {
			for (let x = 0; x < 3; x++) {
				if ((h >>> (y * 3 + x)) & 1) {
					out.push([x, y]);
					if (x < 2) out.push([4 - x, y]);
				}
			}
		}
		return out;
	});
</script>

<svg
	width={size}
	height={size}
	viewBox="0 0 5 5"
	shape-rendering="crispEdges"
	aria-hidden="true"
	class="flex-none bg-fill"
>
	<g fill="var(--color-ink-2)">
		{#each cells as [x, y] (`${x},${y}`)}
			<rect {x} {y} width="1" height="1" />
		{/each}
	</g>
</svg>

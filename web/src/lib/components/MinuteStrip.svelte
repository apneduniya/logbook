<!--
  A stretch of minutes as one strip, in the review packet's alphabet
  (#lib/strip.ts: y counted, a agent alone, c cut, i idle, u unbacked,
  m mismatch). Short strips (up to two hours, like the signed-out home's
  example hour) draw one cell per minute; longer ones (a day's 1,440) draw
  runs. Ticks above it mark moments such as commits: each draws a 1 px mark,
  and a 24 px button over it shows the detail under the strip on hover, focus
  or tap. Ticks closer than one button share it, and its detail lists each
  of them. The runs are also in a visually hidden table (web/DESIGN.md,
  "Charts").

  Props:
  - strip: one character per minute.
  - label: the strip's accessible name and the hidden table's caption.
  - start: the minute of the day strip[0] falls on (default 0), for clock times.
  - ticks: markers, as #lib/strip.ts Tick (at, text, label, href).
  - tickNoun: what a tick is, for the count line ("commit").
  - tall: the hero size (44 px, 32 px on phones) instead of 20 px.
-->
<script lang="ts">
	import { KIND_NAMES, minuteClock, minuteKind, stripRuns, type Tick } from '#lib/strip.ts';

	interface Props {
		strip: string;
		label: string;
		start?: number;
		ticks?: Tick[];
		tickNoun?: string;
		tall?: boolean;
	}
	let { strip, label, start = 0, ticks = [], tickNoun = 'commit', tall = false }: Props = $props();

	// Leading spaces in markup are trimmed, so the hints keep theirs in strings.
	const HOVER_HINT = ' · point at a tick for details';
	const TAP_HINT = ' · tap a tick for details';

	/** Up to this many minutes, each minute is its own cell. */
	const CELLS_MAX = 120;
	/** A tick button's width and height: the smallest comfortable target. */
	const TARGET = 24;

	const runs = $derived(stripRuns(strip));
	const cells = $derived(strip.length <= CELLS_MAX);
	const cellKinds = $derived(cells ? Array.from(strip, minuteKind) : []);
	const n = $derived(Math.max(1, strip.length));

	const noun = (k: number) => `${k} ${k === 1 ? tickNoun : `${tickNoun}s`}`;
	const count = $derived(noun(ticks.length));

	// Ticks closer than one button (at the strip's drawn width) share a button.
	let width = $state(0);
	const groups = $derived.by(() => {
		const near = width > 0 ? (TARGET / width) * n : 0;
		const sorted = [...ticks].sort((a, b) => a.at - b.at);
		const out: Tick[][] = [];
		for (const t of sorted) {
			const g = out[out.length - 1];
			if (g && t.at - g[g.length - 1].at < near) g.push(t);
			else out.push([t]);
		}
		return out.map((g) => {
			const times = g.map((t) => minuteClock(start + t.at)).join(', ');
			return {
				ticks: g,
				// The button's centre: midway between its first and last tick.
				at: (g[0].at + g[g.length - 1].at) / 2,
				head: g.length > 1 ? `${noun(g.length)} · ${times}` : null,
				label: g.length > 1 ? `${noun(g.length)}. ${g.map((t) => t.label).join('. ')}` : g[0].label
			};
		});
	});

	// The group under the pointer or focus wins over the one pinned by a tap.
	let hovered = $state<number | null>(null);
	let pinned = $state<number | null>(null);
	const shown = $derived(hovered ?? pinned);
	const detail = $derived(shown === null ? null : (groups[shown] ?? null));
	const lit = $derived(new Set(detail?.ticks ?? []));

	function onkeydown(e: KeyboardEvent) {
		if (e.key === 'Escape') {
			pinned = null;
			hovered = null;
		}
	}
</script>

<div class="ms" class:tall bind:clientWidth={width}>
	{#if ticks.length}
		<div class="ticks">
			{#each ticks as t, i (i)}
				<i class="mark" class:lit={lit.has(t)} style:left="{((t.at + 0.5) / n) * 100}%"></i>
			{/each}
			{#each groups as g, i (i)}
				<button
					type="button"
					class="tick"
					style:left="{((g.at + 0.5) / n) * 100}%"
					aria-label={g.label}
					aria-pressed={pinned === i}
					onclick={() => (pinned = pinned === i ? null : i)}
					onpointerenter={() => (hovered = i)}
					onpointerleave={() => (hovered = null)}
					onfocus={() => (hovered = i)}
					onblur={() => (hovered = null)}
					{onkeydown}
				></button>
			{/each}
		</div>
	{/if}

	<div class="strip" class:cells style:--n={n} role="img" aria-label={label}>
		{#if cells}
			{#each cellKinds as k, i (i)}<i class="mk-{k}"></i>{/each}
		{:else}
			{#each runs as r (r.from)}<i class="mk-{r.kind}" style:flex-grow={r.to - r.from}></i>{/each}
		{/if}
	</div>

	{#if ticks.length}
		<p class="detail" aria-hidden={detail === null}>
			{#if detail}
				{#if detail.head}<span class="lbl whole">{detail.head}</span>{/if}
				{#each detail.ticks as t, i (i)}
					<span class="one">
						<span class="mono">{t.text}</span>
						{#if t.href}
							<a class="link" href={t.href} rel="external noopener noreferrer">
								GitHub <span aria-hidden="true">↗</span>
							</a>
						{/if}
					</span>
				{/each}
			{:else}
				<span class="lbl">
					{count}<span class="hover-only">{HOVER_HINT}</span><span class="touch-only">
						{TAP_HINT}
					</span>
				</span>
			{/if}
		</p>
	{/if}

	<!-- A table ignores the 1 px height of .sr-only, so a wrapper carries it. -->
	<div class="sr-only">
		<table>
			<caption>{label}</caption>
			<thead>
				<tr><th>From</th><th>To</th><th>Minutes</th><th>What</th></tr>
			</thead>
			<tbody>
				{#each runs as r (r.from)}
					<tr>
						<td>{minuteClock(start + r.from)}</td>
						<td>{minuteClock(start + r.to)}</td>
						<td>{r.to - r.from}</td>
						<td>{KIND_NAMES[r.kind]}</td>
					</tr>
				{/each}
			</tbody>
		</table>
	</div>
</div>

<style>
	.ms {
		--h: 20px;
		min-width: 0;
	}
	.ms.tall {
		--h: 44px;
	}
	.strip {
		display: flex;
		height: var(--h);
	}
	.strip i {
		flex-basis: 0;
		min-width: 0;
	}
	/* One cell per minute with a hairline of ground between, like a ruler. */
	.strip.cells {
		display: grid;
		grid-template-columns: repeat(var(--n), minmax(0, 1fr));
		column-gap: 3px;
	}
	/* A lone idle cell needs its edge to read as a minute. */
	.strip.cells .mk-i,
	.strip.cells .mk-c {
		box-shadow: inset 0 0 0 1px var(--color-ink-3);
	}
	.ticks {
		position: relative;
		height: 12px;
	}
	/* Each tick's 1 px mark; it grows (by transform) while its button is lit. */
	.mark {
		position: absolute;
		bottom: 0;
		width: 1px;
		height: 12px;
		background: var(--color-ink-2);
		transform: scaleY(0.75);
		transform-origin: bottom;
		transition:
			background 150ms ease-out,
			transform 150ms ease-out;
	}
	.mark.lit {
		background: var(--color-ink);
		transform: scaleY(1);
	}
	/* The 24 px hit area reaches down over the strip, not up over the line
	   above, so the tick row stays 12 px. */
	.tick {
		position: absolute;
		top: 0;
		width: 24px;
		height: 24px;
		transform: translateX(-50%);
		padding: 0;
		border: 0;
		background: none;
		cursor: pointer;
	}
	.tick:focus-visible {
		outline-offset: 0;
	}
	.detail {
		display: flex;
		flex-wrap: wrap;
		gap: 4px 14px;
		align-items: baseline;
		min-height: 20px;
		margin-top: 6px;
		font-size: var(--text-xs);
	}
	.detail .whole {
		flex-basis: 100%;
	}
	.detail .one {
		display: inline-flex;
		flex-wrap: wrap;
		gap: 4px 14px;
		align-items: baseline;
	}
	.touch-only {
		display: none;
	}
	@media (hover: none) {
		.hover-only {
			display: none;
		}
		.touch-only {
			display: inline;
		}
	}
	@media (max-width: 900px) {
		.ms.tall {
			--h: 32px;
		}
		.strip.cells {
			column-gap: 1px;
		}
	}
</style>

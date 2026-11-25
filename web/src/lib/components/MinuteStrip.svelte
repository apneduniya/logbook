<!--
  A stretch of minutes as one strip, in the review packet's alphabet
  (#lib/strip.ts: y counted, a agent alone, c cut, i idle, u unbacked,
  m mismatch). Short strips (up to two hours, like the signed-out home's
  example hour) draw one cell per minute; longer ones (a day's 1,440) draw
  runs. Ticks above it mark moments such as commits: each is a button whose
  detail shows under the strip on hover, focus or tap. The runs are also in
  a visually hidden table (web/DESIGN.md, "Charts").

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

	const runs = $derived(stripRuns(strip));
	const cells = $derived(strip.length <= CELLS_MAX);
	const cellKinds = $derived(cells ? Array.from(strip, minuteKind) : []);
	const n = $derived(Math.max(1, strip.length));

	// The tick under the pointer or focus wins over the one pinned by a tap.
	let hovered = $state<number | null>(null);
	let pinned = $state<number | null>(null);
	const shown = $derived(hovered ?? pinned);
	const detail = $derived(shown === null ? null : (ticks[shown] ?? null));
	const count = $derived(`${ticks.length} ${ticks.length === 1 ? tickNoun : `${tickNoun}s`}`);

	function onkeydown(e: KeyboardEvent) {
		if (e.key === 'Escape') {
			pinned = null;
			hovered = null;
		}
	}
</script>

<div class="ms" class:tall>
	{#if ticks.length}
		<div class="ticks">
			{#each ticks as t, i (i)}
				<button
					type="button"
					class="tick"
					style:left="{((t.at + 0.5) / n) * 100}%"
					aria-label={t.label}
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
				<span class="mono">{detail.text}</span>
				{#if detail.href}
					<a class="link" href={detail.href} rel="external noopener noreferrer">
						GitHub <span aria-hidden="true">↗</span>
					</a>
				{/if}
			{:else}
				<span class="lbl">
					{count}<span class="hover-only">{HOVER_HINT}</span><span class="touch-only">
						{TAP_HINT}
					</span>
				</span>
			{/if}
		</p>
	{/if}

	<table class="sr-only">
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
	/* The hit area is wider than the 1 px mark it draws. */
	.tick {
		position: absolute;
		bottom: 0;
		width: 16px;
		height: 16px;
		transform: translateX(-50%);
		padding: 0;
		border: 0;
		background: none;
		cursor: pointer;
	}
	.tick::after {
		content: '';
		position: absolute;
		left: 50%;
		bottom: 2px;
		width: 1px;
		height: 9px;
		background: var(--color-ink-2);
		transition:
			background 150ms ease-out,
			height 150ms ease-out;
	}
	.tick:hover::after,
	.tick:focus-visible::after,
	.tick[aria-pressed='true']::after {
		background: var(--color-ink);
		height: 12px;
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

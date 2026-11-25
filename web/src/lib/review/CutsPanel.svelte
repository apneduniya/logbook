<!--
  The review page's CUTS AND FLAGS panel, as invoice line items: the rule
  in plain words with its amount on the right, and where it happened under
  it (UTC ranges), then the rule's code from STRUGGLE-RULES.md, quiet at the
  end. Cuts took time off; flags are for the reviewer and took nothing off.
  Long range lists show their first few, then "…" and "Show all".
-->
<script lang="ts">
	import type { LineItem } from '#lib/review.ts';
	import Panel from '#lib/components/Panel.svelte';

	let { cuts, flags }: { cuts: LineItem[]; flags: LineItem[] } = $props();

	const FIRST = 4;
	let all = $state<Record<string, boolean>>({});
	const groups = $derived(
		[
			{ key: 'cut', head: 'Cut', note: 'taken off the counted time', items: cuts, minus: true },
			{
				key: 'flag',
				head: 'Flagged',
				note: 'for you to judge; nothing taken off',
				items: flags,
				minus: false
			}
		].filter((g) => g.items.length)
	);
</script>

<Panel
	title="Cuts and flags"
	meta="{cuts.length} {cuts.length === 1 ? 'cut' : 'cuts'} · {flags.length} {flags.length === 1
		? 'flag'
		: 'flags'}"
	padded={!groups.length}
>
	{#if !groups.length}
		<p class="hint">Nothing was cut or flagged.</p>
	{:else}
		{#each groups as g (g.key)}
			<section aria-label={g.head}>
				<p class="lbl gh">{g.head} <span class="mute">· {g.note}</span></p>
				<ul role="list">
					{#each g.items as it (it.code)}
						{@const key = `${g.key}:${it.code}`}
						{@const shown = all[key] ? it.ranges : it.ranges.slice(0, FIRST)}
						<li class="item">
							<div class="line">
								<span class="rule">{it.rule}</span>
								<span class="mono amt">
									{g.minus ? '−' : ''}{it.amount}
									<span class="sr-only">{g.minus ? 'cut' : 'flagged'}</span>
								</span>
							</div>
							<p class="lbl sub">
								{#if shown.length}
									{shown.join(', ')}{shown.length < it.ranges.length ? ', …' : ''} ·
								{/if}
								<span class="code">{it.code}</span>
								{#if it.ranges.length > FIRST}
									<button
										class="link more"
										type="button"
										aria-expanded={!!all[key]}
										onclick={() => (all[key] = !all[key])}
									>
										{all[key] ? 'Show fewer' : `Show all ${it.ranges.length}`}
									</button>
								{/if}
							</p>
						</li>
					{/each}
				</ul>
			</section>
		{/each}
	{/if}
</Panel>

<style>
	ul {
		list-style: none;
		padding: 0;
	}
	.gh {
		padding: 10px 14px;
		border-bottom: 1px solid var(--color-rule);
		text-transform: uppercase;
		letter-spacing: 0.08em;
	}
	.gh .mute {
		text-transform: none;
		letter-spacing: 0.04em;
		color: var(--color-ink-3);
	}
	section + section .gh {
		border-top: 1px solid var(--color-rule);
		margin-top: -1px;
	}
	.item {
		display: grid;
		gap: 4px;
		padding: 12px 14px;
		border-bottom: 1px solid var(--color-rule);
	}
	section:last-child .item:last-child {
		border-bottom: 0;
	}
	.line {
		display: grid;
		grid-template-columns: minmax(0, 1fr) auto;
		gap: 16px;
		align-items: baseline;
	}
	.rule {
		max-width: 60ch;
	}
	.amt {
		white-space: nowrap;
		text-align: right;
	}
	.sub {
		line-height: 1.6;
	}
	.code {
		color: var(--color-ink-3);
	}
	/* A button, so the sans voice: it must not read as one more range. */
	.more {
		margin-left: 8px;
		background: none;
		border: 0;
		padding: 0;
		cursor: pointer;
		font-family: var(--font-sans);
		font-size: var(--text-xs);
		letter-spacing: 0;
	}
</style>

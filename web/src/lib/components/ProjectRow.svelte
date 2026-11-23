<!--
  A clickable project row: name, time at the keyboard, counted time, chevron.
  `live` marks the project being tracked now (accent edge). Doop "ProjectRow".
-->
<script lang="ts">
	interface Props {
		href: string;
		name: string;
		keyboard: string;
		counted: string;
		live?: boolean;
		/** A small note after the name, e.g. "hidden". */
		note?: string;
	}
	let { href, name, keyboard, counted, live = false, note }: Props = $props();
	// "—" means nothing is counted yet (no device): phones then keep the
	// keyboard column, the only real number, and drop the counted one.
	const raw = $derived(counted === '—');
</script>

<!-- eslint-disable-next-line svelte/no-navigation-without-resolve -- callers pass a resolve() result -->
<a class="pr" class:raw {href} data-live={live || undefined}>
	<span class="n">
		<span class="name">{name}</span>{#if note}<span class="note">{note}</span>{/if}
	</span>
	<span class="kb">{keyboard}<span class="sr-only"> at the keyboard</span></span>
	<span class="ct">
		{#if counted === '—'}<span aria-hidden="true">—</span><span class="sr-only">
				not counted
			</span>{:else}{counted}<span class="sr-only"> counted</span>{/if}
	</span>
	<svg class="go" width="7" height="11" viewBox="0 0 7 11" aria-hidden="true">
		<path d="M1 1l4.5 4.5L1 10" fill="none" stroke="currentColor" stroke-width="1.4" />
	</svg>
</a>

<style>
	.pr {
		display: grid;
		grid-template-columns: minmax(0, 1fr) 72px 72px 16px;
		align-items: center;
		gap: 16px;
		min-height: 44px;
		padding: 0 14px;
		border-bottom: 1px solid var(--color-rule);
		color: var(--color-ink);
		text-decoration: none;
		font-size: var(--text-base);
		transition: background 150ms ease-out;
	}
	.n {
		display: flex;
		align-items: baseline;
		min-width: 0;
	}
	.name {
		white-space: nowrap;
		overflow: hidden;
		text-overflow: ellipsis;
		min-width: 0;
	}
	.note {
		flex: none;
		white-space: nowrap;
		margin-left: 10px;
		font-family: var(--font-mono);
		font-size: var(--text-xs);
		letter-spacing: 0.04em;
		color: var(--color-ink-3);
	}
	.kb,
	.ct {
		font-family: var(--font-mono);
		font-size: var(--text-sm);
		text-align: right;
		white-space: nowrap;
		font-variant-numeric: tabular-nums;
	}
	.kb {
		color: var(--color-ink-3);
	}
	.go {
		color: var(--color-ink-3);
		justify-self: end;
		transition:
			transform 150ms ease-out,
			color 150ms ease-out;
	}
	.pr:hover {
		background: var(--color-fill);
	}
	.pr:hover .go {
		color: var(--color-ink);
		transform: translateX(2px);
	}
	.pr:focus-visible {
		outline: 1px solid var(--color-ink);
		outline-offset: -1px;
	}
	.pr[data-live] {
		box-shadow: inset 2px 0 0 var(--color-accent);
	}
	@media (max-width: 480px) {
		.pr {
			grid-template-columns: minmax(0, 1fr) 64px 16px;
			gap: 12px;
		}
		.kb {
			display: none;
		}
		.raw .kb {
			display: block;
		}
		.raw .ct {
			display: none;
		}
	}
</style>

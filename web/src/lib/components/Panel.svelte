<!--
  A hairline HUD panel: a head with the title in pixel caps and optional meta
  on the right, then the body. Doop component "PanelHead" (tab off).
  `heading={false}` sets the title as a paragraph with the same look and
  label, for a page's first panel whose h1 is inside it, so the outline
  starts with the h1.
-->
<script lang="ts">
	import type { Snippet } from 'svelte';

	interface Props {
		title: string;
		/** Right-aligned meta in the head, e.g. a date range. */
		meta?: string;
		/** Replaces `meta` with custom head content (a control). */
		head?: Snippet;
		/** Extra classes on the section (grid placement). */
		class?: string;
		/** An anchor id for links like /settings#devices. */
		anchor?: string;
		/** Pad the body (default true). Lists that run edge to edge pass false. */
		padded?: boolean;
		/** Set the title as an h2 (default) or, with false, a plain paragraph. */
		heading?: boolean;
		children: Snippet;
	}

	let {
		title,
		meta,
		head,
		class: cls = '',
		anchor,
		padded = true,
		heading = true,
		children
	}: Props = $props();
	const id = $props.id();
</script>

<section class="panel {cls}" id={anchor} aria-labelledby={id}>
	<header class="ph">
		{#if heading}
			<h2 class="t" {id}>{title}</h2>
		{:else}
			<p class="t" {id}>{title}</p>
		{/if}
		{#if head}
			{@render head()}
		{:else if meta}
			<span class="m">{meta}</span>
		{/if}
	</header>
	<div class={padded ? 'b' : 'flex flex-1 flex-col'}>
		{@render children()}
	</div>
</section>

<style>
	.panel {
		border: 1px solid var(--color-rule);
		display: flex;
		flex-direction: column;
		min-width: 0;
	}
	.ph {
		display: flex;
		align-items: center;
		gap: 10px;
		height: 36px;
		padding: 0 14px;
		border-bottom: 1px solid var(--color-rule);
		font-family: var(--font-pixel);
		font-size: var(--text-xs);
		letter-spacing: 0.08em;
		text-transform: uppercase;
	}
	.t {
		flex: 1;
		font: inherit;
		color: var(--color-ink);
		white-space: nowrap;
		overflow: hidden;
		text-overflow: ellipsis;
	}
	.m {
		color: var(--color-ink-3);
		white-space: nowrap;
	}
	.b {
		padding: 18px 16px;
		flex: 1;
		display: flex;
		flex-direction: column;
	}
</style>

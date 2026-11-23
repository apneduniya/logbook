<script lang="ts">
	import './layout.css';
	import { app } from '#lib/app.svelte.ts';
	import { ago } from '#lib/format.ts';
	import Header from '#lib/components/Header.svelte';
	import LiveBadge from '#lib/components/LiveBadge.svelte';
	import PublicHeader from '#lib/components/PublicHeader.svelte';
	import type { LayoutProps } from './$types';

	let { children }: LayoutProps = $props();

	$effect(() => app.startLive());

	const lastSeen = $derived(app.agentLastSeen);
</script>

<svelte:head>
	<title>Logbook</title>
	<meta
		name="description"
		content="Logbook counts the hours you actually worked, with or without AI, and shows a reviewer why each minute counted."
	/>
</svelte:head>

<a class="skip" href="#main">Skip to content</a>

<div class="wrap">
	{#if app.session}
		<Header>
			{#snippet status()}
				{#if app.live}
					<LiveBadge />
				{:else if app.devices && app.devices.every((d) => d.revoked_at)}
					<span class="lbl status">Agent not connected</span>
				{:else if lastSeen}
					<span class="lbl status">Agent seen {ago(lastSeen, new Date(app.now))}</span>
				{/if}
			{/snippet}
		</Header>
	{:else}
		<PublicHeader />
	{/if}

	{#if app.sessionError}
		<p class="lbl banner" role="alert">{app.sessionError}</p>
	{/if}

	<main id="main">
		{@render children()}
	</main>
</div>

<style>
	.wrap {
		max-width: 1440px;
		margin: 0 auto;
		padding: 16px 28px 0;
		display: flex;
		flex-direction: column;
		min-height: 100dvh;
	}
	main {
		flex: 1;
		display: flex;
		flex-direction: column;
		padding-top: 12px;
	}
	.skip {
		position: absolute;
		left: 8px;
		top: -40px;
		padding: 6px 10px;
		background: var(--color-ink);
		color: var(--color-ground);
		z-index: 10;
	}
	.skip:focus {
		top: 8px;
	}
	.banner {
		padding: 10px 0;
		border-bottom: 1px solid var(--color-rule);
		color: var(--color-ink);
	}
	@media (max-width: 720px) {
		.wrap {
			padding: 14px 16px 0;
		}
	}
</style>

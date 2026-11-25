<!--
  The apps the user owns or reviews for, as big rows on hairlines: the
  slug's identicon, the name over a mono line (slug and role), and how many
  submissions wait for review. "Register an app" opens the form.
-->
<script lang="ts">
	import { resolve } from '$app/paths';
	import { app } from '#lib/app.svelte.ts';
	import { ApiError } from '#lib/api.ts';
	import { inboxQuery, pendingLabel } from '#lib/apps.ts';
	import Identicon from '#lib/components/Identicon.svelte';
	import Panel from '#lib/components/Panel.svelte';
	import type { PageProps } from './$types';

	let { data }: PageProps = $props();

	const owned = $derived(data.apps.filter((a) => a.role === 'owner').length);

	// Waiting counts, one inbox page each, loaded after the list is on screen.
	let pending = $state<Record<string, number>>({});
	let pendingError = $state<string | null>(null);
	$effect(() => {
		const ids = data.apps.map((a) => a.id);
		const ctl = new AbortController();
		pendingError = null;
		for (const id of ids) {
			app.api
				.appSubmissions(id, inboxQuery('review'), ctl.signal)
				.then((rows) => (pending[id] = rows.length))
				.catch((err) => {
					if (ctl.signal.aborted) return;
					pendingError =
						err instanceof ApiError
							? `Couldn’t count what waits for review. ${err.userMessage}`
							: 'Couldn’t count what waits for review.';
				});
		}
		return () => ctl.abort();
	});
</script>

<svelte:head><title>Apps · Logbook</title></svelte:head>

<h1 class="sr-only">Apps</h1>
<div class="stack">
	<Panel title="Apps" meta={data.apps.length ? String(data.apps.length) : undefined} padded={false}>
		{#if data.apps.length === 0}
			<div class="empty">
				<p class="px">( NO APPS YET )</p>
				<p class="hint">
					An app lets a program sign people in with Logbook, read the hours they allow, and review
					the hours they submit. If someone adds you as a reviewer, their app shows up here too.
				</p>
				<a class="btn mt-2 justify-self-start" href={resolve('/apps/new')}>Register an app</a>
			</div>
		{:else}
			<ul class="rows">
				{#each data.apps as a (a.id)}
					{@const n = pending[a.id]}
					<li>
						<a class="row" href={resolve('/apps/[id]', { id: a.id })}>
							<Identicon seed={a.slug} size={28} />
							<span class="main">
								<span class="name">{a.name}</span>
								<span class="lbl dl">
									{a.slug} · {a.role === 'owner'
										? 'yours'
										: `reviewer for ${a.owner_username}`}{a.public
										? ''
										: ' · not taking submissions'}
								</span>
							</span>
							<span class="mono count" class:quiet={!n}>
								{n === undefined ? '' : pendingLabel(n)}
							</span>
							<svg class="go" width="7" height="11" viewBox="0 0 7 11" aria-hidden="true">
								<path d="M1 1l4.5 4.5L1 10" fill="none" stroke="currentColor" stroke-width="1.4" />
							</svg>
						</a>
					</li>
				{/each}
			</ul>
			<div class="foot">
				{#if pendingError}<p class="hint text-ink" role="alert">{pendingError}</p>{/if}
				{#if owned >= 10}
					<p class="hint">
						You own 10 apps, the most one person can. Delete one to register another.
					</p>
				{:else}
					<a class="btn justify-self-start" href={resolve('/apps/new')}>Register an app</a>
				{/if}
			</div>
		{/if}
	</Panel>
</div>

<style>
	.stack {
		display: grid;
		gap: 12px;
		max-width: calc((100% - 12px) * 8 / 12);
		padding-bottom: 32px;
	}
	.empty {
		display: grid;
		gap: 8px;
		padding: 22px 16px;
	}
	.empty .px {
		font-size: var(--text-sm);
		letter-spacing: 0.06em;
		color: var(--color-ink-2);
	}
	.rows {
		list-style: none;
		padding: 0;
	}
	.row {
		display: grid;
		grid-template-columns: 28px minmax(0, 1fr) auto 16px;
		align-items: center;
		gap: 14px;
		min-height: 60px;
		padding: 10px 14px;
		border-bottom: 1px solid var(--color-rule);
		color: var(--color-ink);
		text-decoration: none;
		transition: background 150ms ease-out;
	}
	.row:hover {
		background: var(--color-fill);
	}
	.row:focus-visible {
		outline: 1px solid var(--color-ink);
		outline-offset: -1px;
	}
	.main {
		display: grid;
		gap: 3px;
		min-width: 0;
	}
	.name {
		font-size: var(--text-base);
		white-space: nowrap;
		overflow: hidden;
		text-overflow: ellipsis;
	}
	.dl {
		white-space: nowrap;
		overflow: hidden;
		text-overflow: ellipsis;
	}
	.count {
		font-size: var(--text-sm);
		white-space: nowrap;
		text-align: right;
	}
	.count.quiet {
		color: var(--color-ink-3);
	}
	.go {
		color: var(--color-ink-3);
	}
	.foot {
		display: grid;
		gap: 10px;
		padding: 14px;
	}
	@media (max-width: 900px) {
		.stack {
			max-width: none;
		}
	}
	@media (max-width: 480px) {
		.row {
			grid-template-columns: 28px minmax(0, 1fr) 16px;
			row-gap: 4px;
		}
		.count {
			grid-column: 2;
			grid-row: 2;
			text-align: left;
		}
	}
</style>

<!--
  An app as a record page (Linear's app page, docs/web.md "Apps"): the
  title line, then FACTS (what the app is) beside CREDENTIALS (how it
  authenticates) on the 8/4 grid, the INBOX across the page, and REVIEWERS
  with the owner's Delete under it. Owner-only actions are hidden for
  reviewers, not disabled. On phones it is one column, and reviewers get
  the inbox first, since it is what they come for.
-->
<script lang="ts">
	import { goto, invalidate } from '$app/navigation';
	import { resolve } from '$app/paths';
	import { untrack } from 'svelte';
	import { app } from '#lib/app.svelte.ts';
	import { APPS, formOf, shortUrl, toPatch, type AppForm } from '#lib/apps.ts';
	import { dateRange } from '#lib/format.ts';
	import { takeSecret } from '#lib/secret.ts';
	import AppFormView from '#lib/apps/AppForm.svelte';
	import CredentialsPanel from '#lib/apps/CredentialsPanel.svelte';
	import InboxPanel from '#lib/apps/InboxPanel.svelte';
	import ReviewersPanel from '#lib/apps/ReviewersPanel.svelte';
	import ConfirmButton from '#lib/components/ConfirmButton.svelte';
	import Identicon from '#lib/components/Identicon.svelte';
	import Panel from '#lib/components/Panel.svelte';
	import type { PageProps } from './$types';

	let { data }: PageProps = $props();

	const a = $derived(data.app);
	const owner = $derived(a.role === 'owner');
	const created = $derived(dateRange(a.created_at.slice(0, 10), a.created_at.slice(0, 10)));
	const homepage = $derived(/^https?:\/\//.test(a.homepage_url) ? a.homepage_url : '');

	// The one-time secret: handed over by the register page, or from a rotate.
	// Held in this component only, so leaving the page drops it for good.
	let fresh = $state<{ id: string; secret: string; rotated: boolean } | null>(null);
	let editing = $state(false);
	let saved = $state<string | null>(null);

	$effect.pre(() => {
		const id = a.id;
		untrack(() => {
			if (fresh && fresh.id !== id) fresh = null;
			const s = takeSecret(id);
			if (s) fresh = { id, secret: s, rotated: false };
			if (editing && fresh?.id !== id) editing = false;
		});
	});
	const secret = $derived(fresh && fresh.id === a.id ? fresh : null);

	async function rotate() {
		const id = a.id;
		const r = await app.api.rotateAppSecret(id);
		// Another app may be on screen by now: never show this secret there.
		if (r.client_secret && a.id === id) fresh = { id, secret: r.client_secret, rotated: true };
		await invalidate(APPS);
	}

	async function save(f: AppForm) {
		const patch = toPatch(a, f);
		if (patch) {
			await app.api.updateApp(a.id, patch);
			await invalidate(APPS);
		}
		editing = false;
		saved = patch ? 'Saved.' : 'Nothing changed.';
	}

	async function remove() {
		await app.api.deleteApp(a.id);
		await goto(resolve('/apps'), { invalidate: [APPS] });
	}
</script>

<svelte:head><title>{a.name} · Apps · Logbook</title></svelte:head>

<div class="head">
	<a class="link lbl" href={resolve('/apps')}>← Apps</a>
	<div class="title">
		<Identicon seed={a.slug} size={40} />
		<h1 class="px">{a.name}</h1>
	</div>
	<p class="mute">
		Created {created} by {a.owner_username} · {owner ? 'you own it' : 'you review for it'}
	</p>
</div>

<div class="page" class:reviewer={!owner}>
	<Panel title="Facts" class="facts">
		{#if editing}
			<AppFormView
				mode="edit"
				initial={formOf(a)}
				submit="Save changes"
				run={save}
				oncancel={() => (editing = false)}
			/>
		{:else}
			<dl class="meta">
				<dt class="lbl">Slug</dt>
				<dd class="mono">{a.slug}</dd>
				<dt class="lbl">Homepage</dt>
				<dd>
					{#if homepage}
						<a class="link" href={homepage} rel="external noopener noreferrer">
							{shortUrl(homepage)} →
						</a>
					{:else}
						<span class="mute">None</span>
					{/if}
				</dd>
				<dt class="lbl">Redirect URIs</dt>
				<dd>
					<ul class="uris">
						{#each a.redirect_uris as u (u)}
							{@const parts = u.split('/')}
							<!-- Breaks only after a slash, so a path never splits mid-word. -->
							<li class="mono">
								{#each parts as part, i (i)}{part}{#if i < parts.length - 1}/<wbr />{/if}{/each}
							</li>
						{/each}
					</ul>
				</dd>
				<dt class="lbl">Program</dt>
				<dd>
					{a.public
						? 'Takes submissions: people can send it their hours.'
						: 'Doesn’t take submissions. Sign-in still works.'}
				</dd>
				<dt class="lbl">Client</dt>
				<dd>{a.confidential ? 'Confidential, with a secret' : 'Public, no secret (PKCE)'}</dd>
			</dl>
			{#if owner}
				<div class="mt-5 flex flex-wrap items-center gap-4">
					<button
						class="btn"
						type="button"
						onclick={() => {
							saved = null;
							editing = true;
						}}
					>
						Edit
					</button>
					{#if saved}<span class="hint" role="status">{saved}</span>{/if}
				</div>
			{/if}
		{/if}
	</Panel>

	<div class="creds">
		<CredentialsPanel
			app={a}
			secret={secret?.secret ?? null}
			rotated={secret?.rotated ?? false}
			onrotate={rotate}
		/>
	</div>

	<div class="inbox">
		<InboxPanel appId={a.id} slug={a.slug} />
	</div>

	<div class="reviewers">
		<ReviewersPanel app={a} members={data.members} onchange={() => invalidate(APPS)} />
	</div>

	{#if owner}
		<Panel title="Delete app" class="del">
			<p class="hint">
				Every sign-in token and grant stops working at once, and the app leaves the submit list.
				Submissions keep its name. Nobody can register the slug {a.slug} again.
			</p>
			<div class="mt-4">
				<ConfirmButton
					label="Delete app"
					ask="Delete {a.name}? This can’t be undone."
					confirm="Delete"
					align="start"
					run={remove}
				/>
			</div>
		</Panel>
	{/if}
</div>

<style>
	.head {
		display: grid;
		gap: 8px;
		padding: 12px 0 20px;
	}
	.title {
		display: flex;
		align-items: center;
		gap: 14px;
		min-width: 0;
	}
	h1 {
		font-weight: 400;
		font-size: var(--text-xl);
		line-height: 1.05;
		letter-spacing: -0.01em;
		overflow-wrap: anywhere;
		min-width: 0;
	}

	.page {
		display: grid;
		grid-template-columns: calc((100% - 12px) * 8 / 12) minmax(0, 1fr);
		gap: 12px;
		padding-bottom: 32px;
	}
	.page > :global(.facts),
	.reviewers {
		grid-column: 1;
	}
	.creds,
	.page > :global(.del) {
		grid-column: 2;
	}
	.creds,
	.reviewers,
	.inbox {
		display: flex;
		flex-direction: column;
		min-width: 0;
	}
	.creds > :global(*),
	.reviewers > :global(*) {
		flex: 1;
	}
	.inbox {
		grid-column: 1 / -1;
	}
	.page > :global(.del) {
		align-self: start;
	}

	/* Invoice meta, as on the review page: labels left, values beside them. */
	.meta {
		display: grid;
		grid-template-columns: max-content minmax(0, 1fr);
		gap: 10px 24px;
		align-items: baseline;
	}
	.meta dd {
		overflow-wrap: anywhere;
		min-width: 0;
	}
	.uris {
		list-style: none;
		padding: 0;
		display: grid;
		gap: 4px;
		font-size: var(--text-sm);
	}
	.uris li {
		overflow-wrap: break-word;
	}

	@media (max-width: 900px) {
		.page {
			grid-template-columns: minmax(0, 1fr);
		}
		.page > :global(*) {
			grid-column: 1 / -1 !important;
		}
		/* Reviewers came for the inbox; it goes first. */
		.page.reviewer .inbox {
			order: -1;
		}
	}
	@media (max-width: 560px) {
		.meta {
			grid-template-columns: minmax(0, 1fr);
			gap: 2px;
		}
		.meta dd + dt {
			margin-top: 10px;
		}
	}
</style>

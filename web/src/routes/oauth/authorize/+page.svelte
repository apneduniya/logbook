<!--
  The OAuth consent page (docs/web.md "Consent"): one framed sheet in plenty
  of space, under a bare wordmark, so the choice is the only thing on the
  page. The app's identicon and yours, "<App> by <owner> wants to:", one row
  per scope in the server's words, the host you go back to, then Allow
  (solid ink, the one other primary besides sign-in) and Deny.

  States: signed out (sign in, then come straight back); already granted
  (approve without asking); expired or unknown (a sentence, no buttons); a
  broken app link (?error=, a sentence and no redirect).
-->
<script lang="ts">
	import { resolve } from '$app/paths';
	import { untrack } from 'svelte';
	import { app } from '#lib/app.svelte.ts';
	import { ApiError } from '#lib/api.ts';
	import { safeRedirect, urlHost } from '#lib/apps.ts';
	import { clock } from '#lib/format.ts';
	import Identicon from '#lib/components/Identicon.svelte';
	import Panel from '#lib/components/Panel.svelte';
	import type { PageProps } from './$types';

	let { data }: PageProps = $props();

	const req = $derived(data.state === 'ask' ? data.request : null);
	const host = $derived(req ? urlHost(req.redirect_uri) : '');

	let busy = $state<'allow' | 'deny' | null>(null);
	let leaving = $state(false);
	let gone = $state(false);
	let error = $state<string | null>(null);
	let autoTried = false;

	async function decide(approve: boolean) {
		if (data.state !== 'ask') return;
		const { id, request } = data;
		busy = approve ? 'allow' : 'deny';
		error = null;
		try {
			const to = await app.api.decideConsent(id, approve);
			const safe = safeRedirect(to, request.redirect_uri);
			if (!safe) {
				error = 'Logbook got back an address it won’t send you to. Tell the program.';
				busy = null;
				return;
			}
			leaving = true;
			window.location.assign(safe);
		} catch (err) {
			busy = null;
			if (err instanceof ApiError && err.status === 404) gone = true;
			else error = err instanceof ApiError ? err.userMessage : 'That didn’t work. Try again.';
		}
	}

	// Granted before (for example, signed in after starting): approve without asking.
	$effect(() => {
		if (req?.already_granted && !autoTried) {
			autoTried = true;
			untrack(() => void decide(true));
		}
	});

	const auto = $derived(!!req?.already_granted && !error && !gone);
</script>

<svelte:head>
	<title>{req ? `Allow ${req.app.name}?` : 'Authorize'} · Logbook</title>
	<meta name="robots" content="noindex, nofollow" />
</svelte:head>

<div class="sheet">
	{#if data.state === 'broken'}
		<Panel title="Authorize" meta="link error">
			<h1 class="say">This app’s link is broken. Tell the program.</h1>
			<p class="hint mt-2">
				{data.problem === 'invalid_client'
					? 'Logbook doesn’t know the app it names, or the app was deleted.'
					: 'It asks Logbook to send you to an address the app hasn’t registered, so Logbook won’t send you anywhere.'}
			</p>
		</Panel>
	{:else if data.state === 'missing'}
		<Panel title="Authorize">
			<h1 class="say">There’s nothing to allow at this address.</h1>
			<p class="hint mt-2">
				Apps send you here when they ask to use your Logbook account. Go back to the app and start
				again.
			</p>
		</Panel>
	{:else if data.state === 'signedout'}
		<Panel title="Authorize">
			<h1 class="say">An app wants to use your Logbook account.</h1>
			<p class="hint mt-2">
				Sign in first. You’ll come straight back here to see what it asks for, and choose.
			</p>
			<div class="mt-5">
				<button
					class="cta"
					type="button"
					onclick={() => app.signIn(location.pathname + location.search)}
				>
					Sign in with Hack Club
				</button>
			</div>
		</Panel>
	{:else if data.state === 'expired' || gone}
		<Panel title="Authorize" meta="ended">
			<h1 class="say">This request has ended.</h1>
			<p class="hint mt-2">
				Requests last 10 minutes and can be answered once. Go back to the app and start again.
			</p>
		</Panel>
	{:else if req && (auto || leaving)}
		<Panel title="Authorize">
			<h1 class="say">
				{busy === 'deny'
					? 'Denied.'
					: busy === 'allow' && !auto
						? 'Allowed.'
						: `You’ve allowed ${req.app.name} before.`}
			</h1>
			<p class="px going mt-4" role="status">( SENDING YOU BACK )</p>
			<p class="hint mt-2">Going to <code class="mono host">{host}</code>.</p>
		</Panel>
	{:else if req}
		<Panel title="Authorize" meta="expires {clock(req.expires_at, app.timezone)}">
			<div class="marks" aria-hidden="true">
				<Identicon seed={req.app.slug} size={40} />
				<span class="to">→</span>
				<Identicon seed={app.user?.username ?? ''} size={40} />
			</div>
			<h1 class="say mt-4">
				<strong>{req.app.name}</strong> <span class="mute">by</span>
				{req.app.owner_username} wants to:
			</h1>
			<ul class="scopes">
				{#each req.scopes as s (s.name)}
					<li><span>{s.description}</span><span class="lbl">{s.name}</span></li>
				{/each}
			</ul>
			<p class="hint">Either way, you go back to <code class="mono host">{host}</code>.</p>
			<div class="acts">
				<button class="cta" type="button" onclick={() => decide(true)} disabled={!!busy}>
					{busy === 'allow' ? 'Allowing…' : 'Allow'}
				</button>
				<button class="btn deny" type="button" onclick={() => decide(false)} disabled={!!busy}>
					{busy === 'deny' ? 'Denying…' : 'Deny'}
				</button>
			</div>
			{#if error}<p class="hint err mt-3" role="alert">{error}</p>{/if}
		</Panel>
		<div class="after">
			<p class="hint">
				You’re signed in as <span class="mono">{app.user?.username}</span>. Logbook doesn’t review
				apps, so only allow apps you trust.
			</p>
			<p class="hint">
				You can remove access any time in
				<a class="link under" href="{resolve('/settings')}#apps">Settings → Apps with access</a>.
			</p>
		</div>
	{/if}
</div>

<style>
	/* One sheet in generous space, as on GitHub's and Linear's consent screens. */
	.sheet {
		width: 100%;
		max-width: 520px;
		margin: clamp(24px, 9vh, 96px) auto 48px;
		display: grid;
		gap: 16px;
	}
	.say {
		font-weight: 400;
		font-size: var(--text-lg);
		line-height: 1.3;
		overflow-wrap: anywhere;
	}
	.say strong {
		font-weight: 500;
	}
	.going {
		font-size: var(--text-sm);
		letter-spacing: 0.06em;
		color: var(--color-ink-2);
	}
	.marks {
		display: flex;
		align-items: center;
		gap: 12px;
	}
	.to {
		font-family: var(--font-mono);
		color: var(--color-ink-3);
	}
	.scopes {
		list-style: none;
		padding: 0;
		margin: 16px 0 14px;
		border-top: 1px solid var(--color-rule);
	}
	.scopes li {
		display: flex;
		justify-content: space-between;
		align-items: baseline;
		gap: 16px;
		padding: 11px 0;
		border-bottom: 1px solid var(--color-rule);
		font-size: var(--text-base);
		line-height: 1.4;
	}
	.scopes .lbl {
		flex: none;
	}
	.host {
		color: var(--color-ink);
		overflow-wrap: anywhere;
	}
	.acts {
		display: flex;
		flex-wrap: wrap;
		gap: 10px;
		margin-top: 20px;
	}
	.cta {
		border: 0;
		cursor: pointer;
		font-family: inherit;
	}
	.cta:disabled {
		cursor: progress;
	}
	/* Deny stands beside Allow at the same height, and stays secondary. */
	.deny {
		height: 44px;
		padding: 0 18px;
		font-size: var(--text-base);
	}
	.err {
		color: var(--color-ink);
	}
	.after {
		display: grid;
		gap: 6px;
	}
	.under {
		text-decoration: underline;
		text-decoration-color: var(--color-rule);
		text-underline-offset: 3px;
	}
	@media (max-width: 480px) {
		.acts > * {
			flex: 1;
			justify-content: center;
		}
	}
</style>

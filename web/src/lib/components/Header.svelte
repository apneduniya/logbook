<!--
  The signed-in header: wordmark, section tabs (active one an inverted block),
  the agent's state, and the account menu. The tabs are the work (Today,
  Projects, then Submissions, which are made from projects), then the
  person's own Profile and Settings. On phones the tabs wrap to their own
  row, the Profile tab steps aside unless it is the open page (the account
  menu has it), and the account button shows only the identicon.
-->
<script lang="ts">
	import { resolve } from '$app/paths';
	import type { Snippet } from 'svelte';
	import { goto } from '$app/navigation';
	import { page } from '$app/state';
	import { app } from '#lib/app.svelte.ts';
	import Identicon from './Identicon.svelte';

	let { status }: { status?: Snippet } = $props();

	const user = $derived(app.user!);
	const name = $derived(user.display_name.split(' ')[0] || user.username);

	const tabs = $derived([
		{ href: resolve('/'), label: 'Today', match: (p: string) => p === '/' },
		{
			href: resolve('/projects'),
			label: 'Projects',
			match: (p: string) => p.startsWith('/projects')
		},
		{
			href: resolve('/submissions'),
			label: 'Submissions',
			match: (p: string) => p.startsWith('/submissions')
		},
		{
			href: resolve('/u/[username]', { username: encodeURIComponent(user.username) }),
			label: 'Profile',
			match: (p: string) => p === `/u/${user.username}`,
			// Five tabs don't fit a phone's row; the account menu links the profile.
			wide: true
		},
		{
			href: resolve('/settings'),
			label: 'Settings',
			match: (p: string) => p.startsWith('/settings')
		}
	]);

	let menu = $state<HTMLElement | null>(null);
	let button = $state<HTMLButtonElement | null>(null);
	let signingOut = $state(false);
	let error = $state<string | null>(null);

	// Place the popover under the button, right-aligned.
	function place(e: ToggleEvent) {
		if (e.newState !== 'open' || !button || !menu) return;
		const r = button.getBoundingClientRect();
		menu.style.top = `${r.bottom + 6}px`;
		menu.style.right = `${document.documentElement.clientWidth - r.right}px`;
	}

	async function signOut() {
		signingOut = true;
		error = null;
		try {
			await app.signOut();
			menu?.hidePopover();
			await goto(resolve('/'), { refreshAll: true });
		} catch {
			error = 'Couldn’t sign out. Try again.';
		} finally {
			signingOut = false;
		}
	}
</script>

<header class="hdr">
	<a class="mark px" href={resolve('/')} aria-label="Logbook, today">LOGBOOK</a>
	<nav class="tabs px" aria-label="Sections">
		{#each tabs as t (t.href)}
			{@const current = t.match(page.url.pathname)}
			<a
				class="tab"
				class:wide={'wide' in t && !current}
				href={t.href}
				aria-current={current ? 'page' : undefined}
			>
				{t.label}
			</a>
		{/each}
	</nav>
	<div class="right">
		{@render status?.()}
		<button
			bind:this={button}
			class="acct"
			type="button"
			popovertarget="account-menu"
			aria-label="Account menu for {user.username}"
		>
			<Identicon seed={user.username} />
			<span class="who">{name}</span>
			<svg width="9" height="6" viewBox="0 0 9 6" aria-hidden="true">
				<path d="M1 1l3.5 3.5L8 1" fill="none" stroke="var(--color-ink-3)" stroke-width="1.3" />
			</svg>
		</button>
		<div bind:this={menu} id="account-menu" class="menu" popover ontoggle={place}>
			<div class="lbl head">Signed in as {user.username}</div>
			<a
				href={resolve('/u/[username]', { username: encodeURIComponent(user.username) })}
				onclick={() => menu?.hidePopover()}
			>
				Public profile
			</a>
			<a href={resolve('/settings')} onclick={() => menu?.hidePopover()}>Settings</a>
			<a href={resolve('/apps')} onclick={() => menu?.hidePopover()}>Apps</a>
			<button type="button" onclick={signOut} disabled={signingOut}>
				{signingOut ? 'Signing out…' : 'Sign out'}
			</button>
			{#if error}<p class="lbl err" role="alert">{error}</p>{/if}
		</div>
	</div>
</header>

<style>
	.hdr {
		display: grid;
		grid-template-columns: auto 1fr auto;
		align-items: center;
		gap: 0 28px;
		padding: 6px 0 14px;
		border-bottom: 1px solid var(--color-rule);
	}
	.mark {
		font-size: var(--text-sm);
		letter-spacing: 0.12em;
		color: var(--color-ink);
		text-decoration: none;
	}
	.tabs {
		display: flex;
		gap: 4px;
		font-size: var(--text-xs);
		letter-spacing: 0.08em;
		text-transform: uppercase;
	}
	.tab {
		color: var(--color-ink-2);
		text-decoration: none;
		padding: 5px 9px;
		transition:
			background 150ms ease-out,
			color 150ms ease-out;
	}
	.tab:not([aria-current]):hover {
		color: var(--color-ink);
		background: var(--color-fill);
	}
	.tab[aria-current] {
		color: var(--color-ground);
		background: var(--color-ink);
	}
	.right {
		display: flex;
		gap: 20px;
		align-items: center;
		justify-self: end;
	}
	.acct {
		display: flex;
		align-items: center;
		gap: 8px;
		height: 30px;
		padding: 0 8px 0 4px;
		border: 1px solid var(--color-rule);
		background: none;
		color: var(--color-ink);
		font: inherit;
		font-size: var(--text-sm);
		cursor: pointer;
		transition:
			background 150ms ease-out,
			border-color 150ms ease-out;
	}
	.acct:hover {
		border-color: var(--color-ink-3);
		background: var(--color-fill);
	}
	.menu {
		position: fixed;
		inset: auto;
		margin: 0;
		min-width: 200px;
		padding: 6px 0;
		border: 1px solid var(--color-rule);
		background: var(--color-ground);
		color: var(--color-ink);
	}
	.menu .head {
		padding: 6px 14px 8px;
		border-bottom: 1px solid var(--color-rule);
		margin-bottom: 4px;
	}
	.menu a,
	.menu button {
		display: block;
		width: 100%;
		padding: 7px 14px;
		text-align: left;
		background: none;
		border: 0;
		font: inherit;
		color: var(--color-ink);
		text-decoration: none;
		cursor: pointer;
	}
	.menu a:hover,
	.menu button:hover:not(:disabled) {
		background: var(--color-fill);
	}
	.menu a:focus-visible,
	.menu button:focus-visible {
		outline: 1px solid var(--color-ink);
		outline-offset: -1px;
	}
	.err {
		padding: 4px 14px;
	}

	@media (max-width: 720px) {
		.hdr {
			grid-template-columns: 1fr auto;
			gap: 14px 16px;
		}
		.tabs {
			grid-row: 2;
			grid-column: 1 / -1;
			overflow-x: auto;
		}
		.tab {
			padding: 10px 8px;
		}
		.tab.wide {
			display: none;
		}
		.who,
		.acct svg:last-child {
			display: none;
		}
		.acct {
			padding: 0 4px;
		}
	}
</style>

<script lang="ts">
	import { resolve } from '$app/paths';
	import { goto, invalidate } from '$app/navigation';
	import { page } from '$app/state';
	import { app, ME } from '#lib/app.svelte.ts';
	import { ApiError } from '#lib/api.ts';
	import { ago, dayLabel, todayIn } from '#lib/format.ts';
	import { grantsSentence, scopeWords } from '#lib/grants.ts';
	import Panel from '#lib/components/Panel.svelte';
	import ConfirmButton from '#lib/components/ConfirmButton.svelte';
	import HackatimeStatus from '#lib/components/HackatimeStatus.svelte';
	import Identicon from '#lib/components/Identicon.svelte';
	import InstallCommand from '#lib/components/InstallCommand.svelte';
	import type { PageProps } from './$types';

	let { data }: PageProps = $props();

	const OS: Record<string, string> = { darwin: 'macOS', linux: 'Linux', windows: 'Windows' };
	const zones = Intl.supportedValuesOf('timeZone');
	const browserZone = Intl.DateTimeFormat().resolvedOptions().timeZone;
	const now = $derived(new Date(app.now));
	const day = (t: string) => dayLabel(todayIn(app.timezone, new Date(t)));

	// Profile form: edits a copy that resets when the profile reloads.
	let displayName = $derived(data.profile.display_name);
	let timezone = $derived(data.profile.timezone);
	let isPublic = $derived(data.profile.profile_public);
	let saving = $state(false);
	let profileMsg = $state<{ ok: boolean; text: string } | null>(null);

	async function saveProfile(e: SubmitEvent) {
		e.preventDefault();
		const patch = {
			display_name:
				displayName.trim() !== data.profile.display_name ? displayName.trim() : undefined,
			timezone: timezone !== data.profile.timezone ? timezone : undefined,
			profile_public: isPublic !== data.profile.profile_public ? isPublic : undefined
		};
		if (Object.values(patch).every((v) => v === undefined)) {
			profileMsg = { ok: true, text: 'Nothing changed.' };
			return;
		}
		saving = true;
		profileMsg = null;
		try {
			await app.api.updateProfile(patch);
			await app.loadSession();
			await invalidate(ME);
			profileMsg = { ok: true, text: 'Saved.' };
		} catch (err) {
			profileMsg = { ok: false, text: message(err) };
		} finally {
			saving = false;
		}
	}

	function message(err: unknown): string {
		return err instanceof ApiError ? err.userMessage : 'Something went wrong. Try again.';
	}
	// A row action (revoke, disconnect, unlink). Errors go back to its
	// ConfirmButton, which shows them by the row.
	async function act(fn: () => Promise<void>) {
		await fn();
		await invalidate(ME);
	}

	const activeDevices = $derived(data.devices.filter((d) => !d.revoked_at));
	const hackatimeNotice = $derived(
		{
			linked: 'Hackatime is linked. Your history is being imported.',
			denied: 'Hackatime linking was cancelled.'
		}[page.url.searchParams.get('hackatime') ?? ''] ?? null
	);

	let signingOut = $state(false);
	let signOutError = $state<string | null>(null);
	async function signOut() {
		signingOut = true;
		signOutError = null;
		try {
			await app.signOut();
			await goto(resolve('/'), { refreshAll: true });
		} catch (err) {
			signOutError = message(err);
		} finally {
			signingOut = false;
		}
	}
</script>

<svelte:head><title>Settings · Logbook</title></svelte:head>

<h1 class="sr-only">Settings</h1>
<div class="stack">
	<Panel title="Profile">
		<form class="form" onsubmit={saveProfile}>
			<div class="row">
				<label for="dn">Display name</label>
				<div class="grid gap-1.5">
					<input
						id="dn"
						class="field"
						maxlength="64"
						autocomplete="name"
						bind:value={displayName}
						placeholder={app.user?.username}
					/>
					<span class="hint">
						Shown on your public profile. Your username, {app.user?.username}, never changes.
					</span>
				</div>
			</div>
			<div class="row">
				<label for="tz">Time zone</label>
				<div class="grid gap-1.5">
					<select id="tz" class="field" bind:value={timezone}>
						{#if !zones.includes(timezone)}<option value={timezone}>{timezone}</option>{/if}
						{#each zones as z (z)}<option value={z}>{z}</option>{/each}
					</select>
					<span class="hint">
						Days and streaks start at midnight here.
						{#if browserZone && browserZone !== timezone}
							This browser is in {browserZone}.
							<button
								type="button"
								class="inline underline underline-offset-2"
								onclick={() => (timezone = browserZone)}
							>
								Use it
							</button>
						{/if}
					</span>
				</div>
			</div>
			<div class="row">
				<span>Public profile</span>
				<div class="grid gap-1.5">
					<label class="check">
						<input type="checkbox" bind:checked={isPublic} /> Anyone can see my profile at /u/{app
							.user?.username}
					</label>
					<span class="hint">
						It shows totals, top projects and languages. Never your email, code or prompts. Hidden
						projects are left out.
					</span>
				</div>
			</div>
			<div class="row">
				<span></span>
				<div class="flex items-center gap-4">
					<button class="btn" type="submit" disabled={saving}>
						{saving ? 'Saving…' : 'Save profile'}
					</button>
					{#if profileMsg}<span class="hint" class:text-ink={!profileMsg.ok} role="status">
							{profileMsg.text}
						</span>{/if}
				</div>
			</div>
		</form>
	</Panel>

	<Panel title="Install keys" meta="{data.keys.length} of 20">
		<p class="mute max-w-[64ch]">
			Each computer gets its own key. The agent and your editor plugins use it to send time to
			Logbook. Revoke a key when you stop using that computer.
		</p>
		{#if data.keys.length}
			<ul class="list mt-4">
				{#each data.keys as k (k.id)}
					<li>
						<div class="min-w-0">
							<div class="truncate">{k.name}</div>
							<div class="lbl">
								<span class="mono">{k.prefix}…</span> · made {day(k.created_at)} · {k.last_used_at
									? `used ${ago(k.last_used_at, now)}`
									: 'never used'}
							</div>
						</div>
						<ConfirmButton
							label="Revoke"
							name="Revoke the key {k.name}"
							ask="Revoke {k.name}? Its computer stops sending time."
							confirm="Revoke"
							run={() => act(() => app.api.revokeApiKey(k.id))}
						/>
					</li>
				{/each}
			</ul>
		{/if}
		<h2 class="sub">Install on another computer</h2>
		<InstallCommand onCreated={() => void invalidate(ME)} />
	</Panel>

	<Panel title="Computers" meta="{activeDevices.length} connected" anchor="devices">
		{#if data.devices.length === 0}
			<p class="mute">No computer has connected yet. Install the agent with a key above.</p>
		{:else}
			<ul class="list">
				{#each data.devices as d (d.id)}
					<li class:revoked={!!d.revoked_at}>
						<div class="min-w-0">
							<div class="truncate">{d.name}</div>
							<div class="lbl">
								{OS[d.os] ?? d.os} · key <span class="mono">{d.key_fingerprint}</span> ·
								{#if d.revoked_at}revoked {day(d.revoked_at)}{:else}{d.last_seen_at
										? `uploaded ${ago(d.last_seen_at, now)}`
										: 'no upload yet'}{/if}
							</div>
						</div>
						{#if !d.revoked_at}
							<ConfirmButton
								label="Disconnect"
								name="Disconnect {d.name}"
								ask="Disconnect {d.name}? Its counted time stays."
								confirm="Disconnect"
								run={() => act(() => app.api.revokeDevice(d.id))}
							/>
						{/if}
					</li>
				{/each}
			</ul>
		{/if}
	</Panel>

	<Panel title="Hackatime">
		{#if hackatimeNotice}<p class="hint mb-3 text-ink" role="status">{hackatimeNotice}</p>{/if}
		<HackatimeStatus link={data.hackatime} />
		{#if data.hackatime.linked}
			<div class="mt-4 flex flex-wrap items-center gap-4">
				<span class="hint">
					Linked to {data.hackatime.hackatime_username
						? `@${data.hackatime.hackatime_username}`
						: `Hackatime account ${data.hackatime.hackatime_user_id}`}.
				</span>
				{#if data.hackatime.trust_level !== 'red'}
					<ConfirmButton
						label="Unlink"
						ask="Unlink Hackatime? Imported history stays."
						confirm="Unlink"
						run={() => act(() => app.api.unlinkHackatime())}
					/>
				{/if}
			</div>
		{/if}
	</Panel>

	<Panel title="Apps with access" meta={String(data.grants.length)} anchor="apps">
		<p class="mute">{grantsSentence(data.grants.length)}</p>
		{#if data.grants.length}
			<ul class="list mt-4">
				{#each data.grants as g (g.app_id)}
					<li>
						<div class="app">
							<Identicon seed={g.app_slug} size={24} />
							<div class="min-w-0">
								<div class="truncate">{g.app_name}</div>
								<div class="lbl">
									by {g.owner_username} · reads your {scopeWords(g.scopes)} ·
									<span class="whitespace-nowrap">since {day(g.created_at)}</span>
								</div>
							</div>
						</div>
						<ConfirmButton
							label="Revoke"
							name="Revoke {g.app_name}"
							ask="Revoke {g.app_name}’s access? It stops working at once."
							confirm="Revoke"
							run={() => act(() => app.api.revokeGrant(g.app_id))}
						/>
					</li>
				{/each}
			</ul>
		{:else}
			<p class="hint mt-1.5">
				When you sign in to a program with Logbook, it shows up here, and you can take its access
				back at any time.
			</p>
		{/if}
	</Panel>

	<Panel title="Session">
		<div class="flex flex-wrap items-center justify-between gap-4">
			<p class="mute">Signed in with Hack Club as {app.user?.username}.</p>
			<div class="grid justify-items-end gap-1.5">
				<button class="btn" type="button" onclick={signOut} disabled={signingOut}>
					{signingOut ? 'Signing out…' : 'Sign out'}
				</button>
				{#if signOutError}<span class="hint text-ink" role="alert">{signOutError}</span>{/if}
			</div>
		</div>
	</Panel>
</div>

<style>
	.stack {
		display: grid;
		gap: 12px;
		/* The page's 8-column line (DESIGN.md); full width on narrow screens. */
		max-width: calc((100% - 12px) * 8 / 12);
		padding-bottom: 32px;
	}
	.form {
		display: grid;
		gap: 18px;
	}
	.row {
		display: grid;
		grid-template-columns: 160px minmax(0, 1fr);
		gap: 16px;
		align-items: baseline;
	}
	.row .field {
		max-width: 420px;
	}
	.check {
		display: flex;
		gap: 10px;
		align-items: center;
	}
	input[type='checkbox'] {
		accent-color: var(--color-ink);
	}
	.list {
		list-style: none;
		padding: 0;
	}
	.list:not(:first-child) {
		border-top: 1px solid var(--color-rule);
	}
	.list:first-child li:first-child {
		padding-top: 0;
	}
	.list li {
		display: flex;
		justify-content: space-between;
		align-items: center;
		gap: 16px;
		padding: 12px 0;
		border-bottom: 1px solid var(--color-rule);
	}
	.app {
		display: flex;
		align-items: center;
		gap: 12px;
		min-width: 0;
	}
	.list li.revoked {
		color: var(--color-ink-3);
	}
	.sub {
		font-weight: 400;
		font-size: var(--text-base);
		margin-top: 22px;
	}
	button.inline {
		background: none;
		border: 0;
		padding: 0;
		font: inherit;
		color: var(--color-ink-2);
		cursor: pointer;
	}
	@media (max-width: 900px) {
		.stack {
			max-width: none;
		}
	}
	@media (max-width: 560px) {
		.row {
			grid-template-columns: 1fr;
			gap: 8px;
		}
		.list li {
			flex-direction: column;
			align-items: flex-start;
		}
	}
</style>

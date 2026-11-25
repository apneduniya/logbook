<!--
  An app's members: the owner first, then reviewers, as Settings-style rows
  with an identicon each. The owner adds reviewers by username and removes
  them with a ConfirmButton; reviewers see the list without those controls
  (hidden, not disabled).

  Props: app (#lib/types.ts App), members, onchange (reload after a change).
-->
<script lang="ts">
	import { app as appState } from '#lib/app.svelte.ts';
	import { appError } from '#lib/apps.ts';
	import { dateRange } from '#lib/format.ts';
	import ConfirmButton from '#lib/components/ConfirmButton.svelte';
	import Identicon from '#lib/components/Identicon.svelte';
	import Panel from '#lib/components/Panel.svelte';
	import type { App, AppMember } from '#lib/types.ts';

	interface Props {
		app: App;
		members: AppMember[];
		onchange: () => Promise<void>;
	}
	let { app, members, onchange }: Props = $props();

	const owner = $derived(app.role === 'owner');
	const reviewers = $derived(members.filter((m) => m.role === 'reviewer').length);
	const day = (t: string) => dateRange(t.slice(0, 10), t.slice(0, 10));
	const me = $derived(appState.user?.username ?? '');

	const id = $props.id();
	let username = $state('');
	let adding = $state(false);
	let addMsg = $state<{ ok: boolean; text: string } | null>(null);
	let input = $state<HTMLInputElement | null>(null);

	async function add(e: SubmitEvent) {
		e.preventDefault();
		const u = username.trim().replace(/^@/, '');
		addMsg = null;
		if (!u) {
			addMsg = { ok: false, text: 'Enter their Logbook username.' };
			input?.focus();
			return;
		}
		adding = true;
		try {
			const m = await appState.api.addAppMember(app.id, u);
			await onchange();
			username = '';
			addMsg = { ok: true, text: `Added ${m.username}. They can open this inbox now.` };
		} catch (err) {
			addMsg = { ok: false, text: appError(err, { username: u }) };
			input?.focus();
		} finally {
			adding = false;
		}
	}

	function note(m: AppMember): string {
		const what =
			m.role === 'owner'
				? 'owner'
				: `added ${day(m.added_at)}${m.added_by ? ` by ${m.added_by}` : ''}`;
		return m.username === me ? `${what} · you` : what;
	}
</script>

<Panel title="Reviewers" meta="{reviewers} of 50">
	<p class="hint">
		Members open the inbox and record decisions on review links. Only the owner changes the app.
	</p>
	<ul class="list mt-3">
		{#each members as m (m.username)}
			<li>
				<div class="who">
					<Identicon seed={m.username} />
					<div class="min-w-0">
						<div class="truncate">{m.username}</div>
						<div class="lbl">{note(m)}</div>
					</div>
				</div>
				{#if owner && m.role === 'reviewer'}
					<ConfirmButton
						label="Remove"
						name="Remove {m.username}"
						ask="Remove {m.username}? They lose this inbox at once."
						confirm="Remove"
						run={async () => {
							await appState.api.removeAppMember(app.id, m.username);
							await onchange();
						}}
					/>
				{/if}
			</li>
		{/each}
	</ul>
	{#if owner}
		<form class="add" onsubmit={add} novalidate>
			<label class="lbl" for="{id}-u">Add a reviewer</label>
			<div class="flex flex-wrap gap-2">
				<input
					id="{id}-u"
					bind:this={input}
					bind:value={username}
					class="field mono min-w-0 flex-1"
					autocomplete="off"
					autocapitalize="none"
					spellcheck="false"
					maxlength="64"
					placeholder="username"
					aria-describedby="{id}-m"
					oninput={() => (addMsg = null)}
				/>
				<button class="btn" type="submit" disabled={adding}>{adding ? 'Adding…' : 'Add'}</button>
			</div>
			<span id="{id}-m" class="hint" class:text-ink={addMsg && !addMsg.ok} role="status">
				{addMsg?.text ?? 'Their Logbook username, as on their profile.'}
			</span>
		</form>
	{/if}
</Panel>

<style>
	.list {
		list-style: none;
		padding: 0;
		border-top: 1px solid var(--color-rule);
	}
	.list li {
		display: flex;
		justify-content: space-between;
		align-items: center;
		flex-wrap: wrap;
		gap: 8px 16px;
		padding: 10px 0;
		border-bottom: 1px solid var(--color-rule);
	}
	.who {
		display: flex;
		align-items: center;
		gap: 10px;
		min-width: 0;
	}
	/* .field resets the font; URIs, slugs and usernames are mono. */
	.field.mono {
		font-family: var(--font-mono);
	}
	.add {
		display: grid;
		gap: 8px;
		margin-top: 18px;
		max-width: 420px;
	}
</style>

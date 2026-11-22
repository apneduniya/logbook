<!--
  Creates an API key on request and shows the one-line install command with
  it. The key lives only in this component's state (docs/web.md, Security):
  masked until "Show", gone on navigation.
-->
<script lang="ts">
	import { app } from '#lib/app.svelte.ts';
	import { ApiError } from '#lib/api.ts';
	import { installCommand } from '#lib/links.ts';
	import Segmented from './Segmented.svelte';

	let { onCreated }: { onCreated?: () => void } = $props();

	type Os = 'unix' | 'windows';
	let os = $state<Os>(navigator.userAgent.includes('Windows') ? 'windows' : 'unix');
	let key = $state<string | null>(null);
	let shown = $state(false);
	let busy = $state(false);
	let error = $state<string | null>(null);
	let copied = $state(false);

	const masked = $derived(key ? `${key.slice(0, 4)}••••` : '');
	const command = $derived(key ? installCommand(os, key, app.api.origin) : '');
	const visible = $derived(key ? installCommand(os, shown ? key : masked, app.api.origin) : '');

	function deviceName(): string {
		const p = navigator.userAgent;
		if (p.includes('Windows')) return 'Windows computer';
		if (p.includes('Mac')) return 'Mac';
		if (p.includes('Linux')) return 'Linux computer';
		return 'Computer';
	}

	async function create() {
		busy = true;
		error = null;
		try {
			const k = await app.api.createApiKey(deviceName());
			key = k.key;
			onCreated?.();
		} catch (err) {
			error =
				err instanceof ApiError && err.status === 409
					? 'You already have 20 keys. Revoke one in Settings first.'
					: err instanceof ApiError
						? err.userMessage
						: 'Couldn’t create a key.';
		} finally {
			busy = false;
		}
	}

	async function copy() {
		try {
			await navigator.clipboard.writeText(command);
			copied = true;
			setTimeout(() => (copied = false), 1600);
		} catch {
			error = 'Couldn’t copy. Press Show and copy the command by hand.';
		}
	}
</script>

<div class="mt-3">
	<Segmented
		label="Your system"
		value={os}
		onchange={(v: Os) => (os = v)}
		options={[
			{ value: 'unix', label: 'macOS · Linux' },
			{ value: 'windows', label: 'Windows' }
		]}
	/>
</div>

{#if key}
	<div class="cmd">
		<code>{visible}</code>
		<div class="flex flex-none gap-1">
			<button class="ghost" type="button" onclick={() => (shown = !shown)} aria-pressed={shown}>
				{shown ? 'Hide' : 'Show'}
			</button>
			<button class="copy" type="button" onclick={copy}>{copied ? 'Copied' : 'Copy'}</button>
		</div>
	</div>
	<p class="lbl mt-2">
		{os === 'windows' ? 'Paste it into PowerShell.' : 'Paste it into a terminal.'} The command has your
		key in it, so don’t paste it anywhere public. This is the only time the key is shown.
	</p>
{:else}
	<div class="mt-3 flex flex-wrap items-center gap-4">
		<button class="btn" type="button" onclick={create} disabled={busy}>
			{busy ? 'Creating a key…' : 'Get my install command'}
		</button>
		<span class="lbl">Creates a key for this computer.</span>
	</div>
{/if}
{#if error}<p class="lbl mt-2 text-ink" role="alert">{error}</p>{/if}

<span class="sr-only" aria-live="polite">{copied ? 'Command copied' : ''}</span>

<style>
	.cmd {
		display: flex;
		align-items: center;
		justify-content: space-between;
		gap: 12px;
		margin-top: 12px;
		padding: 8px 8px 8px 14px;
		background: var(--color-fill);
		border: 1px solid var(--color-rule);
		font-family: var(--font-mono);
		font-size: var(--text-sm);
		min-width: 0;
	}
	code {
		overflow-x: auto;
		white-space: nowrap;
		min-width: 0;
		scrollbar-width: thin;
		padding: 2px 0;
	}
	.copy,
	.ghost {
		font-family: var(--font-pixel);
		font-size: var(--text-xs);
		letter-spacing: 0.06em;
		text-transform: uppercase;
		border: 0;
		padding: 6px 10px;
		cursor: pointer;
		transition: background 150ms ease-out;
	}
	.copy {
		color: var(--color-ground);
		background: var(--color-ink);
	}
	.copy:hover {
		background: var(--color-ink-2);
	}
	.ghost {
		color: var(--color-ink-2);
		background: none;
	}
	.ghost:hover {
		color: var(--color-ink);
	}
</style>

<!--
  Creates an API key on request and shows the one-line install command with
  it. The key lives only in component state (docs/web.md, Security): masked
  until "Show", gone on navigation. A parent may bind `key` so it outlives a
  swap between its child views (the home page does, across reloads).
-->
<script lang="ts">
	import { app } from '#lib/app.svelte.ts';
	import { ApiError } from '#lib/api.ts';
	import { installCommand } from '#lib/links.ts';
	import CopyField from './CopyField.svelte';
	import Segmented from './Segmented.svelte';

	let { onCreated, key = $bindable(null) }: { onCreated?: () => void; key?: string | null } =
		$props();

	type Os = 'unix' | 'windows';
	let os = $state<Os>(navigator.userAgent.includes('Windows') ? 'windows' : 'unix');
	let busy = $state(false);
	let error = $state<string | null>(null);

	const command = $derived(key ? installCommand(os, key, app.api.origin) : '');
	const masked = $derived(key ? installCommand(os, `${key.slice(0, 4)}••••`, app.api.origin) : '');

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
	<div class="mt-3">
		<CopyField value={command} {masked} label="Install command" />
	</div>
	<p class="hint mt-2">
		{os === 'windows' ? 'Paste it into PowerShell.' : 'Paste it into a terminal.'} The command has your
		key in it, so don’t paste it anywhere public. This is the only time the key is shown.
	</p>
{:else}
	<div class="mt-3 flex flex-wrap items-center gap-4">
		<button class="btn" type="button" onclick={create} disabled={busy}>
			{busy ? 'Creating a key…' : 'Get my install command'}
		</button>
		<span class="hint">Creates a key for this computer.</span>
	</div>
{/if}
{#if error}<p class="hint mt-2 text-ink" role="alert">{error}</p>{/if}

<!--
  How an app authenticates (Linear's credentials block): the client id with
  Copy, and the client secret. The secret is shown once, masked until
  "Show", right after register or rotate; after that the panel only says
  when it was set, because only its hash is stored. The owner rotates it
  with a ConfirmButton. A public client has no secret at all.

  Props: app (#lib/types.ts App), secret (the one-time secret, or null),
  rotated (the secret replaced an old one), onrotate (called with the new
  secret; throw ApiError to show a message).
-->
<script lang="ts">
	import { app as appState } from '#lib/app.svelte.ts';
	import { maskSecret } from '#lib/apps.ts';
	import { utcStamp } from '#lib/format.ts';
	import ConfirmButton from '#lib/components/ConfirmButton.svelte';
	import CopyField from '#lib/components/CopyField.svelte';
	import Panel from '#lib/components/Panel.svelte';
	import type { App } from '#lib/types.ts';

	interface Props {
		app: App;
		secret: string | null;
		rotated: boolean;
		onrotate: () => Promise<void>;
	}
	let { app, secret, rotated, onrotate }: Props = $props();

	const setAt = $derived(app.secret_rotated_at ?? app.created_at);
</script>

<Panel title="Credentials" meta={app.confidential ? 'confidential' : 'public client'}>
	<div class="grid gap-1.5">
		<span class="lbl">Client id</span>
		<CopyField value={app.client_id} label="Client id" />
	</div>

	<div class="mt-5 grid gap-1.5">
		<span class="lbl">Client secret</span>
		{#if !app.confidential}
			<p class="hint">
				A public client has no secret. It proves each sign-in with PKCE (S256) instead.
			</p>
		{:else if secret}
			<CopyField value={secret} masked={maskSecret(secret)} label="Client secret" />
			<p class="hint text-ink" role="status">
				Copy it now. Logbook keeps only a hash, so it can’t be shown again.
				{#if rotated}The old secret has stopped working.{/if}
			</p>
		{:else}
			<p class="mono set">Set {utcStamp(setAt)}</p>
			<p class="hint">Only its hash is stored. If it’s lost or leaked, make a new one.</p>
		{/if}
		{#if app.confidential && app.role === 'owner'}
			<div class="mt-2">
				<ConfirmButton
					label={secret ? 'Rotate again' : 'Rotate secret'}
					ask="Make a new secret? The old one stops working at once."
					confirm="Rotate"
					align="start"
					run={onrotate}
				/>
			</div>
		{/if}
	</div>

	<p class="hint mt-5">
		A program that signs people in with Hackatime switches by using these, with
		<code class="mono">{appState.api.origin}</code> in place of Hackatime’s address.
	</p>
</Panel>

<style>
	.set {
		font-size: var(--text-sm);
	}
</style>

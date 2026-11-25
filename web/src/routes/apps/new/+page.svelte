<!--
  Register an app: one panel with the form (no wizard, no modal). On success
  the new client secret is handed to the app's page in memory (#lib/secret.ts),
  shown there once, and never stored.
-->
<script lang="ts">
	import { goto } from '$app/navigation';
	import { resolve } from '$app/paths';
	import { app } from '#lib/app.svelte.ts';
	import { emptyForm, toCreate, type AppForm } from '#lib/apps.ts';
	import { holdSecret } from '#lib/secret.ts';
	import AppFormView from '#lib/apps/AppForm.svelte';
	import Panel from '#lib/components/Panel.svelte';

	async function register(f: AppForm) {
		const created = await app.api.createApp(toCreate(f));
		if (created.client_secret) holdSecret(created.id, created.client_secret);
		await goto(resolve('/apps/[id]', { id: created.id }));
	}
</script>

<svelte:head><title>Register an app · Logbook</title></svelte:head>

<div class="head">
	<a class="link lbl" href={resolve('/apps')}>← Apps</a>
	<h1 class="px">Register an app</h1>
	<p class="hint">
		An app lets a program sign people in with Logbook and read the hours they allow. If it takes
		submissions, you and the reviewers you add decide on them here.
	</p>
</div>

<div class="stack">
	<Panel title="App">
		<AppFormView
			mode="create"
			initial={emptyForm()}
			submit="Register app"
			run={register}
			oncancel={() => goto(resolve('/apps'))}
		/>
	</Panel>
</div>

<style>
	.head {
		display: grid;
		gap: 8px;
		padding: 12px 0 20px;
	}
	h1 {
		font-weight: 400;
		font-size: var(--text-xl);
		letter-spacing: -0.01em;
	}
	.stack {
		max-width: calc((100% - 12px) * 8 / 12);
		padding-bottom: 32px;
	}
	@media (max-width: 900px) {
		.stack {
			max-width: none;
		}
	}
</style>

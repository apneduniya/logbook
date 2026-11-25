<!--
  The register and edit form of an app, in the Settings `.row` layout: label
  and hint on the left, the field on the right. Redirect URIs are a list of
  1–5 fields with "Add another". Fields are checked on submit (and each one
  once it has been left); the submit button never disables to signal bad
  input, only while saving. A failed save keeps what was typed.

  Props:
  - mode: 'create' (slug and client type too) or 'edit'.
  - initial: the starting values (#lib/apps.ts emptyForm() or formOf(app)).
  - submit: the button's word ("Register app", "Save changes").
  - run: called with the checked form; throw to show a message (appError).
    Return a field name ('slug') to put the message under that field.
  - oncancel: shows a Cancel button.
-->
<script lang="ts">
	import { tick } from 'svelte';
	import {
		appError,
		checkAppForm,
		hasErrors,
		MAX_NAME,
		MAX_REDIRECTS,
		slugify,
		type AppForm
	} from '#lib/apps.ts';
	import { ApiError } from '#lib/api.ts';

	interface Props {
		mode: 'create' | 'edit';
		initial: AppForm;
		submit: string;
		run: (form: AppForm) => Promise<void>;
		oncancel?: () => void;
	}
	let { mode, initial, submit, run, oncancel }: Props = $props();

	const id = $props.id();
	// A copy the form edits; the caller's value is only the starting point.
	// svelte-ignore state_referenced_locally
	let f = $state<AppForm>(structuredClone($state.snapshot(initial)));
	// The slug follows the name until it is typed in by hand (create only).
	// svelte-ignore state_referenced_locally
	let slugTyped = $state(mode === 'edit');
	let tried = $state(false);
	let left = $state<Record<string, boolean>>({});
	let busy = $state(false);
	let message = $state<string | null>(null);
	let slugServer = $state<string | null>(null);
	let form = $state<HTMLFormElement | null>(null);

	const errors = $derived(checkAppForm(f, mode));
	const show = (key: string) => tried || !!left[key];
	const nameErr = $derived(show('name') ? errors.name : undefined);
	const slugErr = $derived(slugServer ?? (show('slug') ? errors.slug : undefined));
	const homeErr = $derived(show('home') ? errors.homepage : undefined);
	const listErr = $derived(tried ? errors.redirectList : undefined);

	function onName() {
		if (!slugTyped) f.slug = slugify(f.name);
	}

	async function addRedirect() {
		f.redirects.push('');
		await tick();
		form?.querySelector<HTMLInputElement>(`#${id}-r${f.redirects.length - 1}`)?.focus();
	}

	async function removeRedirect(i: number) {
		f.redirects.splice(i, 1);
		// The fields after it move up one, and so does whether each was left.
		const next: Record<string, boolean> = {};
		for (const [k, v] of Object.entries(left)) {
			const m = /^r(\d+)$/.exec(k);
			if (!m) next[k] = v;
			else if (Number(m[1]) < i) next[k] = v;
			else if (Number(m[1]) > i) next[`r${Number(m[1]) - 1}`] = v;
		}
		left = next;
		await tick();
		form?.querySelector<HTMLInputElement>(`#${id}-r${Math.max(0, i - 1)}`)?.focus();
	}

	async function onsubmit(e: SubmitEvent) {
		e.preventDefault();
		tried = true;
		message = null;
		slugServer = null;
		if (hasErrors(errors)) {
			message = 'Fix the fields marked above.';
			await tick();
			form?.querySelector<HTMLElement>('[aria-invalid="true"]')?.focus();
			return;
		}
		busy = true;
		try {
			await run($state.snapshot(f));
		} catch (err) {
			const text = appError(err);
			if (err instanceof ApiError && err.message === 'slug is taken' && mode === 'create') {
				slugServer = text;
				message = 'Pick another slug.';
			} else {
				message = text;
			}
		} finally {
			busy = false;
		}
	}
</script>

<form class="form" bind:this={form} {onsubmit} novalidate>
	<div class="row">
		<label for="{id}-name">Name</label>
		<div class="grid gap-1.5">
			<input
				id="{id}-name"
				class="field"
				maxlength={MAX_NAME * 2}
				autocomplete="off"
				bind:value={f.name}
				oninput={onName}
				onblur={() => (left.name = true)}
				aria-invalid={!!nameErr}
				aria-describedby="{id}-name-h"
				placeholder="Stardance"
			/>
			<span id="{id}-name-h" class="hint" class:err={nameErr}>
				{nameErr ?? 'People see it when they sign in with Logbook and when they submit hours.'}
			</span>
		</div>
	</div>

	{#if mode === 'create'}
		<div class="row">
			<label for="{id}-slug">Slug</label>
			<div class="grid gap-1.5">
				<input
					id="{id}-slug"
					class="field mono"
					maxlength="40"
					autocomplete="off"
					spellcheck="false"
					autocapitalize="none"
					bind:value={f.slug}
					oninput={() => {
						slugTyped = true;
						slugServer = null;
					}}
					onblur={() => (left.slug = true)}
					aria-invalid={!!slugErr}
					aria-describedby="{id}-slug-h"
					placeholder="stardance"
				/>
				<span id="{id}-slug-h" class="hint" class:err={slugErr}>
					{#if slugErr}{slugErr}{:else}
						What people type to send you hours:
						<code class="mono">logbook submit … --program {f.slug.trim() || 'slug'}</code>. It can’t
						change later.
					{/if}
				</span>
			</div>
		</div>
	{/if}

	<div class="row">
		<label for="{id}-home">Homepage <span class="lbl">optional</span></label>
		<div class="grid gap-1.5">
			<input
				id="{id}-home"
				class="field"
				type="url"
				inputmode="url"
				autocomplete="off"
				spellcheck="false"
				bind:value={f.homepage}
				onblur={() => (left.home = true)}
				aria-invalid={!!homeErr}
				aria-describedby="{id}-home-h"
				placeholder="https://stardance.example"
			/>
			<span id="{id}-home-h" class="hint" class:err={homeErr}>
				{homeErr ?? 'Where people can read about the program.'}
			</span>
		</div>
	</div>

	<div class="row">
		<span id="{id}-r">Redirect URIs</span>
		<div class="grid gap-2" role="group" aria-labelledby="{id}-r" aria-describedby="{id}-r-h">
			{#each f.redirects, i (i)}
				{@const err = show(`r${i}`) ? errors.redirects[i] : null}
				<div class="grid gap-1.5">
					<div class="uri">
						<input
							id="{id}-r{i}"
							class="field mono"
							type="url"
							inputmode="url"
							autocomplete="off"
							spellcheck="false"
							bind:value={f.redirects[i]}
							onblur={() => (left[`r${i}`] = true)}
							aria-label="Redirect URI {i + 1}"
							aria-invalid={!!err || (!!listErr && i === 0)}
							aria-describedby={err ? `${id}-r${i}-e` : undefined}
							placeholder={i === 0 ? 'https://stardance.example/auth/callback' : ''}
						/>
						{#if f.redirects.length > 1}
							<button
								class="link rm"
								type="button"
								onclick={() => removeRedirect(i)}
								aria-label="Remove redirect URI {i + 1}"
							>
								Remove
							</button>
						{/if}
					</div>
					{#if err}<span id="{id}-r{i}-e" class="hint err">{err}</span>{/if}
				</div>
			{/each}
			<div class="flex flex-wrap items-center gap-x-4 gap-y-1.5">
				{#if f.redirects.length < MAX_REDIRECTS}
					<button class="btn" type="button" onclick={addRedirect}>Add another</button>
				{/if}
				<span class="lbl">{f.redirects.length} of {MAX_REDIRECTS}</span>
			</div>
			<span id="{id}-r-h" class="hint" class:err={listErr}>
				{listErr ??
					'Where Logbook sends people back after they choose. It must match exactly, character for character.'}
			</span>
		</div>
	</div>

	<div class="row">
		<span id="{id}-pub">Program</span>
		<div class="grid gap-1.5" role="group" aria-labelledby="{id}-pub">
			<label class="check">
				<input type="checkbox" bind:checked={f.listed} /> Take submissions
			</label>
			<span class="hint">
				Lists the app as a program, so people can send it their hours and you can review them here.
				Sign-in works either way.
			</span>
		</div>
	</div>

	{#if mode === 'create'}
		<div class="row">
			<span id="{id}-type">Client type</span>
			<div class="grid gap-2.5" role="radiogroup" aria-labelledby="{id}-type">
				<label class="check top">
					<input type="radio" name="{id}-type" value={true} bind:group={f.confidential} />
					<span>
						Confidential
						<span class="hint block">Your server keeps a secret. Most apps.</span>
					</span>
				</label>
				<label class="check top">
					<input type="radio" name="{id}-type" value={false} bind:group={f.confidential} />
					<span>
						Public
						<span class="hint block">
							No secret, for apps that run on people’s own devices. Every sign-in uses PKCE. It
							can’t change later.
						</span>
					</span>
				</label>
			</div>
		</div>
	{/if}

	<div class="row">
		<span></span>
		<div class="grid gap-2">
			<div class="flex flex-wrap items-center gap-4">
				<button class="btn" type="submit" disabled={busy}>
					{busy ? 'Saving…' : submit}
				</button>
				{#if oncancel}
					<button class="link cancel" type="button" onclick={oncancel} disabled={busy}>
						Cancel
					</button>
				{/if}
			</div>
			{#if message}<span class="hint err" role="alert">{message}</span>{/if}
		</div>
	</div>
</form>

<style>
	.form {
		display: grid;
		gap: 20px;
	}
	.row {
		display: grid;
		grid-template-columns: 160px minmax(0, 1fr);
		gap: 16px;
		align-items: baseline;
	}
	.row .field {
		width: 100%;
		max-width: 460px;
	}
	/* .field resets the font; URIs, slugs and usernames are mono. */
	.field.mono {
		font-family: var(--font-mono);
	}
	.uri {
		display: flex;
		align-items: center;
		gap: 12px;
		max-width: 540px;
	}
	.uri .field {
		flex: 1;
	}
	.rm,
	.cancel {
		background: none;
		border: 0;
		padding: 0;
		font: inherit;
		cursor: pointer;
		flex: none;
	}
	.check {
		display: flex;
		gap: 10px;
		align-items: center;
	}
	.check.top {
		align-items: baseline;
	}
	input[type='checkbox'],
	input[type='radio'] {
		accent-color: var(--color-ink);
	}
	.field[aria-invalid='true'] {
		border-color: var(--color-ink);
		box-shadow: inset 2px 0 0 var(--color-ink);
	}
	.err {
		color: var(--color-ink);
	}
	@media (max-width: 560px) {
		.row {
			grid-template-columns: 1fr;
			gap: 8px;
		}
	}
</style>

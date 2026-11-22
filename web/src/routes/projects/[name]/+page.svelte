<script lang="ts">
	import { resolve } from '$app/paths';
	import { invalidate } from '$app/navigation';
	import { app, ME } from '#lib/app.svelte.ts';
	import { ApiError } from '#lib/api.ts';
	import { duration, instantLabel, percent, rangeLabel } from '#lib/format.ts';
	import Panel from '#lib/components/Panel.svelte';
	import DayBars from '#lib/components/DayBars.svelte';
	import { dailyBars } from '#lib/bars.ts';
	import type { PageProps } from './$types';

	let { data }: PageProps = $props();
	const p = $derived(data.project);

	// A hint only; the server's check is the real one (docs/api/stats.md).
	const REPO = /^https:\/\/github\.com\/[^/\s?#]+\/[^/\s?#]+$/;

	// The form edits a copy; it resets when the project reloads.
	let repo = $derived(p.repo_url);
	let hidden = $derived(p.hidden);
	let archived = $derived(p.archived);
	let saving = $state(false);
	let message = $state<{ ok: boolean; text: string } | null>(null);

	const dirty = $derived(
		repo.trim() !== p.repo_url || hidden !== p.hidden || archived !== p.archived
	);
	const repoValid = $derived(repo.trim() === '' || REPO.test(repo.trim()));
	const langTotal = $derived(p.languages.reduce((s, l) => s + l.seconds, 0));

	async function save(e: SubmitEvent) {
		e.preventDefault();
		if (!dirty) {
			message = { ok: true, text: 'Nothing changed.' };
			return;
		}
		if (!repoValid) {
			message = { ok: false, text: 'Fix the repository address first.' };
			return;
		}
		saving = true;
		message = null;
		try {
			await app.api.updateProject(p.name, {
				repo_url: repo.trim() !== p.repo_url ? repo.trim() : undefined,
				hidden: hidden !== p.hidden ? hidden : undefined,
				archived: archived !== p.archived ? archived : undefined
			});
			await invalidate(ME);
			message = { ok: true, text: 'Saved.' };
		} catch (err) {
			message = { ok: false, text: err instanceof ApiError ? err.userMessage : 'Couldn’t save.' };
		} finally {
			saving = false;
		}
	}
</script>

<svelte:head><title>{p.name} · Logbook</title></svelte:head>

<div class="head">
	<a class="link lbl" href={resolve('/projects')}>← Projects</a>
	<h1 class="px">{p.name}</h1>
	<p class="mute">
		{#if p.last_activity}Last active {instantLabel(p.last_activity, app.timezone)}{:else}No tracked
			time yet{/if}
		{#if p.hidden}· hidden from your profile{/if}
		{#if p.archived}· archived{/if}
		{#if p.repo_url.startsWith('https://github.com/')}
			· <a
				class="link underline decoration-rule underline-offset-4"
				href={p.repo_url}
				rel="external noopener noreferrer"
			>
				{p.repo_url.slice('https://github.com/'.length)}
			</a>
		{/if}
	</p>
</div>

<div class="g2">
	<Panel title="Last 30 days" meta={rangeLabel(data.summary.start, data.summary.end)}>
		<div class="grid gap-5">
			<div class="text-base">
				<span class="mono">{duration(data.summary.credited_seconds)}</span>
				<span class="mute">counted of</span>
				<span class="mono">{duration(data.summary.total_seconds)}</span>
				<span class="mute">at the keyboard</span>
			</div>
			<DayBars
				bars={dailyBars(data.summary.days)}
				label="Counted time per day in {p.name}, last 30 days"
			/>
		</div>
	</Panel>
	<Panel title="All time">
		<dl class="totals">
			<div>
				<dt class="lbl">Counted</dt>
				<dd class="mono">
					{p.credited_seconds === null ? '—' : duration(p.credited_seconds, { long: true })}
				</dd>
			</div>
			<div>
				<dt class="lbl">At the keyboard</dt>
				<dd class="mono">{duration(p.total_seconds, { long: true })}</dd>
			</div>
		</dl>
		{#if p.languages.length}
			<div class="langs">
				<span class="lbl col-span-full">Languages</span>
				{#each p.languages.slice(0, 6) as l (l.name)}
					<span class="truncate">{l.name || 'Unknown'}</span>
					<span class="mono">{duration(l.seconds)}</span>
					<span class="mono lbl text-right">{percent(l.seconds, langTotal)}%</span>
				{/each}
			</div>
		{/if}
	</Panel>
</div>

<div class="mt-3">
	<Panel title="Project settings">
		<form class="form" onsubmit={save}>
			<div class="row">
				<label for="repo">GitHub repository</label>
				<div class="grid gap-1.5">
					<input
						id="repo"
						class="field"
						type="url"
						inputmode="url"
						autocomplete="off"
						spellcheck="false"
						placeholder="https://github.com/owner/repo"
						bind:value={repo}
						aria-invalid={!repoValid}
						aria-describedby="repo-help"
					/>
					<span id="repo-help" class="lbl">
						{repoValid
							? 'Links this project to its code. Leave empty to unlink.'
							: 'Use the form https://github.com/owner/repo.'}
					</span>
				</div>
			</div>
			<div class="row">
				<span>Visibility</span>
				<div class="grid gap-2.5">
					<label class="check">
						<input type="checkbox" bind:checked={hidden} /> Hide from my public profile
					</label>
					<label class="check">
						<input type="checkbox" bind:checked={archived} /> Archive (leave out of project lists)
					</label>
				</div>
			</div>
			<div class="row">
				<span></span>
				<div class="flex items-center gap-4">
					<button class="btn" type="submit" disabled={saving}>
						{saving ? 'Saving…' : 'Save'}
					</button>
					{#if message}<span class="lbl" class:text-ink={!message.ok} role="status">
							{message.text}
						</span>{/if}
				</div>
			</div>
		</form>
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
		overflow-wrap: anywhere;
	}
	.g2 {
		display: grid;
		grid-template-columns: calc((100% - 12px) * 8 / 12) 1fr;
		gap: 12px;
	}
	.totals {
		display: grid;
		gap: 16px;
	}
	dd {
		font-size: var(--text-lg);
		margin-top: 4px;
	}
	.langs {
		display: grid;
		grid-template-columns: minmax(0, 1fr) auto 40px;
		gap: 10px 12px;
		margin-top: 20px;
		padding-top: 16px;
		border-top: 1px solid var(--color-rule);
	}
	.form {
		display: grid;
		gap: 18px;
		max-width: 760px;
	}
	.row {
		display: grid;
		grid-template-columns: 180px minmax(0, 1fr);
		gap: 16px;
		align-items: baseline;
	}
	.check {
		display: flex;
		gap: 10px;
		align-items: center;
	}
	input[type='checkbox'] {
		accent-color: var(--color-ink);
	}
	.field[aria-invalid='true'] {
		border-color: var(--color-ink);
	}
	@media (max-width: 900px) {
		.g2 {
			grid-template-columns: 1fr;
		}
	}
	@media (max-width: 560px) {
		.row {
			grid-template-columns: 1fr;
			gap: 8px;
		}
	}
</style>

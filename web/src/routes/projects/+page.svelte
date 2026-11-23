<script lang="ts">
	import { resolve } from '$app/paths';
	import { app } from '#lib/app.svelte.ts';
	import { ago, duration } from '#lib/format.ts';
	import Panel from '#lib/components/Panel.svelte';
	import ProjectListHead from '#lib/components/ProjectListHead.svelte';
	import ProjectRow from '#lib/components/ProjectRow.svelte';
	import type { PageProps } from './$types';

	let { data }: PageProps = $props();

	const LIVE_MS = 3 * 60 * 1000;
	let query = $state('');
	let showArchived = $state(false);

	const archivedCount = $derived(data.projects.filter((p) => p.archived).length);
	const shown = $derived.by(() => {
		const q = query.trim().toLowerCase();
		return data.projects.filter(
			(p) => (showArchived || !p.archived) && (!q || p.name.toLowerCase().includes(q))
		);
	});
	const now = $derived(new Date(app.now));

	function note(p: (typeof data.projects)[number]): string | undefined {
		const parts = [];
		if (p.hidden) parts.push('hidden');
		if (p.archived) parts.push('archived');
		if (p.last_activity) parts.push(ago(p.last_activity, now));
		return parts.join(' · ') || undefined;
	}
</script>

<svelte:head><title>Projects · Logbook</title></svelte:head>

<h1 class="sr-only">Projects</h1>
<Panel title="Projects" meta="all time" padded={false}>
	<div class="tools">
		<label class="sr-only" for="q">Find a project</label>
		<input
			id="q"
			class="field w-full sm:max-w-[320px]"
			type="search"
			placeholder="Find a project"
			bind:value={query}
		/>
		{#if archivedCount > 0}
			<label class="lbl flex items-center gap-2">
				<input type="checkbox" bind:checked={showArchived} /> Show {archivedCount} archived
			</label>
		{/if}
	</div>
	<ProjectListHead />
	{#each shown as p (p.name)}
		<ProjectRow
			href={resolve('/projects/[name]', { name: encodeURIComponent(p.name) })}
			name={p.name}
			note={note(p)}
			keyboard={duration(p.total_seconds)}
			counted={p.credited_seconds === null ? '—' : duration(p.credited_seconds)}
			live={!!p.last_activity && app.now - Date.parse(p.last_activity) < LIVE_MS}
		/>
	{:else}
		<p class="mute px-4 py-[18px]">
			{data.projects.length === 0
				? 'No projects yet. A project appears once your editor or the agent records time in it.'
				: 'No project matches.'}
		</p>
	{/each}
	<p class="px-3.5 py-3">
		<span class="lbl">{shown.length} of {data.projects.length}.</span>
		<span class="hint">
			Counted is the time the agent credited to you; keyboard is raw editor time.
		</span>
	</p>
</Panel>

<style>
	.tools {
		display: flex;
		flex-wrap: wrap;
		gap: 12px 20px;
		align-items: center;
		padding: 12px 14px;
		border-bottom: 1px solid var(--color-rule);
	}
	input[type='checkbox'] {
		accent-color: var(--color-ink);
	}
</style>

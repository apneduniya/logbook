<!--
  The top projects over the chosen range; the one with activity in the last
  few minutes gets the live edge.
-->
<script lang="ts">
	import { resolve } from '$app/paths';
	import { app } from '#lib/app.svelte.ts';
	import { duration } from '#lib/format.ts';
	import type { Project, Summary } from '#lib/types.ts';
	import Panel from '#lib/components/Panel.svelte';
	import ProjectListHead from '#lib/components/ProjectListHead.svelte';
	import ProjectRow from '#lib/components/ProjectRow.svelte';

	let { summary, projects, meta }: { summary: Summary; projects: Project[]; meta: string } =
		$props();

	const LIVE_MS = 3 * 60 * 1000;

	const liveName = $derived.by(() => {
		const recent = projects.find(
			(p) => p.last_activity && app.now - Date.parse(p.last_activity) < LIVE_MS
		);
		return recent?.name ?? null;
	});
	const top = $derived(summary.projects.slice(0, 5));
	const total = $derived(projects.filter((p) => !p.archived).length);
</script>

<Panel title="Projects" {meta} padded={false}>
	{#if top.length === 0}
		<p class="mute px-4 py-[18px]">No project time in this range.</p>
	{:else}
		<ProjectListHead />
		{#each top as p (p.name)}
			<ProjectRow
				href={resolve('/projects/[name]', { name: encodeURIComponent(p.name) })}
				name={p.name}
				keyboard={duration(p.seconds)}
				counted={p.credited_seconds === null ? '—' : duration(p.credited_seconds)}
				live={p.name === liveName}
			/>
		{/each}
	{/if}
	<div class="px-3.5 py-3">
		<a class="link" href={resolve('/projects')}>
			All {total}
			{total === 1 ? 'project' : 'projects'} →
		</a>
	</div>
</Panel>

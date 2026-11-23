<script lang="ts">
	import Dashboard from '#lib/home/Dashboard.svelte';
	import FirstRun from '#lib/home/FirstRun.svelte';
	import History from '#lib/home/History.svelte';
	import Landing from '#lib/home/Landing.svelte';
	import type { PageProps } from './$types';

	let { data }: PageProps = $props();
</script>

{#if data.kind === 'signed-out'}
	<Landing />
{:else if data.kind === 'first-run'}
	<FirstRun today={data.today} hackatime={data.hackatime} devices={data.devices} />
{:else if data.kind === 'history'}
	<History
		range={data.range}
		year={data.year}
		summary={data.summary}
		projects={data.projects}
		hackatime={data.hackatime}
	/>
{:else}
	<Dashboard
		range={data.range}
		today={data.today}
		year={data.year}
		summary={data.summary}
		projects={data.projects}
	/>
{/if}

<script lang="ts">
	import Dashboard from '#lib/home/Dashboard.svelte';
	import FirstRun from '#lib/home/FirstRun.svelte';
	import History from '#lib/home/History.svelte';
	import Landing from '#lib/home/Landing.svelte';
	import { waitForDevice } from '#lib/home/setup.ts';
	import type { PageProps } from './$types';

	let { data }: PageProps = $props();

	// The install key lives here, not in FirstRun or History: this page stays
	// mounted across reloads, but a reload can swap those two (history landing
	// mid-setup), which would drop a key the user hasn't copied yet. It is
	// still memory only and gone on navigation (docs/web.md, Security).
	let installKey = $state<string | null>(null);
	const settingUp = $derived(data.kind === 'first-run' || data.kind === 'history');
	// Reading installKey here too means a key request that finishes after
	// sign-out or the dashboard (a late write through the binding) is cleared
	// straight away as well.
	$effect(() => {
		if (!settingUp && installKey !== null) installKey = null;
	});
	// Once a key exists, wait for the agent's first upload, then the dashboard.
	$effect(() => {
		if (settingUp && installKey !== null) return waitForDevice();
	});
</script>

{#if data.kind === 'signed-out'}
	<Landing />
{:else if data.kind === 'first-run'}
	<FirstRun today={data.today} hackatime={data.hackatime} devices={data.devices} bind:installKey />
{:else if data.kind === 'history'}
	<History
		range={data.range}
		year={data.year}
		summary={data.summary}
		projects={data.projects}
		hackatime={data.hackatime}
		bind:installKey
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

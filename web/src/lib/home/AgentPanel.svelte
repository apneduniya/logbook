<!--
  The user's agents: which computers are connected and when each last
  uploaded. Takes the B3 "Programs" slot until the program API exists.
-->
<script lang="ts">
	import { resolve } from '$app/paths';
	import { app } from '#lib/app.svelte.ts';
	import { ago } from '#lib/format.ts';
	import Panel from '#lib/components/Panel.svelte';

	const OS: Record<string, string> = { darwin: 'macOS', linux: 'Linux', windows: 'Windows' };

	const active = $derived((app.devices ?? []).filter((d) => !d.revoked_at));
	const now = $derived(new Date(app.now));
</script>

<Panel title="Agent" meta="{active.length} {active.length === 1 ? 'computer' : 'computers'}">
	{#if app.devices === null}
		<p class="mute">Loading…</p>
	{:else if active.length === 0}
		<div class="px mute tracking-[.06em]">( NO AGENT CONNECTED )</div>
		<p class="mute mt-3">Install the agent again from Settings to keep counting.</p>
	{:else}
		<ul>
			{#each active as d (d.id)}
				<li>
					<span class="truncate">{d.name}</span>
					<span class="lbl">
						{OS[d.os] ?? d.os} · {d.last_seen_at
							? `uploaded ${ago(d.last_seen_at, now)}`
							: 'no upload yet'}
					</span>
				</li>
			{/each}
		</ul>
	{/if}
	<a class="link mt-auto pt-4" href="{resolve('/settings')}#devices">Manage computers →</a>
</Panel>

<style>
	ul {
		list-style: none;
		padding: 0;
	}
	li {
		display: grid;
		padding: 10px 0;
		border-bottom: 1px solid var(--color-rule);
	}
	li:first-child {
		padding-top: 0;
	}
</style>

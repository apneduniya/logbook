<!--
  The Hackatime link and its history import (docs/api/hackatime.md), as a
  sentence plus the action that fits: link, link again, or nothing.
-->
<script lang="ts">
	import { app } from '#lib/app.svelte.ts';
	import { monthName, parseDate } from '#lib/format.ts';
	import type { HackatimeLink } from '#lib/types.ts';

	let { link }: { link: HackatimeLink } = $props();

	const linkUrl = app.api.url('/auth/hackatime/link');

	function monthYear(date: string): string {
		return `${monthName(date)} ${parseDate(date).getUTCFullYear()}`;
	}

	const view = $derived.by(() => {
		if (!link.linked) return { kind: 'unlinked' as const };
		if (link.trust_level === 'unknown') return { kind: 'relink' as const };
		const imp = link.import;
		if (!imp || imp.status === 'pending') return { kind: 'pending' as const };
		if (imp.status === 'failed') return { kind: 'failed' as const, error: imp.last_error };
		if (imp.status === 'running') {
			const pct = imp.days_total ? Math.floor((imp.days_done / imp.days_total) * 100) : 0;
			return { kind: 'running' as const, pct, recent: imp.recent_done };
		}
		return {
			kind: 'done' as const,
			range: `${monthYear(imp.from_date)} to ${monthYear(imp.to_date)}`,
			heartbeats: imp.heartbeats_imported,
			note: imp.last_error
		};
	});
</script>

{#if view.kind === 'unlinked'}
	<p class="mute">
		Link your Hackatime account to bring your history over. It shows as time at the keyboard; only
		the agent can count time from now on.
	</p>
	<a class="btn mt-3" href={linkUrl} rel="external">Link Hackatime</a>
{:else if view.kind === 'relink'}
	<p class="mute">
		Logbook can’t reach your Hackatime account any more, usually because access was revoked on
		Hackatime.
	</p>
	<a class="btn mt-3" href={linkUrl} rel="external">Link again</a>
{:else if view.kind === 'pending'}
	<p class="mute">Linked. The import starts in a moment.</p>
{:else if view.kind === 'running'}
	<p class="mute">
		Importing: <span class="mono text-ink">{view.pct}%</span>.
		{view.recent
			? 'The last 30 days are in; older history follows.'
			: 'The last 30 days come first.'}
	</p>
	<div
		class="bar mt-3"
		role="progressbar"
		aria-valuenow={view.pct}
		aria-valuemin="0"
		aria-valuemax="100"
		aria-label="Hackatime import"
	>
		<span style:width="{view.pct}%"></span>
	</div>
{:else if view.kind === 'failed'}
	<p class="mute">The import stopped: {view.error || 'Hackatime didn’t answer.'}</p>
	<a class="btn mt-3" href={linkUrl} rel="external">Link again to resume</a>
{:else}
	<p class="mute">
		<span class="mono text-ink">{view.heartbeats.toLocaleString('en')}</span> heartbeats imported,
		{view.range}. They show as time at the keyboard.
	</p>
	{#if view.note}<p class="hint mt-2">{view.note}</p>{/if}
{/if}

<style>
	.bar {
		height: 6px;
		background: var(--color-fill);
	}
	.bar span {
		display: block;
		height: 100%;
		background: var(--color-ink);
	}
</style>

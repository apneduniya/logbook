<script lang="ts">
	import { resolve } from '$app/paths';
	import { invalidate } from '$app/navigation';
	import { app, PROFILE } from '#lib/app.svelte.ts';
	import { duration, monthName, parseDate, percent } from '#lib/format.ts';
	import Identicon from '#lib/components/Identicon.svelte';
	import Panel from '#lib/components/Panel.svelte';
	import type { PageProps } from './$types';

	let { data }: PageProps = $props();
	const p = $derived(data.profile);

	// Refetch when the profile's numbers change (at most once a minute, server side).
	$effect(() => {
		if (!p) return;
		const src = app.api.livePublic(p.username);
		let opened = false;
		src.addEventListener('minutes', () => void invalidate(PROFILE));
		// Changes aren't replayed across a reconnect, so refetch after one.
		src.addEventListener('ready', () => {
			if (opened) void invalidate(PROFILE);
			opened = true;
		});
		// The server closes the stream when the profile goes private, and then
		// refuses to reopen it; refetching shows the 404 instead of stale data.
		src.addEventListener('error', () => {
			if (src.readyState === EventSource.CLOSED) void invalidate(PROFILE);
		});
		return () => src.close();
	});

	const since = $derived(
		p
			? `${monthName(p.member_since.slice(0, 10))} ${parseDate(p.member_since.slice(0, 10)).getUTCFullYear()}`
			: ''
	);
	const langTotal = $derived(p ? p.languages.reduce((s, l) => s + l.seconds, 0) : 0);
	const counted = $derived(p?.totals.credited_all_time_seconds ?? null);
</script>

<svelte:head>
	<title>{p ? `${p.display_name || p.username} · Logbook` : 'Profile · Logbook'}</title>
</svelte:head>

{#if !p}
	<Panel title="Your profile">
		<div class="px mute tracking-[.06em]">( PRIVATE )</div>
		<p class="mute mt-3">Your profile is private, so nobody else can see it.</p>
		<a class="btn mt-4 self-start" href={resolve('/settings')}>Make it public in Settings</a>
	</Panel>
{:else}
	<div class="head">
		<Identicon seed={p.username} size={48} />
		<div class="min-w-0">
			<h1 class="px">{p.display_name || p.username}</h1>
			<p class="lbl mt-1">@{p.username} · on Logbook since {since}</p>
		</div>
		{#if data.own}<a class="link ml-auto self-start" href={resolve('/settings')}>
				Edit profile
			</a>{/if}
	</div>

	<div class="figs">
		<div>
			<div class="lbl">Counted, all time</div>
			<div class="px fig">{counted === null ? '—' : duration(counted)}</div>
		</div>
		<div>
			<div class="lbl">Counted, last 7 days</div>
			<div class="px fig">
				{p.totals.credited_last_7_days_seconds === null
					? '—'
					: duration(p.totals.credited_last_7_days_seconds)}
			</div>
		</div>
		<div>
			<div class="lbl">At the keyboard, all time</div>
			<div class="mono fig-sm">{duration(p.totals.all_time_seconds)}</div>
		</div>
		<div>
			<div class="lbl">At the keyboard, last 7 days</div>
			<div class="mono fig-sm">{duration(p.totals.last_7_days_seconds)}</div>
		</div>
	</div>
	{#if counted === null}
		<p class="lbl mb-3">
			{p.display_name || p.username} hasn’t connected the Logbook agent yet, so nothing is counted; keyboard
			time comes from editor plugins.
		</p>
	{/if}

	<div class="g2">
		<Panel title="Projects" meta="top {p.projects.length}, all time" padded={false}>
			{#if p.projects.length}
				<div class="lbl prow head" aria-hidden="true">
					<span>Project</span><span class="text-right">Keyboard</span><span class="text-right">
						Counted
					</span>
				</div>
			{/if}
			{#each p.projects as pr (pr.name)}
				<div class="prow">
					<div class="min-w-0">
						<div class="truncate text-base">{pr.name}</div>
						{#if pr.repo_url.startsWith('https://github.com/')}
							<a class="lbl link" href={pr.repo_url} rel="external noopener noreferrer">
								{pr.repo_url.slice('https://github.com/'.length)} ↗
							</a>
						{/if}
					</div>
					<span class="mono kb">{duration(pr.total_seconds)}</span>
					<span class="mono">
						{pr.credited_seconds === null ? '—' : duration(pr.credited_seconds)}
					</span>
				</div>
			{:else}
				<p class="mute px-4 py-[18px]">No projects to show yet.</p>
			{/each}
		</Panel>
		<Panel title="Languages" meta="all time">
			{#if p.languages.length === 0}
				<p class="mute">No languages yet.</p>
			{:else}
				<div class="langs">
					{#each p.languages.slice(0, 8) as l, i (i)}
						<span class="truncate">{l.name || 'Unknown'}</span>
						<span class="mono">{duration(l.seconds)}</span>
						<span class="mono lbl text-right">{percent(l.seconds, langTotal)}%</span>
					{/each}
				</div>
			{/if}
		</Panel>
	</div>
{/if}

<style>
	.head {
		display: flex;
		gap: 16px;
		align-items: center;
		padding: 16px 0 22px;
	}
	h1 {
		font-weight: 400;
		font-size: var(--text-xl);
		letter-spacing: -0.01em;
		overflow-wrap: anywhere;
	}
	.figs {
		display: grid;
		grid-template-columns: repeat(4, minmax(0, 1fr));
		gap: 12px;
		padding: 18px 0;
		border-top: 1px solid var(--color-rule);
		border-bottom: 1px solid var(--color-rule);
		margin-bottom: 12px;
		align-items: start;
	}
	.fig {
		font-size: var(--text-2xl);
		line-height: 1;
		margin-top: 8px;
	}
	.fig-sm {
		font-size: var(--text-lg);
		margin-top: 8px;
	}
	.g2 {
		display: grid;
		grid-template-columns: calc((100% - 12px) * 8 / 12) 1fr;
		gap: 12px;
	}
	.prow {
		display: grid;
		grid-template-columns: minmax(0, 1fr) 72px 72px;
		gap: 16px;
		align-items: center;
		padding: 10px 14px;
		border-bottom: 1px solid var(--color-rule);
	}
	.prow .mono {
		text-align: right;
	}
	.prow.head {
		padding-top: 10px;
		padding-bottom: 10px;
	}
	.kb {
		color: var(--color-ink-3);
	}
	.langs {
		display: grid;
		grid-template-columns: minmax(0, 1fr) auto 40px;
		gap: 10px 12px;
	}
	@media (max-width: 900px) {
		.g2 {
			grid-template-columns: 1fr;
		}
		.figs {
			grid-template-columns: 1fr 1fr;
			row-gap: 20px;
		}
		.fig {
			font-size: 40px;
		}
	}
</style>

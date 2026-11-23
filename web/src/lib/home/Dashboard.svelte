<!-- The signed-in dashboard (B3 "Dashboard"): an 8/4 grid of hairline panels. -->
<script lang="ts">
	import { app } from '#lib/app.svelte.ts';
	import { instantLabel, zoneAbbr } from '#lib/format.ts';
	import type { Project, Range, Summary, Today } from '#lib/types.ts';
	import AgentPanel from './AgentPanel.svelte';
	import LanguagesPanel from './LanguagesPanel.svelte';
	import ProjectsPanel from './ProjectsPanel.svelte';
	import RangePanel from './RangePanel.svelte';
	import StreakPanel from './StreakPanel.svelte';
	import TodayPanel from './TodayPanel.svelte';
	import YearPanel from './YearPanel.svelte';

	interface Props {
		range: Range;
		today: Today;
		year: Summary;
		summary: Summary;
		projects: Project[];
	}
	let { range, today, year, summary, projects }: Props = $props();

	const rangeMeta = $derived(
		range === '7d' ? 'last 7 days' : range === '30d' ? 'last 30 days' : 'last year'
	);
	const lastActivity = $derived(
		projects.reduce<string | null>(
			(b, p) => (p.last_activity && (!b || p.last_activity > b) ? p.last_activity : b),
			null
		)
	);
</script>

<svelte:head><title>Today · Logbook</title></svelte:head>

<h1 class="sr-only">Your Logbook</h1>
<div class="dash">
	<div class="g2">
		<TodayPanel {today} />
		<StreakPanel current={today.streak.current} longest={today.streak.longest} days={year.days} />
	</div>
	<div class="g2">
		<RangePanel {range} {summary} {year} />
		<LanguagesPanel languages={summary.languages} editors={summary.editors} meta={rangeMeta} />
	</div>
	<div class="g2">
		<ProjectsPanel {summary} {projects} meta={rangeMeta} />
		<AgentPanel />
	</div>
	<YearPanel days={year.days} />
</div>

<footer class="lbl foot">
	<span role="status">
		{#if app.stale}
			Couldn’t refresh, showing earlier data. Retrying.
		{:else}
			{lastActivity
				? `Last activity ${instantLabel(lastActivity, app.timezone)}`
				: 'No activity yet'}
		{/if}
	</span>
	<span>Times in {zoneAbbr(app.timezone)}</span>
</footer>

<style>
	.dash {
		display: grid;
		gap: 12px;
	}
	.g2 {
		display: grid;
		grid-template-columns: calc((100% - 12px) * 8 / 12) 1fr;
		gap: 12px;
	}
	.foot {
		display: flex;
		justify-content: space-between;
		gap: 16px;
		padding: 12px 0 16px;
		margin-top: 12px;
		border-top: 1px solid var(--color-rule);
	}
	@media (max-width: 900px) {
		.g2 {
			grid-template-columns: 1fr;
		}
	}
</style>

<!--
  Signed in with history but no device yet (docs/web.md). The user's editor
  time so far, usually imported from Hackatime, is shown straight away and
  labelled "at the keyboard"; nothing raw is ever called counted. One strip on
  top holds the single next step: install the agent.
-->
<script lang="ts">
	import { app } from '#lib/app.svelte.ts';
	import { zoneAbbr } from '#lib/format.ts';
	import { RULES_URL } from '#lib/links.ts';
	import type { HackatimeLink, Project, Range, Summary } from '#lib/types.ts';
	import Panel from '#lib/components/Panel.svelte';
	import InstallCommand from '#lib/components/InstallCommand.svelte';
	import HackatimeStatus from '#lib/components/HackatimeStatus.svelte';
	import LanguagesPanel from './LanguagesPanel.svelte';
	import ProjectsPanel from './ProjectsPanel.svelte';
	import RangePanel from './RangePanel.svelte';
	import YearPanel from './YearPanel.svelte';
	import { importing } from './kind.ts';
	import { refreshWhileImporting, waitForDevice } from './setup.ts';

	interface Props {
		range: Range;
		year: Summary;
		summary: Summary;
		projects: Project[];
		hackatime: HackatimeLink;
	}
	let { range, year, summary, projects, hackatime }: Props = $props();

	const rangeMeta = $derived(
		range === '7d' ? 'last 7 days' : range === '30d' ? 'last 30 days' : 'last year'
	);
	const source = $derived(
		hackatime.linked ? 'imported from Hackatime' : 'from your editor plugins'
	);

	let waiting = $state(false);
	$effect(() => {
		if (waiting) return waitForDevice();
	});
	$effect(() => {
		if (importing(hackatime)) return refreshWhileImporting();
	});
</script>

<svelte:head><title>Your history · Logbook</title></svelte:head>

<div class="dash">
	<div class="g2">
		<Panel title="Start counting" meta={waiting ? 'waiting for the agent' : 'one step left'}>
			<div class="grid gap-3">
				<h1 class="px">Welcome, {app.firstName}.</h1>
				<p class="mute max-w-[62ch]">
					Below is your editor time so far, {source}. None of it is counted yet: counting starts
					once the agent runs on your computer.
					<a class="link" href={RULES_URL} rel="external">What counts →</a>
				</p>
				<InstallCommand onCreated={() => (waiting = true)} />
			</div>
		</Panel>
		<Panel title="Hackatime" meta={hackatime.linked ? 'linked' : 'not linked'}>
			<HackatimeStatus link={hackatime} />
		</Panel>
	</div>
	<div class="g2">
		<RangePanel {range} {summary} {year} />
		<LanguagesPanel languages={summary.languages} editors={summary.editors} meta={rangeMeta} />
	</div>
	<ProjectsPanel {summary} {projects} meta={rangeMeta} />
	<YearPanel days={year.days} />
</div>

<footer class="lbl foot">
	<span>All of this is time at the keyboard</span>
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
	h1 {
		font-weight: 400;
		font-size: var(--text-xl);
		letter-spacing: -0.01em;
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

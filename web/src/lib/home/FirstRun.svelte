<!--
  First run (B3 "First run"): shown until the user's first device registers.
  Polls for that device after an install command is created, then refreshes
  into the dashboard.
-->
<script lang="ts">
	import { app } from '#lib/app.svelte.ts';
	import { importing } from './kind.ts';
	import { waitForDevice, watchImport } from './setup.ts';
	import { dayLabel } from '#lib/format.ts';
	import { RULES_URL } from '#lib/links.ts';
	import type { Device, HackatimeLink, Today } from '#lib/types.ts';
	import Panel from '#lib/components/Panel.svelte';
	import InstallCommand from '#lib/components/InstallCommand.svelte';
	import HackatimeStatus from '#lib/components/HackatimeStatus.svelte';
	import Rules from './Rules.svelte';

	let { today, hackatime, devices }: { today: Today; hackatime: HackatimeLink; devices: Device[] } =
		$props();

	// The import's latest status between reloads (watchImport keeps it current).
	let latest = $state<HackatimeLink | null>(null);
	const link = $derived(latest ?? hackatime);
	const hackatimeDone = $derived(link.linked && link.import?.status === 'done');
	const done = $derived((hackatimeDone ? 1 : 0) + (devices.length > 0 ? 1 : 0));

	// After a key is made, wait for the agent's first upload (then the dashboard).
	let waiting = $state(false);
	$effect(() => {
		if (waiting) return waitForDevice();
	});
	// While Hackatime history comes in, keep its progress current and reload once
	// it has something to show (never while an install command is on screen).
	$effect(() => {
		latest = null;
		if (importing(hackatime))
			return watchImport(
				hackatime,
				(l) => (latest = l),
				() => !waiting
			);
	});
</script>

<svelte:head><title>Set up · Logbook</title></svelte:head>

<div class="g2">
	<Panel title="Set up" meta="{done} of 2 done" padded={false}>
		<div class="intro">
			<h1 class="px">Welcome, {app.firstName}.</h1>
			<p class="mute mt-1.5 max-w-[60ch]">
				One install and you’re done. After that there is nothing to start or stop: Logbook counts
				while you work.
			</p>
		</div>
		<ol class="steps">
			<li class="step">
				<span class="n px">1</span>
				<div class="min-w-0">
					<h2 class="text-base">Install the agent</h2>
					<p class="mute">
						A small background app. It runs on Windows, macOS and Linux, including old laptops.
					</p>
					<InstallCommand onCreated={() => (waiting = true)} />
				</div>
				<span class="st mute">{waiting ? 'waiting for it' : 'to do'}</span>
			</li>
			<li class="step">
				<span class="n px">2</span>
				<div>
					<h2 class="text-base">Keep your editor plugins</h2>
					<p class="mute">
						Your WakaTime and Hackatime plugins send to Logbook once the agent runs, and still reach
						Hackatime. Nothing to change.
					</p>
				</div>
				<span class="st mute">automatic</span>
			</li>
			<li class="step">
				<span class="n px">3</span>
				<div>
					<h2 class="text-base">Connect your AI tools</h2>
					<p class="mute">
						The agent finds Claude Code, Codex, Cursor, Copilot and others and connects to them.
					</p>
				</div>
				<span class="st mute">automatic</span>
			</li>
			<li class="step last">
				<span class="n px">4</span>
				<div>
					<h2 class="text-base">Bring your Hackatime history</h2>
					<div class="mt-0.5"><HackatimeStatus {link} /></div>
				</div>
				<span class="st" class:mute={!hackatimeDone}>
					{#if hackatimeDone}
						<svg width="12" height="10" viewBox="0 0 12 10" aria-hidden="true" class="check">
							<path d="M1 5l3.5 3.5L11 1" fill="none" stroke="currentColor" stroke-width="1.5" />
						</svg>done
					{:else}optional{/if}
				</span>
			</li>
		</ol>
	</Panel>
	<Panel title="What counts" meta="rules v1">
		<Rules />
		<a class="link mt-3.5" href={RULES_URL} rel="external">Read the rules →</a>
	</Panel>
</div>

<div class="g2 mt-3">
	<Panel title="Today" meta={dayLabel(today.end)}>
		<div class="empty">
			<div class="px mute tracking-[.06em]">( NO COUNTED TIME YET )</div>
			<div class="hint">Counted time shows up here a few minutes after you start working.</div>
		</div>
	</Panel>
	<Panel title="Programs" meta="hours you submit">
		<div class="px mute tracking-[.06em]">( COMING SOON )</div>
		<p class="mute mt-3">
			You’ll be able to send your hours to a You Ship, We Ship program for review from here.
		</p>
	</Panel>
</div>

<style>
	.g2 {
		display: grid;
		grid-template-columns: calc((100% - 12px) * 8 / 12) 1fr;
		gap: 12px;
	}
	.intro {
		padding: 18px 16px 18px;
		border-bottom: 1px solid var(--color-rule);
	}
	h1 {
		font-weight: 400;
		font-size: var(--text-xl);
		letter-spacing: -0.01em;
	}
	h2 {
		font-weight: 400;
	}
	.steps {
		list-style: none;
		padding: 0;
	}
	.step {
		display: grid;
		grid-template-columns: 28px minmax(0, 1fr) auto;
		gap: 14px;
		padding: 18px 16px;
		border-bottom: 1px solid var(--color-rule);
	}
	.step.last {
		border-bottom: 0;
	}
	.n {
		color: var(--color-ink-3);
	}
	.st {
		font-family: var(--font-mono);
		font-size: var(--text-xs);
		letter-spacing: 0.04em;
		white-space: nowrap;
	}
	.check {
		display: inline;
		margin-right: 6px;
		vertical-align: -1px;
	}
	.empty {
		display: grid;
		place-items: center;
		align-content: center;
		min-height: 150px;
		text-align: center;
		gap: 8px;
		flex: 1;
	}
	@media (max-width: 900px) {
		.g2 {
			grid-template-columns: 1fr;
		}
	}
	@media (max-width: 480px) {
		.step {
			grid-template-columns: 20px minmax(0, 1fr);
			gap: 10px;
		}
		.st {
			grid-column: 2;
			grid-row: 2;
		}
	}
</style>

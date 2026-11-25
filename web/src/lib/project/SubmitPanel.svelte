<!--
  "Submit these hours" on a project page (docs/web.md): the one
  `logbook submit …` command, with the project and a range filled in. The
  range defaults to the 30 UTC days ending yesterday, the latest day the
  agent accepts; a program from the public list adds --program. The page
  only builds the command: the agent shows what it will upload and asks
  before sending (docs/agent/submit.md).

  Props:
  - project: the project's name.
  - counted: its counted seconds of all time, null when nothing is counted.
  - programs: the public programs, or null when the list couldn't load.
-->
<script lang="ts">
	import { resolve } from '$app/paths';
	import { defaultSubmitRange, lastSealedDay, rangeProblem, submitCommand } from '#lib/submit.ts';
	import type { PublicProgram } from '#lib/types.ts';
	import CopyField from '#lib/components/CopyField.svelte';
	import Panel from '#lib/components/Panel.svelte';

	interface Props {
		project: string;
		counted: number | null;
		programs: PublicProgram[] | null;
	}
	let { project, counted, programs }: Props = $props();

	const shell = navigator.userAgent.includes('Windows') ? 'windows' : 'unix';
	const last = lastSealedDay();
	const start = defaultSubmitRange();

	let from = $state(start.from);
	let to = $state(start.to);
	let program = $state('');
	// The range's problem shows once a day has been changed or left, not mid-typing.
	let touched = $state(false);

	const problem = $derived(rangeProblem(from, to));
	const command = $derived(submitCommand({ project, from, to, program }, shell));
	const touch = () => (touched = true);
</script>

<Panel title="Submit these hours" meta="days in UTC" anchor="submit">
	{#if !counted}
		<p class="hint">
			Nothing to submit yet. Hours can be sent to a program once the agent has counted time in this
			project.
		</p>
	{:else}
		<div class="form">
			<div class="row">
				<span id="submit-days">Days</span>
				<div class="grid gap-1.5">
					<div class="range" role="group" aria-labelledby="submit-days">
						<label class="sr-only" for="submit-from">First day</label>
						<input
							id="submit-from"
							class="field"
							type="date"
							max={last}
							bind:value={from}
							onchange={touch}
							onblur={touch}
							aria-invalid={touched && !!problem}
							aria-describedby="submit-days-help"
						/>
						<span class="mute">to</span>
						<label class="sr-only" for="submit-to">Last day</label>
						<input
							id="submit-to"
							class="field"
							type="date"
							max={last}
							bind:value={to}
							onchange={touch}
							onblur={touch}
							aria-invalid={touched && !!problem}
							aria-describedby="submit-days-help"
						/>
					</div>
					<span id="submit-days-help" class="hint" class:text-ink={touched && !!problem}>
						{touched && problem
							? problem
							: 'Both days count. Yesterday is the latest: a day’s hours are sealed after midnight UTC.'}
					</span>
				</div>
			</div>
			{#if programs === null}
				<div class="row">
					<span>Program</span>
					<span class="hint">
						The list of programs didn’t load. Add <span class="mono">--program</span> and the program’s
						short name to send it to its reviewers.
					</span>
				</div>
			{:else if programs.length}
				<div class="row">
					<label for="submit-program">Program</label>
					<div class="grid gap-1.5">
						<select id="submit-program" class="field" bind:value={program}>
							<option value="">No program</option>
							{#each programs as p (p.slug)}<option value={p.slug}>{p.name}</option>{/each}
						</select>
						<span class="hint">
							Its reviewers get the submission. Without one, you share the review link yourself.
						</span>
					</div>
				</div>
			{/if}
			<div class="row">
				<span>Command</span>
				<div class="grid min-w-0 gap-1.5">
					<CopyField value={command} label="Submit command" />
					<span class="hint">
						Run it on the computer you worked on. It lists what it will upload and asks before
						sending. The review link then shows under
						<a class="link underline underline-offset-2" href={resolve('/submissions')}>
							Submissions
						</a>.
					</span>
				</div>
			</div>
		</div>
	{/if}
</Panel>

<style>
	.form {
		display: grid;
		gap: 18px;
		/* Wider than the settings form below: a command with a program fits on one line. */
		max-width: 900px;
	}
	.row {
		display: grid;
		grid-template-columns: 180px minmax(0, 1fr);
		gap: 16px;
		align-items: baseline;
	}
	.range {
		display: flex;
		flex-wrap: wrap;
		align-items: center;
		gap: 8px 10px;
	}
	select.field {
		max-width: 320px;
	}
	.field[aria-invalid='true'] {
		border-color: var(--color-ink);
	}
	@media (max-width: 900px) {
		.form {
			max-width: none;
		}
	}
	@media (max-width: 560px) {
		.row {
			grid-template-columns: 1fr;
			gap: 8px;
		}
	}
</style>

<!--
  The user's submissions as a run list (docs/web.md, "Submissions"): a status
  glyph, the project over its dates and program, counted hours with their
  evidence, the decision, then Copy link and Withdraw. A row opens its review
  page. While any submission is being checked the list looks again every
  10 s. Review links are copied, never printed.
-->
<script lang="ts">
	import { resolve } from '$app/paths';
	import { invalidate } from '$app/navigation';
	import { app, ME } from '#lib/app.svelte.ts';
	import {
		canCopyLink,
		canWithdraw,
		emptyFilterSentence,
		filterCounts,
		FILTERS,
		matchesFilter,
		verifyingKey,
		type SubmissionFilter
	} from '#lib/mine.ts';
	import { SvelteSet } from 'svelte/reactivity';
	import { poll } from '#lib/poll.ts';
	import { reviewPath } from '#lib/submissions.ts';
	import type { SubmissionSummary } from '#lib/types.ts';
	import ConfirmButton from '#lib/components/ConfirmButton.svelte';
	import CopyField from '#lib/components/CopyField.svelte';
	import Panel from '#lib/components/Panel.svelte';
	import Segmented from '#lib/components/Segmented.svelte';
	import SubmissionListHead from '#lib/components/SubmissionListHead.svelte';
	import SubmissionRow from '#lib/components/SubmissionRow.svelte';
	import type { PageProps } from './$types';

	let { data }: PageProps = $props();

	// Placeholders in caps, not <angle brackets>: pasted unchanged, a shell would read those as redirects.
	const TEMPLATE = 'logbook submit PROJECT --from YYYY-MM-DD --to YYYY-MM-DD --program PROGRAM';

	let filter = $state<SubmissionFilter>('all');
	/** Rows whose Withdraw is asking; their Copy link steps aside meanwhile. */
	const asking = new SvelteSet<string>();

	const all = $derived(data.submissions);
	const counts = $derived(filterCounts(all));
	const shown = $derived(all.filter((s) => matchesFilter(s, filter)));
	const options = $derived(
		FILTERS.map((f) => ({ value: f.value, label: `${f.label} ${counts[f.value]}` }))
	);
	const checking = $derived(verifyingKey(all));

	// While the server re-checks, look again every 10 s; reload once that changes.
	$effect(() => {
		if (!checking) return;
		const before = checking;
		return poll(10_000, async (alive) => {
			const list = await app.api.mySubmissions();
			if (!alive() || verifyingKey(list) === before) return false;
			await invalidate(ME);
			return true;
		});
	});

	const projects = resolve('/projects');

	function href(s: SubmissionSummary): string | undefined {
		const path = reviewPath(s.review_url);
		return path ? resolve('/r/[token]', { token: path.slice('/r/'.length) }) : undefined;
	}

	async function withdraw(s: SubmissionSummary) {
		await app.api.withdrawSubmission(s.id);
		await invalidate(ME);
	}
</script>

<svelte:head><title>Submissions · Logbook</title></svelte:head>

<h1 class="sr-only">Submissions</h1>
<Panel title="Submissions" meta={all.length ? 'newest first' : undefined} padded={false}>
	{#if all.length === 0}
		<div class="empty">
			<p class="px mute tracking-[.06em]">( NO SUBMISSIONS YET )</p>
			<p class="hint">
				To send your hours to a program, run this on the computer you worked on, with the project’s
				name, its first and last day (UTC) and the program. It shows what it will upload and asks
				before sending anything.
			</p>
			<div class="max-w-[720px] min-w-0">
				<CopyField value={TEMPLATE} label="Submit command" />
			</div>
			<p class="hint">
				Or open one of your <a class="link underline underline-offset-2" href={projects}>
					projects
				</a>: its page fills all of that in.
			</p>
		</div>
	{:else}
		<div class="tools">
			<Segmented
				label="Show"
				value={filter}
				onchange={(v: SubmissionFilter) => (filter = v)}
				{options}
			/>
		</div>
		<SubmissionListHead actions />
		{#each shown as s (s.id)}
			<SubmissionRow submission={s} href={href(s)}>
				{#snippet actions()}
					{#if canCopyLink(s) && s.review_url && !asking.has(s.id)}
						<CopyField
							bare
							value={s.review_url}
							label="Review link for {s.project}"
							button="Copy link"
						/>
					{/if}
					{#if canWithdraw(s)}
						<ConfirmButton
							label="Withdraw"
							ask="Withdraw it? The link stops working."
							confirm="Withdraw"
							align="start"
							run={() => withdraw(s)}
							onopenchange={(open) => (open ? asking.add(s.id) : asking.delete(s.id))}
						/>
					{/if}
				{/snippet}
			</SubmissionRow>
		{:else}
			{#if filter !== 'all'}
				<p class="hint px-3.5 py-[18px]">{emptyFilterSentence(filter)}</p>
			{/if}
		{/each}
		<p class="foot">
			<span class="lbl">{shown.length} of {all.length}.</span>
			<span class="hint">
				Paste a review link into your program’s form. Its reviewers open it to decide; withdrawing
				is possible until they do.
			</span>
		</p>
	{/if}
</Panel>

<style>
	.empty {
		display: grid;
		min-width: 0;
		gap: 14px;
		padding: 18px 16px;
	}
	.tools {
		display: flex;
		flex-wrap: wrap;
		gap: 12px 20px;
		align-items: center;
		padding: 12px 14px;
		border-bottom: 1px solid var(--color-rule);
	}
	.foot {
		padding: 12px 14px;
	}
</style>

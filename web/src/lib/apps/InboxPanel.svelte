<!--
  A program's inbox (docs/api/submissions.md, "The program's inbox"): the
  submissions addressed to the app, newest first, as SubmissionRows with the
  submitter column. A Segmented filter picks what to list ("To review" is
  ready with no decision yet); each row opens its review page, and "Copy
  link" copies the review link for the program's own tracker. It loads by
  itself, so changing the filter doesn't reload the page; while it loads the
  old rows stay at full strength and the tools row says "Loading…". On
  phones the six filters wrap onto two rows.

  Props: appId, slug (for the empty state's command).
-->
<script lang="ts">
	import { resolve } from '$app/paths';
	import { app } from '#lib/app.svelte.ts';
	import { ApiError } from '#lib/api.ts';
	import {
		INBOX_FILTERS,
		INBOX_PAGE,
		inboxEmpty,
		inboxQuery,
		type InboxFilter
	} from '#lib/apps.ts';
	import { reviewPath } from '#lib/submissions.ts';
	import CopyField from '#lib/components/CopyField.svelte';
	import Panel from '#lib/components/Panel.svelte';
	import Segmented from '#lib/components/Segmented.svelte';
	import SubmissionListHead from '#lib/components/SubmissionListHead.svelte';
	import SubmissionRow from '#lib/components/SubmissionRow.svelte';
	import type { SubmissionSummary } from '#lib/types.ts';

	let { appId, slug }: { appId: string; slug: string } = $props();

	let filter = $state<InboxFilter>('review');
	let rows = $state<SubmissionSummary[] | null>(null);
	let loading = $state(true);
	let more = $state(false);
	let error = $state<string | null>(null);
	let moreError = $state<string | null>(null);
	let loadingMore = $state(false);
	/** Bumped by "Try again" to run the load once more. */
	let attempt = $state(0);

	function message(err: unknown): string {
		return err instanceof ApiError ? err.userMessage : 'Couldn’t load the inbox.';
	}

	// Reload whenever the app or the filter changes; drop answers that arrive late.
	$effect(() => {
		const id = appId;
		const f = filter;
		void attempt;
		const ctl = new AbortController();
		loading = true;
		error = null;
		moreError = null;
		app.api
			.appSubmissions(id, inboxQuery(f), ctl.signal)
			.then((r) => {
				rows = r;
				more = r.length === INBOX_PAGE;
			})
			.catch((err) => {
				if (ctl.signal.aborted) return;
				rows = null;
				error = message(err);
			})
			.finally(() => {
				if (!ctl.signal.aborted) loading = false;
			});
		return () => ctl.abort();
	});

	async function loadMore() {
		if (!rows) return;
		const f = filter;
		loadingMore = true;
		moreError = null;
		try {
			const r = await app.api.appSubmissions(appId, inboxQuery(f, rows.length));
			if (f !== filter || !rows) return;
			// A submission may have moved up a page since; keep the first copy.
			const seen = new Set(rows.map((s) => s.id));
			rows = [...rows, ...r.filter((s) => !seen.has(s.id))];
			more = r.length === INBOX_PAGE;
		} catch (err) {
			moreError = message(err);
		} finally {
			loadingMore = false;
		}
	}

	function href(s: SubmissionSummary): string | undefined {
		const p = reviewPath(s.review_url);
		return p ? resolve('/r/[token]', { token: p.slice('/r/'.length) }) : undefined;
	}

	const count = $derived(
		rows === null
			? ''
			: `${rows.length}${more ? '+' : ''} ${rows.length === 1 ? 'submission' : 'submissions'}`
	);
</script>

<Panel title="Inbox" meta={count} padded={false} class="inbox">
	<div class="tools">
		<div class="seg">
			<Segmented
				options={INBOX_FILTERS}
				value={filter}
				label="Show submissions"
				onchange={(v: InboxFilter) => (filter = v)}
			/>
		</div>
		{#if loading && rows !== null && rows.length > 0}
			<span class="lbl" role="status">Loading…</span>
		{/if}
	</div>

	{#if error}
		<div class="px-[14px] py-[18px]">
			<p class="hint text-ink" role="alert">{error}</p>
			<button class="btn mt-3" type="button" onclick={() => attempt++}>Try again</button>
		</div>
	{:else if rows === null || (rows.length === 0 && loading)}
		<p class="lbl px-[14px] py-[18px]" aria-live="polite">Loading…</p>
	{:else if rows.length === 0}
		<div class="empty">
			<p class="px">{inboxEmpty(filter)}</p>
			<p class="hint">
				{#if filter === 'all'}
					Submissions arrive when people run
					<code class="mono">logbook submit &lt;project&gt; … --program {slug}</code>.
				{:else if filter === 'review'}
					Every ready submission has a decision. New ones show up here once Logbook has checked
					them.
				{:else}
					Nothing in this inbox matches.
				{/if}
			</p>
		</div>
	{:else}
		<div class="list" aria-busy={loading}>
			<SubmissionListHead submitter actions />
			{#each rows as s (s.id)}
				<SubmissionRow submission={s} href={href(s)} submitter>
					{#snippet actions()}
						{#if s.review_url}
							<CopyField
								bare
								value={s.review_url}
								label={s.user
									? `Review link for ${s.project} by ${s.user.username}`
									: `Review link for ${s.project}`}
								button="Copy link"
							/>
						{/if}
					{/snippet}
				</SubmissionRow>
			{/each}
		</div>
		{#if more}
			<div class="grid justify-items-start gap-2 px-[14px] py-3">
				<button class="btn" type="button" onclick={loadMore} disabled={loadingMore}>
					{loadingMore ? 'Loading…' : `Show ${INBOX_PAGE} more`}
				</button>
				{#if moreError}<span class="hint text-ink" role="alert">{moreError}</span>{/if}
			</div>
		{/if}
	{/if}
</Panel>

<style>
	.tools {
		display: flex;
		flex-wrap: wrap;
		align-items: center;
		gap: 12px 20px;
		min-width: 0;
		padding: 12px 14px;
		border-bottom: 1px solid var(--color-rule);
	}
	.seg {
		max-width: 100%;
	}
	.seg :global(button) {
		white-space: nowrap;
	}
	/* Six choices don't fit a phone's row: they wrap three and three, and the
	   control's border wraps with them. */
	@media (max-width: 640px) {
		.seg :global(.seg) {
			display: grid;
			grid-template-columns: repeat(3, auto);
			justify-content: start;
		}
	}
	.list > :global(:last-child) {
		border-bottom: 0;
	}
	.empty {
		display: grid;
		gap: 8px;
		padding: 22px 16px;
	}
	.empty .px {
		font-size: var(--text-sm);
		letter-spacing: 0.06em;
		color: var(--color-ink-2);
	}
</style>

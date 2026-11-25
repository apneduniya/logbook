<!--
  One submission as a run-list row (GitHub Actions' run list in the mkdir
  row style): an ink status glyph, the project over a mono dateline, counted
  hours with the evidence under them, the decision as a small stamp, then
  actions. With `href` the whole row opens it (a stretched link; actions
  stay clickable above it). A failed row says why under it when the API
  gives the reason (the submitter's own list only). Pair with
  SubmissionListHead, passing the same `submitter` and `actions`.

  Props:
  - submission: #lib/types.ts SubmissionSummary.
  - href: where the row goes (e.g. resolve('/r/[token]', …)), optional.
  - submitter: show the submitter column (a program's inbox).
  - actions: a snippet for the end of the row (Copy link, Withdraw).
-->
<script lang="ts">
	import type { Snippet } from 'svelte';
	import { dateRange, hours } from '#lib/format.ts';
	import { evidenceShort, STATUS_GLYPH, statusWord } from '#lib/submissions.ts';
	import type { SubmissionSummary } from '#lib/types.ts';
	import DecisionStamp from './DecisionStamp.svelte';

	interface Props {
		submission: SubmissionSummary;
		href?: string;
		submitter?: boolean;
		actions?: Snippet;
	}
	let { submission: s, href, submitter = false, actions }: Props = $props();

	const ready = $derived(s.status === 'ready');
	const who = $derived(s.user ? s.user.display_name || s.user.username : '');
	const status = $derived(
		{ verifying: 'Verifying', ready: 'Ready', failed: 'Failed', withdrawn: 'Withdrawn' }[s.status]
	);
</script>

<div class="sr" class:who-col={submitter} class:acts-col={!!actions}>
	<span
		class="g"
		class:dim={s.status === 'verifying' || s.status === 'withdrawn'}
		aria-hidden="true"
	>
		{STATUS_GLYPH[s.status]}
	</span>
	<div class="main">
		{#if href}
			<!-- eslint-disable-next-line svelte/no-navigation-without-resolve -- callers pass a resolve() result -->
			<a class="name" {href}>{s.project}</a>
		{:else}
			<span class="name">{s.project}</span>
		{/if}
		<span class="lbl dl">
			{dateRange(s.from, s.to)} · {s.program?.name ?? 'no program'}{#if s.user}<span
					class="who-inline"
				>
					{` · ${s.user.username}`}
				</span>{/if}<span class="sr-only">. {status}.</span>
		</span>
	</div>
	{#if submitter}
		<div class="who">
			{#if s.user}
				<span class="truncate">{who}</span>
				<span class="lbl truncate">{s.user.username}</span>
			{/if}
		</div>
	{/if}
	<div class="ct">
		{#if ready}
			<span class="mono hrs">{hours(s.counted_seconds)}<span class="sr-only"> counted</span></span>
			<span class="lbl ev">{evidenceShort(s.minutes)}</span>
		{:else}
			<span class="mono hrs dim" aria-hidden="true">—</span>
		{/if}
	</div>
	<div class="dec">
		{#if s.decision && s.status !== 'withdrawn'}
			<DecisionStamp decision={s.decision} size="sm" />
		{:else}
			<span class="lbl word">{statusWord(s)}</span>
		{/if}
	</div>
	{#if actions}
		<div class="acts">{@render actions()}</div>
	{/if}
	{#if s.status === 'failed' && s.error}
		<p class="hint why">{s.error}</p>
	{/if}
</div>

<style>
	.sr {
		--cols: 14px minmax(0, 1fr) 176px 150px;
		position: relative;
		display: grid;
		grid-template-columns: var(--cols);
		align-items: center;
		gap: 6px 16px;
		min-height: 60px;
		padding: 10px 14px;
		border-bottom: 1px solid var(--color-rule);
		transition: background 150ms ease-out;
	}
	.sr.who-col {
		--cols: 14px minmax(0, 1fr) 160px 176px 150px;
	}
	.sr.acts-col {
		--cols: 14px minmax(0, 1fr) 176px 150px 200px;
	}
	.sr.who-col.acts-col {
		--cols: 14px minmax(0, 1fr) 160px 176px 150px 200px;
	}
	.sr:has(.name:hover) {
		background: var(--color-fill);
	}
	.g {
		font-family: var(--font-mono);
		color: var(--color-ink);
		line-height: 1;
	}
	.dim {
		color: var(--color-ink-3);
	}
	.main,
	.who,
	.ct {
		display: grid;
		gap: 3px;
		min-width: 0;
	}
	.name {
		font-size: var(--text-base);
		color: var(--color-ink);
		text-decoration: none;
		white-space: nowrap;
		overflow: hidden;
		text-overflow: ellipsis;
	}
	/* The whole row is the link; the actions sit above it. */
	a.name::after {
		content: '';
		position: absolute;
		inset: 0;
	}
	a.name:focus-visible {
		outline: none;
	}
	a.name:focus-visible::after {
		outline: 1px solid var(--color-ink);
		outline-offset: -1px;
	}
	.dl {
		text-transform: uppercase;
		white-space: nowrap;
		overflow: hidden;
		text-overflow: ellipsis;
	}
	.who-inline {
		display: none;
	}
	.ct {
		text-align: right;
		justify-items: end;
	}
	.hrs,
	.ev {
		white-space: nowrap;
	}
	.hrs {
		font-size: var(--text-base);
	}
	.word {
		letter-spacing: 0.08em;
	}
	/* Actions start on the column's left edge, so the same button lines up
	   from row to row whatever the others offer. */
	.acts {
		position: relative;
		z-index: 1;
		display: flex;
		justify-content: flex-start;
		align-items: center;
		gap: 8px;
		flex-wrap: wrap;
	}
	.why {
		grid-column: 2 / -1;
		min-width: 0;
		color: var(--color-ink);
		/* The server's reason can hold 64-character hashes. */
		overflow-wrap: anywhere;
	}
	@media (max-width: 640px) {
		.sr,
		.sr.who-col,
		.sr.acts-col,
		.sr.who-col.acts-col {
			grid-template-columns: 14px minmax(0, 1fr) auto;
			gap: 8px 12px;
		}
		.who {
			display: none;
		}
		.who-inline {
			display: inline;
		}
		.dec,
		.acts {
			grid-column: 2 / -1;
		}
		/* Narrow columns wrap instead of cutting off the program's name. */
		.ct {
			max-width: 112px;
		}
		.ev,
		.dl {
			white-space: normal;
		}
		/* A failed row's reason reads before its actions. */
		.acts {
			order: 1;
		}
	}
</style>

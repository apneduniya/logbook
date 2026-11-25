<!--
  The review page (ADR 0020, docs/web.md "Review page"): an audited timesheet
  read top to bottom like an invoice. On desktop the document takes 8 columns
  and a rail of 4 holds trust, the justification and the decision; on phones
  everything stacks, and for program members the decision moves up under the
  hours. No ultramarine: nothing here is live except the verifying spinner.
  The token never appears in the title, in a link or in print.
-->
<script lang="ts">
	import { invalidate } from '$app/navigation';
	import { page } from '$app/state';
	import { ApiError } from '#lib/api.ts';
	import { app, REVIEW } from '#lib/app.svelte.ts';
	import { duration, percent } from '#lib/format.ts';
	import { poll } from '#lib/poll.ts';
	import { packetView, submissionMeta } from '#lib/review.ts';
	import CopyField from '#lib/components/CopyField.svelte';
	import LiveBadge from '#lib/components/LiveBadge.svelte';
	import Panel from '#lib/components/Panel.svelte';
	import CutsPanel from '#lib/review/CutsPanel.svelte';
	import DaysPanel from '#lib/review/DaysPanel.svelte';
	import DecisionPanel from '#lib/review/DecisionPanel.svelte';
	import type { PageProps } from './$types';

	let { data }: PageProps = $props();

	const token = $derived(page.params.token ?? '');
	const review = $derived(data.state === 'ok' ? data.review : null);
	const status = $derived(review?.submission.status ?? null);
	const meta = $derived(review ? submissionMeta(review) : []);
	const view = $derived(review?.packet ? packetView(review.packet) : null);
	const member = $derived(data.state === 'ok' && data.viewer.kind === 'member');

	// While the re-check runs, look again every 10 s and reload once it is done.
	$effect(() => {
		if (status !== 'verifying' || !token) return;
		const t = token;
		return poll(10_000, async (alive) => {
			let r;
			try {
				r = await app.api.review(t);
			} catch (err) {
				// Withdrawn (410) or gone (404) while verifying: show that, stop.
				if (!(err instanceof ApiError) || (err.status !== 410 && err.status !== 404)) throw err;
				if (alive()) await invalidate(REVIEW);
				return true;
			}
			if (!alive() || r.submission.status === 'verifying') return false;
			await invalidate(REVIEW);
			return true;
		});
	});

	const madeTotal = $derived(view ? view.madeOf.reduce((s, m) => s + m.seconds, 0) : 0);
	const title = $derived(
		review ? `Review: ${review.submission.project} · Logbook` : 'Review · Logbook'
	);
</script>

<svelte:head>
	<title>{title}</title>
	<!-- The link is a credential: keep it out of search engines and Referer headers. -->
	<meta name="robots" content="noindex, nofollow" />
	<meta name="referrer" content="no-referrer" />
</svelte:head>

{#if data.state === 'missing'}
	<div class="narrow">
		<Panel title="Review" meta="404">
			<h1 class="text-lg font-normal">There’s no submission at this link.</h1>
			<p class="hint mt-2">Check that you copied all of it. Links end after 22 characters.</p>
		</Panel>
	</div>
{:else if data.state === 'withdrawn'}
	<div class="narrow">
		<Panel title="Review" meta="410">
			<h1 class="text-lg font-normal">The submitter withdrew this submission.</h1>
			<p class="hint mt-2">There’s nothing to review, and no decision can be recorded.</p>
		</Panel>
	</div>
{:else if review && status === 'verifying'}
	<div class="narrow">
		<Panel title="Submission" meta="v{review.submission.version}">
			<h1 class="px name">{review.submission.project}</h1>
			<div class="mt-4"><LiveBadge label="( verifying )" /></div>
			<p class="hint mt-3">
				Logbook is re-computing every minute on the server from the signed device logs. This page
				updates on its own when that’s done, usually within a few minutes.
			</p>
			<dl class="meta mt-5">
				{#each meta as r (r.k)}
					<dt class="lbl">{r.k}</dt>
					<dd class:mono={r.mono}>{r.v}</dd>
				{/each}
			</dl>
		</Panel>
	</div>
{:else if review && status === 'failed'}
	<div class="narrow">
		<Panel title="Submission" meta="failed">
			<h1 class="px name">{review.submission.project}</h1>
			{#if data.state === 'ok' && data.viewer.kind === 'submitter'}
				<p class="mt-4">
					This submission failed its check, so reviewers see nothing to review.
					{#if data.failure}<span class="mute">{data.failure}</span>{/if}
				</p>
				<p class="hint mt-2">
					Run <code class="mono">logbook submit</code> again to send a new one.
				</p>
			{:else}
				<p class="mt-4">
					This submission failed its check: the evidence sent with it couldn’t be verified, so
					there’s nothing to review.
				</p>
				<p class="hint mt-2">The submitter has been told why and can submit again.</p>
			{/if}
			<dl class="meta mt-5">
				{#each meta as r (r.k)}
					<dt class="lbl">{r.k}</dt>
					<dd class:mono={r.mono}>{r.v}</dd>
				{/each}
			</dl>
		</Panel>
	</div>
{:else if review && view && data.state === 'ok'}
	<div class="review" class:member>
		<div class="doc">
			<div class="head">
				<Panel title="Submission" meta="v{review.submission.version}">
					<h1 class="px name">{review.submission.project}</h1>
					<dl class="meta mt-4">
						{#each meta as r (r.k)}
							<dt class="lbl">{r.k}</dt>
							<dd class:mono={r.mono}>{r.v}</dd>
						{/each}
					</dl>
				</Panel>

				<Panel title="Hours" meta="rules {review.packet?.rules_version}" padded={false}>
					<div class="figs">
						<div class="fig main">
							<span class="px big">{view.figures.counted}</span>
							<span class="lbl">counted</span>
						</div>
						<div class="fig">
							<span class="px mid">{view.figures.keyboard}</span>
							<span class="lbl">at the keyboard</span>
						</div>
						<div class="fig">
							<span class="px mid">{view.figures.oneThird}</span>
							<span class="lbl">one-third AI rule</span>
						</div>
					</div>
					<p class="hint pad">
						Counted is what the rules credit. The one-third rule is time at the keyboard with AI
						coding time counted at a third, for comparison.
					</p>
					<div class="evidence">
						<p>{view.evidence.sentence}</p>
						{#if view.evidence.clean}
							<span class="tag">VERIFIED</span>
						{:else}
							<a class="tag" href="#not-verified">{view.evidence.tag}</a>
						{/if}
					</div>
				</Panel>
			</div>

			<div class="body">
				<Panel title="Made of" meta={duration(madeTotal)}>
					{#if madeTotal === 0}
						<p class="hint">Nothing counted to split.</p>
					{:else}
						<div
							class="split"
							role="img"
							aria-label={view.madeOf.map((m) => `${m.label} ${duration(m.seconds)}`).join(', ')}
						>
							{#each view.madeOf as m (m.key)}
								{#if m.seconds}<span class={m.key} style:flex={m.seconds}></span>{/if}
							{/each}
						</div>
						<div class="legend">
							{#each view.madeOf as m (m.key)}
								<span>
									<i class="sw {m.key}"></i>{m.label}
									<span class="mono mute">{duration(m.seconds)}</span>
									<span class="lbl">{percent(m.seconds, madeTotal)}%</span>
								</span>
							{/each}
						</div>
						<p class="hint mt-3">
							Editing: the submitter typed in the project that minute. With AI: they prompted,
							answered or watched an AI agent working on it. Other: counted without either in the
							logs sent.
						</p>
					{/if}
				</Panel>

				<CutsPanel cuts={view.cuts} flags={view.flags} />

				<DaysPanel days={view.days} />

				<Panel
					title="AI sessions"
					meta={String(view.sessions.length)}
					padded={!view.sessions.length}
				>
					{#if !view.sessions.length}
						<p class="hint">No AI sessions in this range.</p>
					{:else}
						<table class="rows">
							<thead class="lbl">
								<tr>
									<th scope="col">Tool</th>
									<th scope="col" class="when">When (UTC)</th>
									<th scope="col" class="r opt">Prompts</th>
									<th scope="col" class="r">Counted</th>
									<th scope="col" class="r opt">Agent alone</th>
								</tr>
							</thead>
							<tbody>
								{#each view.sessions as s, i (i)}
									<tr>
										<td>
											<span class="block">{s.harness}</span>
											{#if s.model}<span class="lbl block">{s.model}</span>{/if}
											<span class="lbl phone block">{s.when}</span>
										</td>
										<td class="mono when">{s.when}</td>
										<td class="mono r opt">{s.prompts}</td>
										<td class="mono r">{s.counted}</td>
										<td class="mono r opt">{s.alone}</td>
									</tr>
								{/each}
							</tbody>
						</table>
					{/if}
				</Panel>

				{#if !view.evidence.clean}
					<Panel
						title="Not verified"
						meta="{view.evidence.mismatch + view.evidence.unbacked} min"
						anchor="not-verified"
						padded={false}
					>
						<div class="grid gap-2 px-[14px] py-[14px]">
							{#if view.evidence.mismatch}
								<p class="hint text-ink">
									{view.evidence.mismatch.toLocaleString('en-US')}
									{view.evidence.mismatch === 1 ? 'minute' : 'minutes'} came out differently when the
									server replayed the device’s signed log.
									{#if view.mismatches.length < view.evidence.mismatch}
										The newest {view.mismatches.length} are below.
									{/if}
								</p>
							{/if}
							{#if view.evidence.unbacked}
								<p class="hint">
									{view.evidence.unbacked.toLocaleString('en-US')}
									{view.evidence.unbacked === 1 ? 'minute is' : 'minutes are'} not yet backed by evidence:
									no device sent a proof that covers them. They’re in the counted time as the device reported
									them.
								</p>
							{/if}
						</div>
						{#if view.mismatches.length}
							<table class="rows">
								<thead class="lbl">
									<tr>
										<th scope="col">Minute (UTC)</th>
										<th scope="col">Device said</th>
										<th scope="col">Server got</th>
									</tr>
								</thead>
								<tbody>
									{#each view.mismatches as m, i (i)}
										<tr>
											<td class="mono">{m.when}</td>
											<td class="mono">{m.device}</td>
											<td class="mono">{m.server}</td>
										</tr>
									{/each}
								</tbody>
							</table>
						{/if}
					</Panel>
				{/if}
			</div>
		</div>

		<div class="rail">
			<Panel title="Trust" meta="Hackatime" class="trust">
				<dl class="meta">
					{#each view.trust.rows as r (r.k)}
						<dt class="lbl">{r.k}</dt>
						<dd class:mono={r.mono}>{r.v}</dd>
					{/each}
				</dl>
				{#each view.trust.warnings as w (w)}
					<p class="warn mt-3">{w}</p>
				{/each}
			</Panel>

			{#if view.justification}
				<Panel title="Justification" class="just">
					<CopyField
						value={view.justification.copy}
						display={view.justification.display}
						label="Justification"
						wrap
					/>
					<p class="hint mt-2">
						For the program’s justification field. Copy includes the full review link.
					</p>
				</Panel>
			{/if}

			<DecisionPanel
				class="decision"
				{token}
				{review}
				viewer={data.viewer}
				appsError={data.appsError}
			/>
		</div>
	</div>
{/if}

<style>
	.narrow {
		max-width: 640px;
		padding-top: 12px;
	}
	.name {
		font-weight: 400;
		font-size: var(--text-xl);
		line-height: 1.05;
		letter-spacing: -0.01em;
		overflow-wrap: anywhere;
	}
	/* Invoice meta: labels left, values right of them, on one baseline. */
	.meta {
		display: grid;
		grid-template-columns: max-content minmax(0, 1fr);
		gap: 6px 20px;
		align-items: baseline;
	}
	.meta dd {
		overflow-wrap: anywhere;
	}

	.review {
		display: grid;
		grid-template-columns: calc((100% - 12px) * 8 / 12) minmax(0, 1fr);
		gap: 12px;
		align-items: start;
		padding-top: 12px;
	}
	.doc,
	.head,
	.body,
	.rail {
		display: flex;
		flex-direction: column;
		gap: 12px;
		min-width: 0;
	}

	/* The headline: three figures in hairline cells, counted the biggest. */
	.figs {
		display: grid;
		grid-template-columns: 1.4fr 1fr 1fr;
	}
	.fig {
		display: grid;
		align-content: end;
		gap: 8px;
		padding: 18px 16px 16px;
		border-left: 1px solid var(--color-rule);
		min-width: 0;
	}
	.fig.main {
		border-left: 0;
	}
	.big {
		font-size: var(--text-2xl);
		line-height: 0.9;
		letter-spacing: -0.02em;
		white-space: nowrap;
	}
	.mid {
		font-size: var(--text-xl);
		line-height: 0.95;
		white-space: nowrap;
	}
	.pad {
		padding: 0 16px 16px;
	}
	.evidence {
		display: flex;
		flex-wrap: wrap;
		align-items: center;
		gap: 10px 16px;
		padding: 12px 16px;
		border-top: 1px solid var(--color-rule);
	}
	.evidence p {
		max-width: 62ch;
	}
	/* A bordered seal, like a Verified badge: ink, never a colour. */
	.tag {
		font-family: var(--font-pixel);
		font-size: var(--text-xs);
		letter-spacing: 0.08em;
		padding: 3px 7px 2px;
		border: 1px solid var(--color-ink);
		color: var(--color-ink);
		text-decoration: none;
		white-space: nowrap;
		transition:
			background 150ms ease-out,
			color 150ms ease-out;
	}
	a.tag:hover {
		background: var(--color-ink);
		color: var(--color-ground);
	}

	/* MADE OF: the TodayPanel split bar. */
	.split {
		display: flex;
		height: 14px;
		gap: 2px;
	}
	.editing {
		background: var(--color-ink);
	}
	.with_ai {
		background: repeating-linear-gradient(135deg, var(--color-ink-2) 0 2px, transparent 2px 4px);
	}
	.other {
		background: var(--color-fill);
	}
	.sw.other {
		box-shadow: inset 0 0 0 1px var(--color-ink-3);
	}
	.legend {
		display: flex;
		flex-wrap: wrap;
		gap: 8px 24px;
		margin-top: 12px;
	}

	/* Table rows on hairlines, as ProjectRow lists. */
	.rows {
		width: 100%;
		border-collapse: collapse;
	}
	.rows th {
		font-weight: 400;
		text-align: left;
		padding: 10px 14px;
		border-bottom: 1px solid var(--color-rule);
	}
	.rows td {
		padding: 10px 14px;
		border-bottom: 1px solid var(--color-rule);
		vertical-align: baseline;
		font-size: var(--text-sm);
	}
	.rows tr:last-child td {
		border-bottom: 0;
	}
	.rows .r {
		text-align: right;
		white-space: nowrap;
	}
	.rows .when {
		white-space: nowrap;
	}
	.phone {
		display: none;
	}

	.warn {
		border-left: 2px solid var(--color-ink);
		padding-left: 12px;
		max-width: 60ch;
	}

	@media (max-width: 900px) {
		/* One column. The wrappers step aside so each block can take its place:
		   the decision comes up under the hours for members. */
		.review {
			display: flex;
			flex-direction: column;
			align-items: stretch;
		}
		.doc,
		.rail {
			display: contents;
		}
		.head {
			order: 1;
		}
		.body {
			order: 3;
		}
		.rail > :global(.trust) {
			order: 4;
		}
		.rail > :global(.just) {
			order: 5;
		}
		.rail > :global(.decision) {
			order: 6;
		}
		.review.member .rail > :global(.decision) {
			order: 2;
		}
	}
	@media (max-width: 640px) {
		.figs {
			grid-template-columns: 1fr 1fr;
		}
		.fig.main {
			grid-column: 1 / -1;
			border-bottom: 1px solid var(--color-rule);
		}
		.fig:nth-child(2) {
			border-left: 0;
		}
		.mid {
			font-size: var(--text-lg);
		}
		.rows .opt,
		.rows .when {
			display: none;
		}
		.phone {
			display: block;
		}
	}
</style>

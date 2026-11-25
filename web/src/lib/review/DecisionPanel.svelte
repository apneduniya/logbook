<!--
  The review page's DECISION panel. A recorded decision is a stamp. Program
  members (owner or reviewer) also get Approve, Adjust and Reject, each of
  which asks once inline: Approve takes no reason (the API refuses one),
  Adjust takes hours below the counted time and a reason, Reject a reason.
  Every decision sends the packet version the page read, so a packet checked
  again since is refused (409) and the panel offers a reload instead.
  After a decision, focus moves to the stamp, so a screen reader reads the
  verdict. The page may hold two copies (rail and, on phones, under the
  hours), so the recorded decision lives with the page (`current`,
  `ondecided`).
-->
<script lang="ts">
	import { tick } from 'svelte';
	import { invalidate } from '$app/navigation';
	import { app, REVIEW } from '#lib/app.svelte.ts';
	import { ApiError } from '#lib/api.ts';
	import { hours } from '#lib/format.ts';
	import { decisionError, type Viewer } from '#lib/review.ts';
	import type { Decision, DecisionInput, Review } from '#lib/types.ts';
	import ConfirmButton from '#lib/components/ConfirmButton.svelte';
	import DecisionStamp from '#lib/components/DecisionStamp.svelte';
	import Panel from '#lib/components/Panel.svelte';
	import ReasonConfirm from '#lib/components/ReasonConfirm.svelte';

	interface Props {
		token: string;
		review: Review;
		viewer: Viewer;
		/** Why membership couldn't be checked, if it couldn't. */
		appsError: string | null;
		/** The decision on screen: the loaded one, or the one just recorded. */
		current: Decision | null;
		ondecided: (d: Decision) => void;
		class?: string;
	}
	let { token, review, viewer, appsError, current, ondecided, class: cls = '' }: Props = $props();

	const sub = $derived(review.submission);
	const counted = $derived(review.packet?.counted_seconds ?? 0);
	const program = $derived(sub.program?.name ?? '');

	let changing = $state(false);
	let open = $state<'approve' | 'adjust' | 'reject' | null>(null);
	let stale = $state(false);
	let signedOut = $state(false);
	let reloading = $state(false);
	let stampEl = $state<HTMLElement | null>(null);
	let actsEl = $state<HTMLElement | null>(null);
	let recordEl = $state<HTMLButtonElement | null>(null);

	const member = $derived(viewer.kind === 'member');
	const controls = $derived(member && sub.status === 'ready' && (!current || changing));

	async function decide(input: DecisionInput): Promise<void> {
		try {
			const d = await app.api.decide(token, { ...input, version: sub.version });
			ondecided(d);
			changing = false;
			open = null;
		} catch (err) {
			const d = decisionError(err);
			if (d.stale) stale = true;
			if (err instanceof ApiError && err.status === 401) signedOut = true;
			// Shown by the control that asked (ConfirmButton, ReasonConfirm); a
			// stale packet is explained once, by the Reload button below.
			throw new ApiError(
				err instanceof ApiError ? err.status : 400,
				d.stale ? 'Not recorded.' : d.message,
				err instanceof ApiError ? err.retryAfter : null
			);
		}
		await tick();
		stampEl?.focus();
	}

	async function change() {
		changing = true;
		await tick();
		actsEl?.querySelector('button')?.focus();
	}

	async function keep() {
		changing = false;
		await tick();
		recordEl?.focus();
	}

	async function reload() {
		reloading = true;
		try {
			await invalidate(REVIEW);
			stale = false;
		} finally {
			reloading = false;
		}
	}

	type Asked = { reason: string; seconds: number | null };
	const approve = () => decide({ value: 'approved' });
	const adjust = ({ reason, seconds }: Asked) =>
		decide({ value: 'adjusted', approved_seconds: seconds ?? 0, reason });
	const reject = ({ reason }: Asked) => decide({ value: 'rejected', reason });

	const toggle = (which: 'approve' | 'adjust' | 'reject') => (o: boolean) => {
		open = o ? which : null;
	};
</script>

<Panel title="Decision" meta={current ? current.value : 'none yet'} class={cls}>
	<div class="grid gap-4">
		{#if !sub.program}
			<p class="hint">This submission isn’t addressed to a program, so nobody decides on it.</p>
		{:else if current && !changing}
			<div tabindex="-1" bind:this={stampEl} class="outline-none">
				<DecisionStamp decision={current} />
			</div>
			{#if member && sub.status === 'ready'}
				<button bind:this={recordEl} class="btn justify-self-start" type="button" onclick={change}>
					Record a new decision
				</button>
			{/if}
		{:else if !current && !member}
			<p class="hint">
				{#if viewer.kind === 'submitter'}
					No decision yet. {program}’s reviewers decide here, and you’ll see it on this page.
				{:else if viewer.kind === 'signed-out'}
					No decision yet. Reviewing for {program}? Sign in to decide.
				{:else}
					No decision yet. {program}’s owner and reviewers decide here.
				{/if}
			</p>
			{#if viewer.kind === 'signed-out'}
				<button class="btn justify-self-start" type="button" onclick={() => app.signIn()}>
					Sign in
				</button>
			{:else if appsError}
				<p class="hint">Couldn’t check whether you review for {program}: {appsError}</p>
			{/if}
		{/if}

		{#if controls}
			<p class="hint">
				{current ? 'The newest decision is the one that counts. ' : ''}The submitter sees your
				decision and reason.
			</p>
			<div class="acts" bind:this={actsEl}>
				{#if open === null || open === 'approve'}
					<ConfirmButton
						label="Approve"
						ask="Approve all {hours(counted)}?"
						confirm="Approve"
						align="start"
						onopenchange={toggle('approve')}
						run={approve}
					/>
				{/if}
				{#if open === null || open === 'adjust'}
					<ReasonConfirm
						label="Adjust"
						ask="Approve fewer hours?"
						confirm="Adjust"
						hours={{
							label: 'Hours to approve',
							hint: `Less than the ${hours(counted)} counted.`,
							below: counted
						}}
						reasonHint="Say what you took off and why."
						onopenchange={toggle('adjust')}
						run={adjust}
					/>
				{/if}
				{#if open === null || open === 'reject'}
					<ReasonConfirm
						label="Reject"
						ask="Reject these hours?"
						confirm="Reject"
						reasonHint="Say why, so the submitter can fix it."
						onopenchange={toggle('reject')}
						run={reject}
					/>
				{/if}
			</div>
			{#if changing && open === null}
				<button class="link justify-self-start" type="button" onclick={keep}>
					Keep the current decision
				</button>
			{/if}
		{/if}

		{#if stale}
			<div class="grid justify-items-start gap-2">
				<p class="hint text-ink">
					This submission was checked again since you read it. Reload to see the new numbers.
				</p>
				<button class="btn" type="button" onclick={reload} disabled={reloading}>
					{reloading ? 'Reloading…' : 'Reload'}
				</button>
			</div>
		{/if}
		{#if signedOut}
			<button class="btn justify-self-start" type="button" onclick={() => app.signIn()}>
				Sign in again
			</button>
		{/if}
	</div>
</Panel>

<style>
	.acts {
		display: flex;
		flex-wrap: wrap;
		gap: 8px;
		align-items: flex-start;
	}
	.acts > :global(form) {
		flex-basis: 100%;
	}
	.link {
		background: none;
		border: 0;
		padding: 0;
		cursor: pointer;
		font: inherit;
	}
</style>

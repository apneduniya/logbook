<!--
  ConfirmButton with a reason: the first press opens, inline and never in a
  modal, a reason field (and, with `hours`, an hours field), the second does
  it. Fields are checked on submit, not while typing, and the confirm button
  is never disabled to signal bad input (web/DESIGN.md). Cancel or Escape
  backs out and returns focus to the trigger. A failure keeps the form and
  what was typed, with its message under the buttons.

  Props:
  - label: the trigger's word ("Reject").
  - ask: the question at the top of the form ("Reject these hours?").
  - confirm: the submit button's word (default: label).
  - reasonHint: a sentence under the reason field.
  - hours: { label, hint, below }: also ask for hours, which must be at
    least 0 and below `below` seconds; run gets them as whole seconds.
  - run({ reason, seconds }): does it; throw an ApiError to show its message.
  - onopenchange(open): lets a group hide its other actions while this asks.
-->
<script lang="ts">
	import { tick } from 'svelte';
	import { ApiError } from '#lib/api.ts';
	import { checkReason, MAX_REASON, parseHours, reasonLength } from '#lib/reason.ts';

	interface Props {
		label: string;
		ask: string;
		confirm?: string;
		reasonHint?: string;
		hours?: { label: string; hint: string; below: number };
		run: (input: { reason: string; seconds: number | null }) => Promise<void>;
		onopenchange?: (open: boolean) => void;
	}
	let { label, ask, confirm, reasonHint, hours, run, onopenchange }: Props = $props();

	const id = $props.id();
	let open = $state(false);
	let busy = $state(false);
	let tried = $state(false);
	let reason = $state('');
	let hoursText = $state('');
	let error = $state<string | null>(null);
	let trigger = $state<HTMLButtonElement | null>(null);
	let hoursInput = $state<HTMLInputElement | null>(null);
	let reasonInput = $state<HTMLTextAreaElement | null>(null);

	const reasonError = $derived(tried ? checkReason(reason) : null);
	const parsed = $derived(hours ? parseHours(hoursText, hours.below) : null);
	const hoursError = $derived(tried && parsed ? parsed.error : null);
	const count = $derived(reasonLength(reason));

	async function start() {
		error = null;
		open = true;
		onopenchange?.(true);
		await tick();
		(hoursInput ?? reasonInput)?.focus();
	}

	async function back() {
		open = false;
		tried = false;
		error = null;
		onopenchange?.(false);
		await tick();
		trigger?.focus();
	}

	async function submit(e: SubmitEvent) {
		e.preventDefault();
		tried = true;
		error = null;
		if (hoursError || reasonError) {
			(hoursError ? hoursInput : reasonInput)?.focus();
			return;
		}
		busy = true;
		try {
			await run({ reason: reason.trim(), seconds: parsed?.seconds ?? null });
			open = false;
			tried = false;
			reason = '';
			hoursText = '';
			onopenchange?.(false);
		} catch (err) {
			error = err instanceof ApiError ? err.userMessage : 'That didn’t work. Try again.';
		} finally {
			busy = false;
		}
	}

	function onkeydown(e: KeyboardEvent) {
		if (e.key === 'Escape' && !busy) {
			e.preventDefault();
			void back();
		}
	}
</script>

{#if open}
	<form class="rc" aria-labelledby="{id}-ask" onsubmit={submit} novalidate>
		<p id="{id}-ask" class="ask">{ask}</p>
		{#if hours}
			<div class="fieldset">
				<label for="{id}-h">{hours.label}</label>
				<input
					bind:this={hoursInput}
					bind:value={hoursText}
					id="{id}-h"
					class="field w-32"
					type="text"
					inputmode="decimal"
					autocomplete="off"
					aria-invalid={hoursError !== null}
					aria-describedby="{id}-hh"
					{onkeydown}
				/>
				<span id="{id}-hh" class="hint" class:bad={hoursError}>{hoursError ?? hours.hint}</span>
			</div>
		{/if}
		<div class="fieldset">
			<label for="{id}-r">Reason</label>
			<textarea
				bind:this={reasonInput}
				bind:value={reason}
				id="{id}-r"
				class="field area"
				rows="3"
				aria-invalid={reasonError !== null}
				aria-describedby="{id}-rh {id}-rc"
				{onkeydown}></textarea>
			<span class="flex flex-wrap justify-between gap-x-4 gap-y-1">
				<span id="{id}-rh" class="hint" class:bad={reasonError}>
					{reasonError ?? reasonHint ?? ''}
				</span>
				<span id="{id}-rc" class="lbl" class:text-ink={count > MAX_REASON}>
					{count}/{MAX_REASON}
				</span>
			</span>
		</div>
		<div class="flex flex-wrap items-center gap-3">
			<button class="btn" type="submit" disabled={busy} {onkeydown}>
				{busy ? 'Working…' : (confirm ?? label)}
			</button>
			<button class="link cancel" type="button" onclick={back} disabled={busy} {onkeydown}>
				Cancel
			</button>
		</div>
		{#if error}<p class="hint bad" role="alert">{error}</p>{/if}
	</form>
{:else}
	<button bind:this={trigger} class="btn" type="button" onclick={start}>{label}</button>
{/if}

<style>
	.rc {
		display: grid;
		gap: 14px;
		width: 100%;
	}
	.ask {
		color: var(--color-ink);
		font-size: var(--text-base);
	}
	.fieldset {
		display: grid;
		gap: 6px;
		justify-items: start;
	}
	.fieldset > label {
		color: var(--color-ink);
	}
	.area {
		width: 100%;
		height: auto;
		min-height: 76px;
		padding: 8px 10px;
		line-height: 1.45;
		resize: vertical;
	}
	.cancel {
		background: none;
		border: 0;
		padding: 0;
		cursor: pointer;
		font: inherit;
	}
	.bad {
		color: var(--color-ink);
	}
</style>

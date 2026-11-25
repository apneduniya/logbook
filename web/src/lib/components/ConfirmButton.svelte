<!--
  A destructive action in two steps: the first press asks, the second does it.
  Focus moves to the confirm button; Cancel or Escape backs out and returns
  focus to the trigger. A failure shows its message right here, by the row.
  `align="start"` keeps it on its panel's left edge (a rail, a form) instead
  of the right end of a row; `onopenchange` lets a group hide its other
  actions while this one asks. `name` gives the trigger a fuller accessible
  name in lists of alike rows ("Withdraw terra-bot, 1–20 Nov"); it starts
  with the visible word. The confirm button and Cancel wrap as a pair.
-->
<script lang="ts">
	import { tick } from 'svelte';
	import { ApiError } from '#lib/api.ts';

	interface Props {
		label: string;
		/** The question shown on the first press, e.g. "Revoke this key?" */
		ask: string;
		confirm?: string;
		run: () => Promise<void>;
		align?: 'start' | 'end';
		onopenchange?: (open: boolean) => void;
		/** The trigger's accessible name, when the visible word alone is ambiguous. */
		name?: string;
	}
	let { label, ask, confirm = 'Yes', run, align = 'end', onopenchange, name }: Props = $props();

	let asking = $state(false);
	let busy = $state(false);
	let error = $state<string | null>(null);
	let yes = $state<HTMLButtonElement | null>(null);
	let trigger = $state<HTMLButtonElement | null>(null);

	async function start() {
		error = null;
		asking = true;
		onopenchange?.(true);
		await tick();
		yes?.focus();
	}

	async function back() {
		asking = false;
		onopenchange?.(false);
		await tick();
		trigger?.focus();
	}

	async function go() {
		busy = true;
		error = null;
		try {
			await run();
			asking = false;
			onopenchange?.(false);
		} catch (err) {
			error = err instanceof ApiError ? err.userMessage : 'That didn’t work. Try again.';
			await back();
		} finally {
			busy = false;
		}
	}

	function onkeydown(e: KeyboardEvent) {
		if (e.key === 'Escape' && !busy) void back();
	}
</script>

<span class="wrap" class:start={align === 'start'}>
	{#if asking}
		<span class="ask" role="group" aria-label={ask}>
			<span class="hint">{ask}</span>
			<span class="pair">
				<button bind:this={yes} class="btn" type="button" onclick={go} disabled={busy} {onkeydown}>
					{busy ? 'Working…' : confirm}
				</button>
				<button class="link cancel" type="button" onclick={back} disabled={busy} {onkeydown}>
					Cancel
				</button>
			</span>
		</span>
	{:else}
		<button bind:this={trigger} class="btn" type="button" onclick={start} aria-label={name}>
			{label}
		</button>
	{/if}
	{#if error}<span class="hint err" role="alert">{error}</span>{/if}
</span>

<style>
	.wrap {
		display: inline-grid;
		justify-items: end;
		gap: 6px;
	}
	.wrap.start {
		justify-items: start;
	}
	.ask {
		display: inline-flex;
		align-items: center;
		gap: 12px;
		flex-wrap: wrap;
	}
	.pair {
		display: inline-flex;
		align-items: center;
		gap: 12px;
		white-space: nowrap;
	}
	.cancel {
		background: none;
		border: 0;
		padding: 0;
		cursor: pointer;
		font: inherit;
	}
	.err {
		color: var(--color-ink);
	}
	@media (max-width: 560px) {
		.wrap {
			justify-items: start;
		}
	}
</style>

<!--
  A destructive action in two steps: the first press asks, the second does it.
  Focus moves to the confirm button; Cancel or Escape backs out and returns
  focus to the trigger. A failure shows its message right here, by the row.
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
	}
	let { label, ask, confirm = 'Yes', run }: Props = $props();

	let asking = $state(false);
	let busy = $state(false);
	let error = $state<string | null>(null);
	let yes = $state<HTMLButtonElement | null>(null);
	let trigger = $state<HTMLButtonElement | null>(null);

	async function start() {
		error = null;
		asking = true;
		await tick();
		yes?.focus();
	}

	async function back() {
		asking = false;
		await tick();
		trigger?.focus();
	}

	async function go() {
		busy = true;
		error = null;
		try {
			await run();
			asking = false;
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

<span class="wrap">
	{#if asking}
		<span class="ask" role="group" aria-label={ask}>
			<span class="hint">{ask}</span>
			<button bind:this={yes} class="btn" type="button" onclick={go} disabled={busy} {onkeydown}>
				{busy ? 'Working…' : confirm}
			</button>
			<button class="link cancel" type="button" onclick={back} disabled={busy} {onkeydown}>
				Cancel
			</button>
		</span>
	{:else}
		<button bind:this={trigger} class="btn" type="button" onclick={start}>{label}</button>
	{/if}
	{#if error}<span class="hint err" role="alert">{error}</span>{/if}
</span>

<style>
	.wrap {
		display: inline-grid;
		justify-items: end;
		gap: 6px;
	}
	.ask {
		display: inline-flex;
		align-items: center;
		gap: 12px;
		flex-wrap: wrap;
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

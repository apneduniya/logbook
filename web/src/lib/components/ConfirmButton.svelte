<!--
  A destructive action in two steps: the first press asks, the second does it.
  Focus moves to the confirm button, and Escape or "Cancel" backs out.
-->
<script lang="ts">
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
	let yes = $state<HTMLButtonElement | null>(null);

	$effect(() => {
		if (asking) yes?.focus();
	});

	async function go() {
		busy = true;
		try {
			await run();
		} finally {
			busy = false;
			asking = false;
		}
	}
</script>

{#if asking}
	<span class="ask" role="group" aria-label={ask}>
		<span class="lbl">{ask}</span>
		<button
			bind:this={yes}
			class="btn"
			type="button"
			onclick={go}
			disabled={busy}
			onkeydown={(e) => e.key === 'Escape' && (asking = false)}
		>
			{busy ? 'Working…' : confirm}
		</button>
		<button class="link text-sm" type="button" onclick={() => (asking = false)} disabled={busy}>
			Cancel
		</button>
	</span>
{:else}
	<button class="link text-sm" type="button" onclick={() => (asking = true)}>{label}</button>
{/if}

<style>
	.ask {
		display: inline-flex;
		align-items: center;
		gap: 12px;
		flex-wrap: wrap;
	}
	button.link {
		background: none;
		border: 0;
		padding: 0;
		cursor: pointer;
		font: inherit;
	}
</style>

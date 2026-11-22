<!--
  A segmented control: one choice of a few, the active one an inverted block
  like the active nav tab. Doop "RangeSwitch".
-->
<script lang="ts" generics="T extends string">
	interface Props {
		options: { value: T; label: string }[];
		value: T;
		label: string;
		onchange: (value: T) => void;
	}
	let { options, value, label, onchange }: Props = $props();
</script>

<div class="seg" role="group" aria-label={label}>
	{#each options as o (o.value)}
		<button type="button" aria-pressed={o.value === value} onclick={() => onchange(o.value)}>
			{o.label}
		</button>
	{/each}
</div>

<style>
	.seg {
		display: inline-flex;
		border: 1px solid var(--color-rule);
		flex: none;
	}
	button {
		font-family: var(--font-pixel);
		font-size: var(--text-xs);
		letter-spacing: 0.06em;
		text-transform: uppercase;
		color: var(--color-ink-2);
		background: none;
		border: 0;
		padding: 5px 10px;
		cursor: pointer;
		transition:
			background 150ms ease-out,
			color 150ms ease-out;
	}
	button:hover:not([aria-pressed='true']) {
		color: var(--color-ink);
		background: var(--color-fill);
	}
	button:focus-visible {
		outline: 1px solid var(--color-ink);
		outline-offset: -1px;
	}
	button[aria-pressed='true'] {
		background: var(--color-ink);
		color: var(--color-ground);
	}
	/* Ink focus ring on an ink block would vanish; draw it in the ground colour inside. */
	button[aria-pressed='true']:focus-visible {
		outline-color: var(--color-ground);
		outline-offset: -3px;
	}
	@media (max-width: 720px) {
		button {
			padding-top: 10px;
			padding-bottom: 10px;
		}
	}
</style>

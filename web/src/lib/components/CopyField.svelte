<!--
  A value with a Copy button, as on InstallCommand. What is shown and what is
  copied can differ:
  - `masked` hides a one-time secret until "Show" (the install command, an
    app's new client secret). Copy always copies the full value.
  - `display` replaces the value on screen for good, with no Show: a review
    link or a justification that holds one is never printed in full.
  - `wrap` sets long prose (a justification) as a wrapping sans paragraph
    instead of one scrolling mono line.
  - `bare` drops the value box and leaves only the button ("Copy link").

  Props: value, label (the accessible name, e.g. "Client id"), masked?,
  display?, wrap?, bare?, button? (the button's word, default "Copy").
-->
<script lang="ts">
	interface Props {
		value: string;
		label: string;
		masked?: string;
		display?: string;
		wrap?: boolean;
		bare?: boolean;
		button?: string;
	}
	let {
		value,
		label,
		masked,
		display,
		wrap = false,
		bare = false,
		button = 'Copy'
	}: Props = $props();

	let shown = $state(false);
	// A new value (a secret rotated again) is hidden again until "Show".
	let shownFor = $state(value);
	$effect.pre(() => {
		if (value !== shownFor) {
			shownFor = value;
			shown = false;
		}
	});
	let copied = $state(false);
	let error = $state<string | null>(null);
	let timer: ReturnType<typeof setTimeout> | null = null;

	const visible = $derived(masked !== undefined && !shown ? masked : (display ?? value));

	async function copy() {
		error = null;
		try {
			await navigator.clipboard.writeText(value);
			copied = true;
			if (timer) clearTimeout(timer);
			timer = setTimeout(() => (copied = false), 1600);
		} catch {
			error =
				masked !== undefined
					? 'Couldn’t copy. Press Show and copy it by hand.'
					: display !== undefined
						? 'Couldn’t copy. Your browser blocked the clipboard; try again.'
						: 'Couldn’t copy. Select it and copy it by hand.';
		}
	}

	$effect(() => () => {
		if (timer) clearTimeout(timer);
	});
</script>

{#if bare}
	<span class="bare">
		<!-- Both words share one cell, so "Copied" never narrows the button and
		     moves what comes after it. -->
		<button class="btn stack" type="button" onclick={copy} aria-label="{button}: {label}">
			<span class:off={copied}>{button}</span>
			<span class:off={!copied} aria-hidden="true">Copied</span>
		</button>
		{#if error}<span class="hint err" role="alert">{error}</span>{/if}
	</span>
{:else}
	<div class="cf" class:wrap role="group" aria-label={label}>
		{#if wrap}<p class="val">{visible}</p>{:else}<code class="val">{visible}</code>{/if}
		<div class="acts">
			{#if masked !== undefined}
				<button class="ghost" type="button" onclick={() => (shown = !shown)} aria-pressed={shown}>
					{shown ? 'Hide' : 'Show'}
				</button>
			{/if}
			<button class="copy" type="button" onclick={copy}>{copied ? 'Copied' : button}</button>
		</div>
	</div>
	{#if error}<p class="hint err mt-2" role="alert">{error}</p>{/if}
{/if}
<span class="sr-only" aria-live="polite">{copied ? `${label} copied` : ''}</span>

<style>
	.cf {
		display: flex;
		align-items: center;
		justify-content: space-between;
		gap: 12px;
		padding: 8px 8px 8px 14px;
		background: var(--color-fill);
		border: 1px solid var(--color-rule);
		min-width: 0;
	}
	code.val {
		font-family: var(--font-mono);
		font-size: var(--text-sm);
		overflow-x: auto;
		white-space: nowrap;
		min-width: 0;
		scrollbar-width: thin;
		padding: 2px 0;
	}
	/* Prose: the button sits under the paragraph, on its left edge. */
	.cf.wrap {
		flex-direction: column;
		align-items: flex-start;
		padding: 12px 14px;
	}
	p.val {
		font-size: var(--text-sm);
		line-height: 1.55;
		overflow-wrap: anywhere;
		max-width: 62ch;
	}
	.acts {
		display: flex;
		flex: none;
		gap: 4px;
	}
	.copy,
	.ghost {
		font-family: var(--font-pixel);
		font-size: var(--text-xs);
		letter-spacing: 0.06em;
		text-transform: uppercase;
		border: 0;
		padding: 6px 10px;
		cursor: pointer;
		transition: background 150ms ease-out;
	}
	.copy {
		color: var(--color-ground);
		background: var(--color-ink);
	}
	.copy:hover {
		background: var(--color-ink-2);
	}
	.ghost {
		color: var(--color-ink-2);
		background: none;
	}
	.ghost:hover {
		color: var(--color-ink);
	}
	.bare {
		display: inline-grid;
		gap: 6px;
	}
	.stack {
		display: inline-grid;
	}
	.stack > span {
		grid-area: 1 / 1;
	}
	.stack > .off {
		visibility: hidden;
	}
	.err {
		color: var(--color-ink);
	}
</style>

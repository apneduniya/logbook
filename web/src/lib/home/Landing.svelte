<!--
  Signed-out home. One promise, one action, one picture of what the promise
  means: an example hour as a single strip, with the two cuts labelled on
  the minutes they took. The hour is illustrative and labelled "example".
-->
<script lang="ts">
	import { page } from '$app/state';
	import { app } from '#lib/app.svelte.ts';
	import { REPO_URL } from '#lib/links.ts';

	type Kind = 'you' | 'agent' | 'idle';
	interface Cut {
		kind: Exclude<Kind, 'you'>;
		from: number;
		to: number;
		why: string;
	}

	const cuts: Cut[] = [
		{ kind: 'agent', from: 21, to: 26, why: 'Agent ran alone after you left' },
		{ kind: 'idle', from: 47, to: 50, why: 'Idle' }
	];
	const minutes: Kind[] = Array.from(
		{ length: 60 },
		(_, i) => cuts.find((c) => i >= c.from && i < c.to)?.kind ?? 'you'
	);
	const counted = minutes.filter((k) => k === 'you').length;
	const at = (m: number) => `14:${String(m).padStart(2, '0')}`;
	const describe =
		`An example hour from 14:00: ${counted} minutes counted. ` +
		cuts
			.map((c) => `${at(c.from)}, ${c.to - c.from} minutes cut: ${c.why.toLowerCase()}.`)
			.join(' ');

	const errors: Record<string, string> = {
		invalid_state: 'That sign-in took too long or was interrupted. Please try again.',
		access_denied: 'Sign-in was cancelled.',
		failed: 'Hack Club sign-in didn’t work this time. Please try again.'
	};
	const authError = $derived(errors[page.url.searchParams.get('auth_error') ?? ''] ?? null);

	const points = [
		[
			'Nothing to start or stop',
			'Install once. It works with your WakaTime plugins, which keep sending to Hackatime, and with Claude Code, Codex, Cursor and Copilot.'
		],
		[
			'Every cut says why',
			'Idle time, agents left running alone and AI changes accepted without reading are taken off, and each cut shows its reason.'
		],
		[
			'Only timings leave your laptop',
			'Prompts, code and keystrokes stay on your computer. The server gets counted minutes and a signed daily hash.'
		]
	];
</script>

<svelte:head><title>Logbook: count the hours you actually worked</title></svelte:head>

<section class="hero">
	<h1 class="px"><span>Count the hours</span> <span>you actually worked.</span></h1>
	<p class="lede">
		Logbook runs in the background while you build, with or without AI, and shows a reviewer why
		each minute counted.
	</p>
	{#if authError}
		<p class="err" role="alert">{authError}</p>
	{/if}
	<button class="cta" type="button" onclick={() => app.signIn()}>Sign in with Hack Club</button>
</section>

<figure class="fig">
	<figcaption class="lbl cap"><span>Fig. 1 · one hour</span><span>example</span></figcaption>
	<p class="reading">
		<span class="px big">{counted}m</span>
		<span class="mute">counted out of 60 minutes at the keyboard</span>
	</p>
	<div class="strip" role="img" aria-label={describe}>
		{#each minutes as k, i (i)}<i class={k}></i>{/each}
	</div>
	<div class="notes" aria-hidden="true">
		<span class="lbl start">14:00</span>
		{#each cuts as c, i (c.from)}
			<!-- Each label runs from its cut to the next one (or the hour's end). -->
			<span class="note" style:grid-column="{c.from + 1} / {(cuts[i + 1]?.from ?? 60) + 1}">
				<span class="mono">
					<span class="when">{at(c.from)}&nbsp;·&nbsp;</span>−{c.to - c.from}m
				</span>
				<span class="why">{c.why}</span>
			</span>
		{/each}
	</div>
</figure>

<ol class="points">
	{#each points as [title, body], i (title)}
		<li class="row">
			<span class="px mute">{String(i + 1).padStart(2, '0')}</span>
			<span class="text-base">{title}</span>
			<span class="mute">{body}</span>
		</li>
	{/each}
</ol>

<footer class="lbl foot">
	<span>Logbook · free and open source (AGPLv3)</span>
	<a class="link" href={REPO_URL} rel="external">Source on GitHub →</a>
</footer>

<style>
	.hero {
		display: grid;
		justify-items: start;
		gap: 28px;
		padding-top: 64px;
	}
	h1 {
		font-weight: 400;
		font-size: clamp(40px, 6vw, var(--text-3xl));
		line-height: 1;
		letter-spacing: -0.02em;
		max-width: 20ch;
		text-wrap: balance;
	}
	.lede {
		font-size: var(--text-lg);
		line-height: 1.4;
		color: var(--color-ink-2);
		max-width: 46ch;
	}
	.cta {
		border: 0;
		cursor: pointer;
		font-family: inherit;
		margin-top: 4px;
	}
	.err {
		color: var(--color-ink);
		border-left: 2px solid var(--color-ink);
		padding-left: 12px;
	}

	/* Fig. 1: the hour as one strip, with the cuts labelled where they happen. */
	.fig {
		margin: 72px 0 0;
		border-top: 1px solid var(--color-rule);
		padding-top: 14px;
	}
	.cap {
		display: flex;
		justify-content: space-between;
	}
	.reading {
		display: flex;
		align-items: baseline;
		gap: 14px;
		margin: 28px 0 20px;
	}
	.big {
		font-size: var(--text-2xl);
		line-height: 0.9;
	}
	.strip,
	.notes {
		display: grid;
		grid-template-columns: repeat(60, minmax(0, 1fr));
		column-gap: 3px;
	}
	.strip i {
		height: 44px;
	}
	.strip .you {
		background: var(--color-ink);
	}
	.strip .agent {
		background: var(--hatch);
	}
	.strip .idle {
		background: var(--color-fill);
		outline: 1px solid var(--color-ink-3);
		outline-offset: -1px;
	}
	.notes {
		grid-template-rows: auto;
	}
	.notes > * {
		grid-row: 1;
	}
	.start {
		grid-column: 1 / span 10;
		padding-top: 14px;
	}
	/* A tick from the strip down to the label marks where the cut starts. */
	.note {
		display: grid;
		align-content: start;
		gap: 2px;
		padding: 12px 8px 0;
		border-left: 1px solid var(--color-ink);
		font-size: var(--text-sm);
		min-width: 0;
	}
	.why {
		color: var(--color-ink-2);
		text-wrap: balance;
	}
	/* On wide screens each line of the headline stays whole, so it breaks
	   after "hours". Phones let it wrap naturally. */
	@media (min-width: 901px) {
		h1 > span {
			display: inline-block;
		}
	}

	.points {
		margin-top: 72px;
		list-style: none;
		padding: 0;
	}
	.row {
		display: grid;
		grid-template-columns: 40px calc((100% - 12px) * 5 / 12 - 68px) minmax(0, 1fr);
		gap: 20px;
		padding: 18px 0;
		border-top: 1px solid var(--color-rule);
		align-items: baseline;
	}
	.row > :last-child {
		max-width: 62ch;
	}
	.row:last-child {
		border-bottom: 1px solid var(--color-rule);
	}
	.foot {
		display: flex;
		justify-content: space-between;
		gap: 16px;
		padding: 16px 0 18px;
		margin-top: auto;
	}
	@media (max-width: 900px) {
		.hero {
			padding-top: 40px;
			gap: 22px;
		}
		.lede {
			font-size: var(--text-base);
		}
		.fig,
		.points {
			margin-top: 48px;
		}
		.strip,
		.notes {
			column-gap: 1px;
		}
		.strip i {
			height: 32px;
		}
		.big {
			font-size: var(--text-xl);
		}
		.note {
			font-size: var(--text-xs);
			padding-left: 6px;
		}
		/* Too narrow for the clock time; the strip shows where each cut is. */
		.when {
			display: none;
		}
		.row {
			grid-template-columns: 32px 1fr;
			gap: 6px 12px;
		}
		.row > :last-child {
			grid-column: 2;
		}
		.foot {
			flex-direction: column;
			gap: 4px;
		}
	}
</style>

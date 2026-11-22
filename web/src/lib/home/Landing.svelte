<!--
  Signed-out home (B3 "Signed-out home"). The example hour is illustrative
  and labelled "example"; nothing here claims real numbers.
-->
<script lang="ts">
	import { page } from '$app/state';
	import { app } from '#lib/app.svelte.ts';
	import { RULES_URL } from '#lib/links.ts';
	import Panel from '#lib/components/Panel.svelte';

	type Kind = 'you' | 'agent' | 'idle';
	const minutes: Kind[] = Array.from({ length: 60 }, (_, i) =>
		i >= 21 && i < 26 ? 'agent' : i >= 47 && i < 50 ? 'idle' : 'you'
	);
	const what: Record<Kind, string> = {
		you: 'counted',
		agent: 'agent alone, not counted',
		idle: 'idle, not counted'
	};

	const errors: Record<string, string> = {
		invalid_state: 'That sign-in took too long or was interrupted. Please try again.',
		access_denied: 'Sign-in was cancelled.',
		failed: 'Hack Club sign-in didn’t work this time. Please try again.'
	};
	const authError = $derived(errors[page.url.searchParams.get('auth_error') ?? ''] ?? null);

	const points = [
		[
			'Nothing to start or stop',
			'Install once. The agent starts with your computer and works with every WakaTime plugin, plus Claude Code, Codex, Cursor and Copilot.'
		],
		[
			'Every cut says why',
			'Idle time, an agent running on its own, rubber-stamped changes and loops are taken off, with the reason shown. The rules are public.'
		],
		[
			'Only timings leave your laptop',
			'Prompts, code and keystrokes stay on your computer. The server gets counted minutes and a daily fingerprint, unless you choose to submit evidence.'
		]
	];
</script>

<svelte:head><title>Logbook: count the hours you actually worked</title></svelte:head>

<div class="hero">
	<div class="pitch">
		<h1 class="px">Count the hours you actually worked.</h1>
		<p class="mute lede">
			Logbook runs in the background while you build, with or without AI. It counts the time you
			were really working, takes off the rest, and tells a reviewer why each minute counted.
		</p>
		{#if authError}
			<p class="err" role="alert">{authError}</p>
		{/if}
		<div class="actions">
			<button class="cta" type="button" onclick={() => app.signIn()}>Sign in with Hack Club</button>
			<a class="link" href={RULES_URL} rel="external">How counting works →</a>
		</div>
		<p class="lbl">
			Free and open source (AGPLv3). Your editor plugins keep working with Hackatime.
		</p>
	</div>

	<Panel title="One hour, minute by minute" meta="example">
		<div class="grid gap-4">
			<div class="flex items-baseline gap-3">
				<span class="px text-xl">52m</span><span class="mute">counted of 60m at the keyboard</span>
			</div>
			<div
				class="hour"
				role="img"
				aria-label="An example hour: 52 minutes you, 5 minutes agent alone, 3 minutes idle"
			>
				{#each minutes as k, i (i)}
					<i class="m {k}" title="14:{String(i).padStart(2, '0')} · {what[k]}"></i>
				{/each}
			</div>
			<div class="lbl flex justify-between">
				<span>14:00</span><span>14:30</span><span>14:59</span>
			</div>
			<div class="cuts">
				<span class="mono">14:21</span><span>
					Agent working alone: it kept running after you left the desk
				</span><span class="mono mute">−5m</span>
				<span class="mono">14:47</span><span>Idle: no input, no agent running</span><span
					class="mono mute"
				>
					−3m
				</span>
				<span class="mono">14:08</span><span>Prompting, reading the diff, fixing a test</span><span
					class="mono"
				>
					counted
				</span>
			</div>
			<div class="lbl flex flex-wrap gap-5">
				<span><i class="sw bg-ink"></i>you</span>
				<span><i class="sw hatch"></i>agent alone</span>
				<span><i class="sw idle-sw"></i>idle</span>
			</div>
		</div>
	</Panel>
</div>

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
	<span>Logbook · AGPLv3</span><span>
		For Hack Clubbers and the programs that review their hours
	</span>
</footer>

<style>
	.hero {
		display: grid;
		grid-template-columns: calc((100% - 12px) * 5 / 12) 1fr;
		gap: 12px;
		padding-top: 24px;
	}
	.pitch {
		display: grid;
		align-content: start;
		gap: 24px;
		padding-right: 24px;
	}
	h1 {
		font-weight: 400;
		font-size: var(--text-2xl);
		line-height: 1.02;
		letter-spacing: -0.02em;
		text-wrap: balance;
	}
	.lede {
		font-size: var(--text-base);
		max-width: 44ch;
	}
	.actions {
		display: flex;
		flex-wrap: wrap;
		gap: 20px;
		align-items: center;
	}
	.actions .cta {
		border: 0;
		cursor: pointer;
		font-family: inherit;
	}
	.actions .link {
		font-size: var(--text-base);
	}
	.err {
		color: var(--color-ink);
		border-left: 2px solid var(--color-ink);
		padding-left: 12px;
	}
	.hour {
		display: grid;
		grid-template-columns: repeat(12, 1fr);
		gap: 4px;
	}
	.m {
		height: 22px;
	}
	.m.you {
		background: var(--color-ink);
	}
	.m.agent {
		background: var(--hatch);
	}
	.m.idle {
		background: var(--color-fill);
	}
	.idle-sw {
		background: var(--color-fill);
		outline: 1px solid var(--color-rule);
	}
	.cuts {
		display: grid;
		grid-template-columns: auto 1fr auto;
		gap: 10px 16px;
		padding-top: 14px;
		border-top: 1px solid var(--color-rule);
		align-items: baseline;
	}
	.points {
		margin-top: 28px;
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
			grid-template-columns: 1fr;
			gap: 32px;
		}
		.pitch {
			padding-right: 0;
		}
		h1 {
			font-size: 40px;
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

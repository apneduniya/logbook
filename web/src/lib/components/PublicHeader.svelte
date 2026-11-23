<!-- The header for signed-out visitors: wordmark, reading links, sign in. -->
<script lang="ts">
	import { resolve } from '$app/paths';
	import { app } from '#lib/app.svelte.ts';
	import { RULES_URL, REPO_URL } from '#lib/links.ts';
</script>

<header class="hdr">
	<a class="mark px" href={resolve('/')}>LOGBOOK</a>
	<nav aria-label="Site">
		<a class="link hide-sm" href={RULES_URL} rel="external">How counting works</a>
		<a class="link hide-sm" href={REPO_URL} rel="external">GitHub</a>
		<button class="signin" type="button" onclick={() => app.signIn()}>Sign in</button>
	</nav>
</header>

<style>
	.hdr {
		display: flex;
		justify-content: space-between;
		align-items: center;
		padding: 6px 0 14px;
		border-bottom: 1px solid var(--color-rule);
	}
	.mark {
		font-size: var(--text-sm);
		letter-spacing: 0.12em;
		color: var(--color-ink);
		text-decoration: none;
	}
	nav {
		display: flex;
		gap: 24px;
		align-items: center;
	}
	.signin {
		font: inherit;
		font-size: var(--text-sm);
		color: var(--color-ink);
		background: none;
		border: 1px solid var(--color-ink-3);
		padding: 6px 12px;
		cursor: pointer;
		transition:
			background 150ms ease-out,
			color 150ms ease-out;
	}
	.signin:hover {
		background: var(--color-ink);
		color: var(--color-ground);
	}
	@media (max-width: 520px) {
		.hide-sm {
			display: none;
		}
	}
</style>

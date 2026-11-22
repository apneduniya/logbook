<script lang="ts">
	import { resolve } from '$app/paths';
	import { page } from '$app/state';
	import Panel from '#lib/components/Panel.svelte';

	const missing = $derived(page.status === 404);
	const title = $derived(
		missing ? 'Not found' : page.status === 429 ? 'Slow down' : 'Something went wrong'
	);
	// Loads pass the API's user-safe message through (#lib/load.ts). SvelteKit's
	// own messages ("Not Found", "Internal Error") get a plainer sentence.
	const generic = ['Not Found', 'Internal Error', ''];
	const text = $derived(
		page.error?.message && !generic.includes(page.error.message)
			? page.error.message
			: missing
				? 'There’s nothing at this address.'
				: 'Logbook couldn’t load this page. Check your connection and try again.'
	);
</script>

<svelte:head><title>{title} · Logbook</title></svelte:head>

<div class="max-w-[640px] pt-3">
	<Panel {title} meta={String(page.status)}>
		<h1 class="text-lg font-normal">{text}</h1>
		<div class="mt-5 flex items-center gap-5">
			<a class="btn" href={resolve('/')}>Go to today</a>
			{#if !missing}
				<button class="link cursor-pointer" type="button" onclick={() => location.reload()}>
					Try again
				</button>
			{/if}
		</div>
	</Panel>
</div>

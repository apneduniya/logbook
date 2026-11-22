<!-- Languages and editors over the chosen range, from time at the keyboard. -->
<script lang="ts">
	import { duration, editorName, percent } from '#lib/format.ts';
	import type { Named } from '#lib/types.ts';
	import Panel from '#lib/components/Panel.svelte';

	let { languages, editors, meta }: { languages: Named[]; editors: Named[]; meta: string } =
		$props();

	const tones = ['bg-ink', 'bg-ink-2', 'bg-ink-3'];

	// Rows carry their own key: a language may itself be called "Other" or
	// "Unknown", which must not collide with the remainder row.
	const rows = $derived.by(() => {
		const total = languages.reduce((s, l) => s + l.seconds, 0);
		const out = languages.slice(0, 3).map((l, i) => ({
			key: `lang:${i}`,
			name: l.name || 'Unknown',
			seconds: l.seconds,
			tone: tones[i]
		}));
		const rest = languages.slice(3).reduce((s, l) => s + l.seconds, 0);
		if (rest > 0) out.push({ key: 'rest', name: 'Other', seconds: rest, tone: 'other' });
		return out.map((r) => ({ ...r, pc: percent(r.seconds, total) }));
	});
</script>

<Panel title="Languages" {meta}>
	{#if rows.length === 0}
		<p class="mute">No editor time in this range.</p>
	{:else}
		<div class="grid content-start gap-4">
			<div class="split" role="img" aria-label={rows.map((r) => `${r.name} ${r.pc}%`).join(', ')}>
				{#each rows as r (r.key)}
					<span class={r.tone} style:flex={r.seconds}></span>
				{/each}
			</div>
			<div class="table">
				{#each rows as r (r.key)}
					<span class="truncate"><i class="sw {r.tone}"></i>{r.name}</span>
					<span class="mono">{duration(r.seconds)}</span>
					<span class="mono lbl text-right">{r.pc}%</span>
				{/each}
			</div>
			{#if editors.length}
				<div class="editors">
					<span class="lbl col-span-full">Editors</span>
					{#each editors.slice(0, 4) as e, i (i)}
						<span class="truncate">{editorName(e.name)}</span><span class="mono">
							{duration(e.seconds)}
						</span>
					{/each}
				</div>
			{/if}
		</div>
	{/if}
</Panel>

<style>
	.split {
		display: flex;
		height: 14px;
		gap: 2px;
	}
	.other {
		background: var(--color-fill);
		outline: 1px solid var(--color-rule);
		outline-offset: -1px;
	}
	.table {
		display: grid;
		grid-template-columns: minmax(0, 1fr) auto 40px;
		gap: 10px 12px;
		align-items: baseline;
	}
	.editors {
		display: grid;
		grid-template-columns: minmax(0, 1fr) auto;
		gap: 10px 12px;
		padding-top: 16px;
		border-top: 1px solid var(--color-rule);
	}
</style>

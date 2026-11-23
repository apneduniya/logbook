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
	// Plugins name one editor several ways ("code", "vscode"): merge by the
	// name people know, then rank.
	const editorRows = $derived.by(() => {
		const byName: Record<string, number> = {};
		for (const e of editors) {
			const n = editorName(e.name);
			byName[n] = (byName[n] ?? 0) + e.seconds;
		}
		return Object.entries(byName)
			.map(([name, seconds]) => ({ name, seconds }))
			.sort((a, b) => b.seconds - a.seconds || a.name.localeCompare(b.name))
			.slice(0, 4);
	});

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
			{#if editorRows.length}
				<div class="editors">
					<span class="lbl col-span-full">Editors</span>
					{#each editorRows as e (e.name)}
						<span class="truncate">{e.name}</span><span class="mono">
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

<!-- The cuts and flags of rules v1 (docs/STRUGGLE-RULES.md), in plain words. -->
<script lang="ts">
	const cuts = [
		['Idle', 'no input and no agent running'],
		['Agent working alone', 'it ran while you were away'],
		['Rubber-stamping', 'saying “yes”, “continue” again and again without reading'],
		['Loops', 'the same long prompt sent over and over, with no editing in between'],
		['Background editor', 'editor open, nothing happening (only when the OS can’t report input)']
	];
</script>

<p class="mute">
	Logbook starts from your time at the keyboard and takes off what clearly isn’t work. Every cut
	says why.
</p>
<ul class="mt-3.5">
	{#each cuts as [name, why] (name)}
		<li><span>{name}</span><span class="lbl">{why}</span></li>
	{/each}
</ul>
<p class="mute mt-3.5">
	Scripted input and agents running on a schedule are flagged for the reviewer, never cut. Beginners
	are never penalised, and AI work counts the same as typing.
</p>

<style>
	ul {
		list-style: none;
		padding: 0;
		border-bottom: 1px solid var(--color-rule);
	}
	li {
		display: grid;
		padding: 10px 0;
		border-top: 1px solid var(--color-rule);
	}
</style>

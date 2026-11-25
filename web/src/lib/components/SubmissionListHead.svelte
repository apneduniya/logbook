<!--
  Column labels above a list of SubmissionRows; the same grid as the row.
  Pass the same `submitter` and `actions` as the rows. Phones keep only
  "Submission" and "Counted", as the rows fold the rest under the name.

  Props: submitter?: boolean, actions?: boolean.
-->
<script lang="ts">
	let { submitter = false, actions = false }: { submitter?: boolean; actions?: boolean } = $props();
</script>

<div class="lbl cols" class:who-col={submitter} class:acts-col={actions} aria-hidden="true">
	<span></span><span>Submission</span>{#if submitter}<span class="who">Submitter</span>{/if}<span
		class="r"
	>
		Counted
	</span><span class="dec">Decision</span>{#if actions}<span class="act"></span>{/if}
</div>

<style>
	.cols {
		--cols: 14px minmax(0, 1fr) 176px 150px;
		display: grid;
		grid-template-columns: var(--cols);
		gap: 16px;
		padding: 10px 14px;
		border-bottom: 1px solid var(--color-rule);
	}
	.cols.who-col {
		--cols: 14px minmax(0, 1fr) 160px 176px 150px;
	}
	.cols.acts-col {
		--cols: 14px minmax(0, 1fr) 176px 150px 200px;
	}
	.cols.who-col.acts-col {
		--cols: 14px minmax(0, 1fr) 160px 176px 150px 200px;
	}
	.r {
		text-align: right;
	}
	@media (max-width: 640px) {
		.cols,
		.cols.who-col,
		.cols.acts-col,
		.cols.who-col.acts-col {
			grid-template-columns: 14px minmax(0, 1fr) auto;
			gap: 12px;
		}
		.who,
		.dec,
		.act {
			display: none;
		}
	}
</style>

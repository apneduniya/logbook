<!-- Column labels above a list of ProjectRows; same grid as the row. -->
<div class="lbl cols" aria-hidden="true">
	<span>Project</span><span class="kb">Keyboard</span><span class="r">Counted</span><span></span>
</div>

<style>
	.cols {
		display: grid;
		grid-template-columns: minmax(0, 1fr) 72px 72px 16px;
		gap: 16px;
		padding: 10px 14px;
		border-bottom: 1px solid var(--color-rule);
	}
	.kb,
	.r {
		text-align: right;
	}
	@media (max-width: 480px) {
		.cols {
			grid-template-columns: minmax(0, 1fr) 64px 16px;
			gap: 12px;
		}
		.kb {
			display: none;
		}
	}
</style>

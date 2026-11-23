import { app, ME } from '#lib/app.svelte.ts';
import { addDays } from '#lib/format.ts';
import type { Range } from '#lib/types.ts';
import { loadFailed } from '#lib/load.ts';
import { homeKind } from '#lib/home/kind.ts';
import type { PageLoad } from './$types';

const RANGE_DAYS: Record<Range, number> = { '7d': 7, '30d': 30, year: 365 };

function parseRange(v: string | null): Range {
	return v === '30d' || v === 'year' ? v : '7d';
}

export const load: PageLoad = async ({ url, depends, parent }) => {
	depends(ME);
	// The layout's load checks the session; loads run in parallel unless they wait.
	await parent();
	if (!app.session) return { kind: 'signed-out' as const };
	try {
		return await dashboard(url);
	} catch (err) {
		loadFailed(err);
	}
};

async function dashboard(url: URL) {
	const api = app.api;
	const range = parseRange(url.searchParams.get('range'));
	const today = await api.today();
	const end = today.end;
	// The year (365 days) feeds the year grid, the 14-day streak strip and the
	// previous period's totals; the range summary has the per-range breakdowns.
	// It also tells a user with no device yet whether they have history to see.
	const yearStart = addDays(end, -364);
	const rangeStart = addDays(end, -(RANGE_DAYS[range] - 1));
	const setUp = today.credited_seconds === null;
	const [year, setup] = await Promise.all([
		api.summary({ start: yearStart, end }),
		setUp ? Promise.all([api.hackatime(), api.devices()]) : null
	]);

	const kind = homeKind(today, year);
	if (kind === 'first-run' && setup) {
		const [hackatime, devices] = setup;
		return { kind, today, hackatime, devices };
	}

	const [projects, rangeSummary] = await Promise.all([
		api.projects(),
		range === 'year' ? null : api.summary({ start: rangeStart, end })
	]);
	const summary = rangeSummary ?? year;

	for (const p of projects) app.sawActivity(p.last_activity);
	// Live refreshes reload this; keep the agent status in the header current too.
	void app.loadDevices();

	if (kind === 'history' && setup) {
		const [hackatime] = setup;
		return { kind, range, year, summary, projects, hackatime };
	}
	return { kind: 'dashboard' as const, range, today, year, summary, projects };
}

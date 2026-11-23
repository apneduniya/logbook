import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { poll } from './poll.ts';

describe('poll', () => {
	beforeEach(() => vi.useFakeTimers());
	afterEach(() => vi.useRealTimers());

	it('stops once a check resolves true, and retries failures', async () => {
		const results = [
			Promise.reject(new Error('offline')),
			Promise.resolve(false),
			Promise.resolve(true)
		];
		const check = vi.fn(() => results.shift() ?? Promise.resolve(false));
		poll(1000, check);
		for (let i = 0; i < 5; i++) await vi.advanceTimersByTimeAsync(1000);
		expect(check).toHaveBeenCalledTimes(3);
	});

	it('never overlaps checks', async () => {
		let finish: (v: boolean) => void = () => {};
		const check = vi.fn(() => new Promise<boolean>((r) => (finish = r)));
		poll(1000, check);
		await vi.advanceTimersByTimeAsync(3000);
		expect(check).toHaveBeenCalledTimes(1);
		finish(false);
		await vi.advanceTimersByTimeAsync(1000);
		expect(check).toHaveBeenCalledTimes(2);
	});

	it('tells a check still in flight that it was stopped', async () => {
		let finish: () => void = () => {};
		let aliveAfter: boolean | null = null;
		const stop = poll(1000, async (alive) => {
			await new Promise<void>((r) => (finish = r));
			aliveAfter = alive();
			return false;
		});
		await vi.advanceTimersByTimeAsync(1000);
		stop();
		finish();
		await vi.advanceTimersByTimeAsync(0);
		expect(aliveAfter).toBe(false);
	});
});

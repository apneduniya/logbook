package osprobe

import "time"

// idleFromTicks computes Windows idle time from GetTickCount64 (now, ms since
// boot) and LASTINPUTINFO.dwTime (last, a 32-bit GetTickCount value that
// wraps every ~49.7 days).
//
// Subtracting in uint32 arithmetic is correct across a wrap as long as the
// real idle time is under 2^32 ms, which holds for any interactive session
// that has been idle less than 49.7 days.
func idleFromTicks(now uint64, last uint32) time.Duration {
	elapsed := uint32(now) - last //nolint:gosec // truncation to the low 32 bits is the point: dwTime is the low 32 bits of the tick count
	return time.Duration(elapsed) * time.Millisecond
}

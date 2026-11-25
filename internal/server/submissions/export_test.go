package submissions

import "context"

// SetDecodeHook tells hook +1 when c's re-check holds a decoded part and
// -1 when it has dropped it.
func SetDecodeHook(c *Checker, hook func(delta int)) { c.decoded = hook }

// AcquireSlot takes one of d's decode slots, waiting until ctx ends.
func AcquireSlot(ctx context.Context, d *DecodeSlots) (func(), error) { return d.acquire(ctx) }

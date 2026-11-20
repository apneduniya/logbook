package crediting

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/apneduniya/logbook/internal/agent/devicekey"
	"github.com/apneduniya/logbook/internal/agent/localdb"
	"github.com/apneduniya/logbook/internal/agent/serverapi"
)

// maxLoggedRejections bounds per-item rejection log lines per batch.
const maxLoggedRejections = 5

// uploadMinutes sends every stored minute whose current content the server
// doesn't have, in batches of whole minutes (all projects of a minute go in
// one request, so the server can apply decreases before increases under its
// 60-second cap). Per-item rejections are logged, counted and never retried
// (unless the minute changes). A batch refused for its size is split, down
// to single minutes, which are then rejected; a batch refused as a whole
// for any other reason returns a *refusedError and nothing is marked sent.
//
// drained is true when nothing is left to send, and false when the run's
// budget (maxBatchesPerRun) ran out first; the rest goes on the next run.
func (r *Runner) uploadMinutes(ctx context.Context, t serverapi.Target, dev devicekey.Device, now time.Time, st settleState) (drained bool, err error) {
	final := finalBefore(now)
	held, err := r.held(ctx, now, st)
	if err != nil {
		return false, err
	}
	for range maxBatchesPerRun {
		size := r.batchSize()
		rows, err := r.opt.Store.UnsentMinutes(ctx, final, held, size)
		if err != nil {
			return false, err
		}
		if len(rows) == 0 {
			return true, nil
		}
		rows = sameRules(rows)
		res, err := r.opt.Server.UploadMinutes(ctx, t, dev.ID, minutesRequest(rows))
		switch {
		case serverapi.IsTooLarge(err) && size > 1:
			r.shrink(len(rows))
			r.opt.Logger.WarnContext(ctx, "server refused a minutes batch as too large; splitting it", "size", len(rows), "err", err)
			continue
		case serverapi.IsTooLarge(err):
			// Even a single minute is too large: it can never be sent.
			res, err = rejectAll(len(rows), err), nil
		case serverapi.KindOf(err) == serverapi.KindInvalid:
			return false, &refusedError{err: err}
		}
		if err != nil {
			return false, err
		}
		r.observeServer(ctx, res.ServerTime)
		if err := r.opt.Store.MarkMinutesSent(ctx, rows, final); err != nil {
			return false, err
		}
		r.countRejections(ctx, rows, res.Rejected)
	}
	return false, nil
}

// rejectAll is a result rejecting all n minutes with err.
func rejectAll(n int, err error) serverapi.MinutesResult {
	res := serverapi.MinutesResult{Rejected: make([]serverapi.ItemError, n)}
	for i := range res.Rejected {
		res.Rejected[i] = serverapi.ItemError{Index: i, Error: err.Error()}
	}
	return res
}

// sameRules keeps the leading rows that share the first row's rules version,
// since a request carries one version (only an agent upgrade mixes them).
func sameRules(rows []localdb.CreditedMinute) []localdb.CreditedMinute {
	for i, m := range rows {
		if m.RulesVersion != rows[0].RulesVersion {
			return rows[:i]
		}
	}
	return rows
}

func minutesRequest(rows []localdb.CreditedMinute) serverapi.MinutesRequest {
	req := serverapi.MinutesRequest{RulesVersion: rows[0].RulesVersion, Minutes: make([]serverapi.Minute, len(rows))}
	for i, m := range rows {
		req.Minutes[i] = serverapi.Minute{
			Minute: m.Minute.UTC(), Project: m.Project, Label: m.Label, Credited: m.Credited,
			Cuts: nonNil(m.Cuts), Flags: nonNil(m.Flags),
		}
	}
	return req
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func (r *Runner) countRejections(ctx context.Context, rows []localdb.CreditedMinute, rejected []serverapi.ItemError) {
	if len(rejected) == 0 {
		return
	}
	var sealed []localdb.CreditedMinute
	for i, e := range rejected {
		if e.Index < 0 || e.Index >= len(rows) {
			continue // a server bug; the count below still includes it
		}
		m := rows[e.Index]
		if isSealedRejection(e.Error) {
			sealed = append(sealed, m)
		}
		if i < maxLoggedRejections {
			r.opt.Logger.WarnContext(ctx, "server rejected a credited minute; not retrying",
				"minute", m.Minute.Format(time.RFC3339), "credited", m.Credited, "err", e.Error)
		}
	}
	if err := r.opt.Store.Add(ctx, keyRejected, int64(len(rejected))); err != nil {
		r.opt.Logger.WarnContext(ctx, "counting rejected minutes", "err", err)
	}
	if len(sealed) > 0 {
		r.reportSealed(ctx, sealed)
	}
}

// sealedRejection is how the server says a minute's UTC day is already
// sealed by an evidence record (internal/server/credited). The agent only
// seals a day once all its minutes are accepted, so this should never
// happen; if it does, the minute's credit is lost and the user must see it.
const sealedRejection = "already sealed"

func isSealedRejection(msg string) bool { return strings.Contains(msg, sealedRejection) }

// reportSealed shows minutes the server refused because their day was
// already sealed: they can never be sent, so they are counted (with the
// other rejections) and shown in status as an error, not dropped silently.
func (r *Runner) reportSealed(ctx context.Context, ms []localdb.CreditedMinute) {
	last := ms[len(ms)-1].Minute
	r.opt.Logger.ErrorContext(ctx, "server refused credited minutes because their day was already sealed; their credit is lost",
		"minutes", len(ms), "last", last.Format(time.RFC3339))
	if err := r.opt.Store.Add(ctx, keySealedRejected, int64(len(ms))); err != nil {
		r.opt.Logger.WarnContext(ctx, "counting minutes refused for a sealed day", "err", err)
	}
	r.persist(ctx, localdb.KV{Key: keySealedError, Value: fmt.Sprintf(
		"the server refused credited minutes up to %s because their day was already sealed; that credit is lost. Please report this.",
		last.Format(time.RFC3339))})
}

func (r *Runner) batchSize() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.batch
}

// shrink halves the batch size below n (at least 1); success grows it back.
func (r *Runner) shrink(n int) {
	r.mu.Lock()
	r.batch = max(1, min(r.batch, n)/2)
	r.mu.Unlock()
}

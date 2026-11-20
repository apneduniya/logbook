package crediting

import (
	"context"
	"time"

	"github.com/apneduniya/logbook/internal/agent/devicekey"
	"github.com/apneduniya/logbook/internal/agent/localdb"
	"github.com/apneduniya/logbook/internal/agent/serverapi"
)

// maxLoggedRejections bounds per-item rejection log lines per batch.
const maxLoggedRejections = 5

// uploadMinutes sends every stored minute whose current content the server
// doesn't have, in batches. Per-item rejections are logged, counted and
// never retried (unless the minute changes). A batch the server refuses as
// a whole is split, down to single minutes, which are then rejected.
func (r *Runner) uploadMinutes(ctx context.Context, t serverapi.Target, dev devicekey.Device, now time.Time) error {
	final := finalBefore(now)
	for range maxBatchesPerRun {
		size := r.batchSize()
		rows, err := r.opt.Store.UnsentMinutes(ctx, final, size)
		if err != nil {
			return err
		}
		if len(rows) == 0 {
			return nil
		}
		rows = sameRules(rows)
		res, err := r.opt.Server.UploadMinutes(ctx, t, dev.ID, minutesRequest(rows))
		if serverapi.KindOf(err) == serverapi.KindInvalid {
			if len(rows) > 1 {
				r.shrink(len(rows))
				r.opt.Logger.WarnContext(ctx, "server refused a minutes batch; splitting it", "size", len(rows), "err", err)
				continue
			}
			res, err = serverapi.MinutesResult{Rejected: []serverapi.ItemError{{Index: 0, Error: err.Error()}}}, nil
		}
		if err != nil {
			return err
		}
		if err := r.opt.Store.MarkMinutesSent(ctx, rows, final); err != nil {
			return err
		}
		r.countRejections(ctx, rows, res.Rejected)
		if len(rows) < size {
			return nil
		}
	}
	return nil
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
	for i, e := range rejected {
		if i == maxLoggedRejections {
			break
		}
		m := rows[e.Index]
		r.opt.Logger.WarnContext(ctx, "server rejected a credited minute; not retrying",
			"minute", m.Minute.Format(time.RFC3339), "credited", m.Credited, "err", e.Error)
	}
	if err := r.opt.Store.Add(ctx, keyRejected, int64(len(rejected))); err != nil {
		r.opt.Logger.WarnContext(ctx, "counting rejected minutes", "err", err)
	}
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

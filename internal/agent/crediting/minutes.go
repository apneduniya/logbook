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

// uploadMinutes sends every stored row the server hasn't answered for as it
// is, with its computation key, in batches of whole minutes (a minute's
// rows are one unit, and the server applies a minute's decreases before
// its increases under its 60-second cap only within one request). Open
// minutes the server would freeze before the agent settles are held back.
//
// The server's answers (applyAnswer): an accepted row is acknowledged; a
// row refused because its minute is final comes back with the rows the
// server holds, which replace the minute here, final and acknowledged; a
// final row refused for any other reason is dropped and counted. A batch
// refused for its size is split, down to
// single minutes; one refused as a whole for any other reason returns a
// *refusedError and nothing is marked.
//
// drained is true when nothing is left to send, and false when the run's
// budget (maxBatchesPerRun) ran out first; the rest goes on the next run.
func (r *Runner) uploadMinutes(ctx context.Context, t serverapi.Target, dev devicekey.Device, now time.Time, st settleState) (drained bool, err error) {
	hold := holdBefore(now, st)
	// after walks forward through the minutes, so no minute is read twice
	// in one run.
	var after time.Time
	for range maxBatchesPerRun {
		size := r.batchSize()
		rows, err := r.opt.Store.UnsentMinutes(ctx, after, hold, size)
		if err != nil {
			return false, err
		}
		if len(rows) == 0 {
			return true, nil
		}
		rows = sameRules(rows)
		if err := r.opt.Store.MarkMinutesTried(ctx, rows); err != nil {
			return false, err
		}
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
		if err := r.applyAnswer(ctx, rows, res.Rejected); err != nil {
			return false, err
		}
		after = rows[len(rows)-1].Minute
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
// A minute's rows come from one run, so this never splits a minute.
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
			Cuts: nonNil(m.Cuts), Flags: nonNil(m.Flags), ComputedAt: m.Key.T.UTC(), Snapshot: m.Key.H,
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

// applyAnswer records the server's per-row answer to a batch of rows.
//
//   - Accepted rows are acknowledged.
//   - A refusal carrying the server's stored rows means the minute is final
//     there. The server's value is the truth: the minute's rows are
//     replaced with exactly those rows (values and keys), final and
//     acknowledged. Stored rows without a key (from before keys) are
//     counted (crediting.unkeyed).
//   - Any other refusal of an open row marks it answered: it is not sent
//     again while open, and once more when it becomes final (the server's
//     reason may not hold for the final value).
//   - Any other refusal of a final row, or of a row in a day the server
//     has sealed, is for good: the server holds no such row, so the row is
//     dropped and counted (crediting.rejected). The minute stays final, and
//     the agent never keeps a final value the server doesn't hold (ADR
//     0017). Retrying it would never succeed and would hold back sealing
//     for good. A sealed-day refusal is also reported as lost credit.
func (r *Runner) applyAnswer(ctx context.Context, rows []localdb.CreditedMinute, rejected []serverapi.ItemError) error {
	refused := make(map[int]serverapi.ItemError, len(rejected))
	for _, e := range rejected {
		if e.Index >= 0 && e.Index < len(rows) {
			refused[e.Index] = e
		}
	}
	stored := map[time.Time][]serverapi.StoredMinute{}
	var answer localdb.MinutesAnswer
	var sealed []localdb.CreditedMinute
	logged := 0
	for i, m := range rows {
		e, ok := refused[i]
		switch {
		case !ok:
			answer.Accepted = append(answer.Accepted, m)
			continue
		case e.Stored != nil:
			stored[m.Minute] = e.Stored
			if isSealedRejection(e.Error) && lostTo(m, e.Stored) {
				// The server's rows are adopted, but credit the agent held
				// for a sealed day is lost: show it, as below.
				sealed = append(sealed, m)
			}
			continue
		case isSealedRejection(e.Error):
			sealed = append(sealed, m)
			answer.Dropped = append(answer.Dropped, m)
		case m.Final:
			answer.Dropped = append(answer.Dropped, m)
		default:
			answer.Answered = append(answer.Answered, m)
		}
		if logged < maxLoggedRejections {
			logged++
			r.opt.Logger.WarnContext(ctx, "server rejected a credited minute",
				"minute", m.Minute.Format(time.RFC3339), "project", m.Project, "final", m.Final, "err", e.Error)
		}
	}
	if err := r.opt.Store.MarkMinutesSent(ctx, answer); err != nil {
		return err
	}
	// The server keeps each row with the rules version it was uploaded
	// with: the batch's, as a batch carries one version.
	for minute, held := range stored {
		if err := r.adopt(ctx, minute, held, rows[0].RulesVersion); err != nil {
			return err
		}
	}
	if n := len(answer.Dropped); n > 0 {
		if err := r.opt.Store.Add(ctx, keyRejected, int64(n)); err != nil {
			r.opt.Logger.WarnContext(ctx, "counting rejected minutes", "err", err)
		}
	}
	if len(sealed) > 0 {
		r.reportSealed(ctx, sealed)
	}
	return nil
}

// adopt replaces minute's rows with the rows the server holds (ADR 0017).
func (r *Runner) adopt(ctx context.Context, minute time.Time, stored []serverapi.StoredMinute, rules string) error {
	rows := make([]localdb.CreditedMinute, 0, len(stored))
	unkeyed := 0
	for _, s := range stored {
		m := newRow(minute, s.Project, s.Label, s.Credited, s.Cuts, s.Flags, rules)
		if s.ComputedAt != nil && s.Snapshot != nil {
			m.Key = localdb.Key{T: s.ComputedAt.UTC(), H: *s.Snapshot}
		} else {
			unkeyed++
		}
		rows = append(rows, m)
	}
	if err := r.opt.Store.AdoptMinute(ctx, minute, rows); err != nil {
		return fmt.Errorf("adopting the server's rows for %s: %w", minute.Format(time.RFC3339), err)
	}
	r.opt.Logger.InfoContext(ctx, "the server holds this minute as final; keeping its value",
		"minute", minute.Format(time.RFC3339), "rows", len(rows), "unkeyed", unkeyed)
	if unkeyed > 0 {
		if err := r.opt.Store.Add(ctx, keyUnkeyed, int64(unkeyed)); err != nil {
			r.opt.Logger.WarnContext(ctx, "counting unkeyed minutes", "err", err)
		}
	}
	return nil
}

// sealedRejection is how the server says a minute's UTC day is already
// sealed by an evidence record (internal/server/credited). The agent only
// seals a day once all its minutes are final and acknowledged, so this
// should never happen; if it does, the minute's credit is lost and the user
// must see it.
const sealedRejection = "already sealed"

func isSealedRejection(msg string) bool { return strings.Contains(msg, sealedRejection) }

// lostTo reports whether adopting the server's rows loses credit the local
// row m holds: the server holds less, or nothing, for its project.
func lostTo(m localdb.CreditedMinute, held []serverapi.StoredMinute) bool {
	for _, s := range held {
		if s.Project == m.Project {
			return s.Credited < m.Credited
		}
	}
	return m.Credited > 0
}

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

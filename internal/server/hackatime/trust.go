package hackatime

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/apneduniya/logbook/internal/server/secretbox"
	"github.com/apneduniya/logbook/internal/server/store"
	"github.com/apneduniya/logbook/internal/server/store/db"
)

// TrustMaxAge is how old a trust level may get before it is re-read.
const TrustMaxAge = 24 * time.Hour

// trustSweepLimit caps how many refresh jobs one sweep queues.
const trustSweepLimit = 500

// TrustRefresher keeps hackatime_links.trust_level current.
type TrustRefresher struct {
	st     *store.Store
	box    *secretbox.Box
	client *Client
	log    *slog.Logger
	now    func() time.Time
}

// NewTrustRefresher builds a TrustRefresher. now may be nil (time.Now).
func NewTrustRefresher(st *store.Store, box *secretbox.Box, client *Client, logger *slog.Logger, now func() time.Time) *TrustRefresher {
	if logger == nil {
		logger = slog.Default()
	}
	if now == nil {
		now = time.Now
	}
	return &TrustRefresher{st: st, box: box, client: client, log: logger, now: now}
}

// DueUsers lists linked users whose trust level is older than TrustMaxAge.
func (r *TrustRefresher) DueUsers(ctx context.Context) ([]uuid.UUID, error) {
	cutoff := r.now().Add(-TrustMaxAge)
	ids, err := r.st.ListHackatimeLinksDueForTrustCheck(ctx, db.ListHackatimeLinksDueForTrustCheckParams{
		TrustCheckedAt: &cutoff, Limit: trustSweepLimit,
	})
	if err != nil {
		return nil, fmt.Errorf("listing hackatime links due for trust check: %w", err)
	}
	return ids, nil
}

// Refresh re-reads one user's trust level with the user's own token
// (GET /authenticated/me), which identifies them unambiguously.
//
// If Hackatime rejects the token (the user revoked Logbook's access), the
// level can no longer be verified. A red level is kept, so revoking access
// can't shed a ban; any other level becomes TrustUnknown until the user links
// again. Hackatime's public trust_factor endpoint is deliberately not used as
// a fallback: it matches the id against usernames and Slack ids too, without
// a defined order, so an alt account named after the user's numeric id could
// answer for them.
func (r *TrustRefresher) Refresh(ctx context.Context, userID uuid.UUID) error {
	link, err := r.st.GetHackatimeLink(ctx, userID)
	if store.IsNotFound(err) {
		return nil // unlinked
	}
	if err != nil {
		return fmt.Errorf("reading hackatime link: %w", err)
	}
	level, err := r.fetch(ctx, userID, link)
	if err != nil {
		return err
	}
	return r.st.InTx(ctx, func(q *db.Queries) error {
		n, err := q.UpdateHackatimeTrust(ctx, db.UpdateHackatimeTrustParams{
			UserID: userID, TrustLevel: string(level), LinkedAt: link.LinkedAt,
		})
		if err != nil {
			return fmt.Errorf("saving hackatime trust level: %w", err)
		}
		if n == 0 {
			return nil // relinked meanwhile; the new link stored its own level
		}
		if err := recordTrust(ctx, q, userID, level, r.now()); err != nil {
			return err
		}
		if TrustLevel(link.TrustLevel) == level {
			return nil
		}
		r.log.InfoContext(ctx, "hackatime trust level changed", "user_id", userID, "from", link.TrustLevel, "to", level)
		return audit(ctx, q, userID, "hackatime.trust_changed", map[string]any{
			"hackatime_user_id": link.HackatimeUserID, "from": link.TrustLevel, "to": level,
		})
	})
}

func (r *TrustRefresher) fetch(ctx context.Context, userID uuid.UUID, link db.HackatimeLink) (TrustLevel, error) {
	token, err := r.box.Open(link.AccessTokenEnc, userID[:])
	if err != nil {
		r.log.WarnContext(ctx, "hackatime token unreadable; trust unverifiable", "user_id", userID)
		return unverifiable(link), nil
	}
	me, err := r.client.Me(ctx, string(token))
	switch {
	case err == nil && me.ID == link.HackatimeUserID:
		return me.Trust, nil
	case err == nil:
		return "", fmt.Errorf("refreshing hackatime trust: token now belongs to user %d, not %d", me.ID, link.HackatimeUserID)
	case errors.Is(err, ErrUnauthorized):
		return unverifiable(link), nil
	default:
		return "", fmt.Errorf("refreshing hackatime trust: %w", err)
	}
}

// recordTrust adds a level read at checkedAt to the user's trust history
// (hackatime_trust_history): the newest row is extended while the level is
// unchanged, and a new row starts when it changes.
func recordTrust(ctx context.Context, q *db.Queries, userID uuid.UUID, level TrustLevel, checkedAt time.Time) error {
	if err := q.RecordHackatimeTrust(ctx, db.RecordHackatimeTrustParams{
		UserID: userID, Level: string(level), CheckedAt: checkedAt,
	}); err != nil {
		return fmt.Errorf("recording hackatime trust history: %w", err)
	}
	return nil
}

// unverifiable is the level to record when Hackatime can't be asked about
// the user any more: a ban sticks, anything else becomes unknown.
func unverifiable(link db.HackatimeLink) TrustLevel {
	if TrustLevel(link.TrustLevel).Banned() {
		return TrustRed
	}
	return TrustUnknown
}

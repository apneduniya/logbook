package hackatime

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

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

// Refresh re-reads one user's trust level. It prefers the user's own token
// (GET /authenticated/me), which identifies them unambiguously. If Hackatime
// rejects the token (the user revoked it), it falls back to the public
// trust_factor endpoint by numeric id so a revoked token can't freeze a
// trust level that has since turned red.
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
	return pgx.BeginFunc(ctx, r.st.Pool, func(tx pgx.Tx) error {
		q := db.New(tx)
		n, err := q.UpdateHackatimeTrust(ctx, db.UpdateHackatimeTrustParams{UserID: userID, TrustLevel: string(level)})
		if err != nil {
			return fmt.Errorf("saving hackatime trust level: %w", err)
		}
		if n == 0 || TrustLevel(link.TrustLevel) == level {
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
	if err == nil {
		me, err := r.client.Me(ctx, string(token))
		switch {
		case err == nil && me.ID == link.HackatimeUserID:
			return me.Trust, nil
		case err == nil:
			return "", fmt.Errorf("refreshing hackatime trust: token now belongs to user %d, not %d", me.ID, link.HackatimeUserID)
		case !errors.Is(err, ErrUnauthorized):
			return "", fmt.Errorf("refreshing hackatime trust: %w", err)
		}
	}
	level, err := r.client.TrustFactor(ctx, link.HackatimeUserID)
	if err != nil {
		return "", fmt.Errorf("refreshing hackatime trust: %w", err)
	}
	return level, nil
}

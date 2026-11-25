package submissions

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/apneduniya/logbook/internal/server/hackatime"
	"github.com/apneduniya/logbook/internal/server/store"
	"github.com/apneduniya/logbook/internal/server/store/db"
)

// trustState is a user's stored Hackatime trust level, unmasked, and the
// Hackatime account it belongs to.
type trustState struct {
	Linked      bool
	HackatimeID int64
	Level       hackatime.TrustLevel
	CheckedAt   *time.Time
}

// banned reports whether the user may not submit or be approved (ADR 0020).
func (t trustState) banned() bool { return t.Linked && t.Level.Banned() }

// readTrust reads the stored level.
func readTrust(ctx context.Context, q db.Querier, user uuid.UUID) (trustState, error) {
	link, err := q.GetHackatimeLink(ctx, user)
	if store.IsNotFound(err) {
		return trustState{}, nil
	}
	if err != nil {
		return trustState{}, fmt.Errorf("reading hackatime link: %w", err)
	}
	return trustState{
		Linked: true, HackatimeID: link.HackatimeUserID, Level: hackatime.TrustLevel(link.TrustLevel), CheckedAt: link.TrustCheckedAt,
	}, nil
}

// switchedAccount reports whether the submitter was linked to a Hackatime
// account at submission and isn't linked to that same account now
// (unlinked, or linked to another one): the level read now isn't the
// level of the account the hours were submitted under.
func switchedAccount(sub db.Submission, now trustState) bool {
	return sub.HackatimeUserID.Valid && (!now.Linked || now.HackatimeID != sub.HackatimeUserID.Int64)
}

// freshTrust is readTrust after re-reading the level from Hackatime when
// it is older than hackatime.TrustMaxAge (ADR 0020). If Hackatime can't be
// reached, the last known level decides; the refresher keeps red sticky.
func (s *Service) freshTrust(ctx context.Context, user uuid.UUID) (trustState, error) {
	t, err := readTrust(ctx, s.st, user)
	if err != nil || !t.Linked || s.trust == nil {
		return t, err
	}
	if t.CheckedAt != nil && s.now().Sub(*t.CheckedAt) < hackatime.TrustMaxAge {
		return t, nil
	}
	rctx, cancel := context.WithTimeout(ctx, s.trustTimeout)
	err = s.trust.Refresh(rctx, user)
	cancel()
	if err != nil {
		s.log.WarnContext(ctx, "refreshing hackatime trust failed; using the last known level", "user_id", user, "err", err)
		return t, nil
	}
	return readTrust(ctx, s.st, user)
}

// maskTrust is a level as reviewers see it, masked as Hackatime masks it
// publicly: yellow (suspected) and unknown read as blue, as does a user
// with no link (the packet says hackatime_linked separately).
func maskTrust(level hackatime.TrustLevel, linked bool) string {
	if !linked {
		return string(hackatime.TrustBlue)
	}
	switch l := level.Public(); l {
	case hackatime.TrustBlue, hackatime.TrustRed, hackatime.TrustGreen:
		return string(l)
	default:
		return string(hackatime.TrustBlue)
	}
}

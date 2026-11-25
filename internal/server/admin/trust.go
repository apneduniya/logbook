package admin

import (
	"context"
	"fmt"
	"net/http"
	"time"
)

// maxTrustLogs is how many trust changes user/trust_logs lists (Hackatime's
// recent.limit(25)).
const maxTrustLogs = 25

// TrustLog is one user/trust_logs entry in Hackatime's shape. Logbook
// doesn't decide trust levels, it reads them from Hackatime, so an entry is
// a level Logbook read that differs from the one read before it:
// previous_trust_level is null for the first level read; changed_by,
// reason, notes and the edit fields are null; created_at is when the level
// was first read.
type TrustLog struct {
	ID                 int64          `json:"id"`
	PreviousTrustLevel *string        `json:"previous_trust_level"`
	NewTrustLevel      string         `json:"new_trust_level"`
	ChangedBy          *struct{}      `json:"changed_by"`
	Reason             *string        `json:"reason"`
	Notes              *string        `json:"notes"`
	CreatedAt          string         `json:"created_at"`
	EditedAt           *string        `json:"edited_at"`
	EditedBy           *struct{}      `json:"edited_by"`
	OriginalReason     *string        `json:"original_reason"`
	OriginalNotes      *string        `json:"original_notes"`
	Logbook            trustLogExtras `json:"logbook"`
}

// trustLogExtras says how long the level held: first and last read.
type trustLogExtras struct {
	FirstCheckedAt time.Time `json:"first_checked_at"`
	LastCheckedAt  time.Time `json:"last_checked_at"`
}

// TrustLogs answers user/trust_logs for u: the newest 25 levels read from
// Hackatime, newest first.
func (s *Service) TrustLogs(ctx context.Context, u UserRow) ([]TrustLog, error) {
	rows, err := s.q.AdminTrustHistory(ctx, u.ID)
	if err != nil {
		return nil, fmt.Errorf("reading trust history: %w", err)
	}
	out := make([]TrustLog, 0, min(len(rows), maxTrustLogs))
	for i, r := range rows {
		if i == maxTrustLogs {
			break
		}
		e := TrustLog{
			ID: r.ID, NewTrustLevel: r.Level, CreatedAt: railsTime(r.FirstCheckedAt),
			Logbook: trustLogExtras{FirstCheckedAt: r.FirstCheckedAt.UTC(), LastCheckedAt: r.CheckedAt.UTC()},
		}
		if i+1 < len(rows) {
			prev := rows[i+1].Level
			e.PreviousTrustLevel = &prev
		}
		out = append(out, e)
	}
	return out, nil
}

func (h *Handler) trustLogs(w http.ResponseWriter, r *http.Request, c *call) error {
	u, err := h.user(r.Context(), c)
	if err != nil {
		return err
	}
	logs, err := h.svc.TrustLogs(r.Context(), u)
	if err != nil {
		return err
	}
	return writeJSON(w, r, c, map[string]any{"trust_logs": logs}, u.ID)
}

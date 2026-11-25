package admin

import (
	"context"
	"errors"
	"io"
	"time"

	"github.com/google/uuid"

	"github.com/apneduniya/logbook/internal/server/store/db"
	"github.com/apneduniya/logbook/internal/server/submissions"
)

// Reads are the queries the admin API serves from: every one is a SELECT
// in store/queries/admin.sql (a test checks both). *store.Store
// implements it.
type Reads interface {
	AdminUsers(ctx context.Context, publicIDs []int64) ([]db.AdminUsersRow, error)
	AdminUserByEmail(ctx context.Context, email string) (db.AdminUserByEmailRow, error)
	AdminSearchUsers(ctx context.Context, arg db.AdminSearchUsersParams) ([]db.AdminSearchUsersRow, error)
	AdminHeartbeatStats(ctx context.Context, userID uuid.UUID) (db.AdminHeartbeatStatsRow, error)
	AdminUserCounts(ctx context.Context, userID uuid.UUID) (db.AdminUserCountsRow, error)
	AdminUserHeartbeats(ctx context.Context, arg db.AdminUserHeartbeatsParams) ([]db.AdminUserHeartbeatsRow, error)
	AdminUserHeartbeatCount(ctx context.Context, arg db.AdminUserHeartbeatCountParams) (int64, error)
	AdminUserProjects(ctx context.Context, arg db.AdminUserProjectsParams) ([]db.AdminUserProjectsRow, error)
	AdminTrustHistory(ctx context.Context, userID uuid.UUID) ([]db.AdminTrustHistoryRow, error)
	AdminDevices(ctx context.Context, userID uuid.UUID) ([]db.AdminDevicesRow, error)
	AdminEvidenceDays(ctx context.Context, userID uuid.UUID) ([]db.AdminEvidenceDaysRow, error)
	AdminEvidenceConflicts(ctx context.Context, deviceIDs []string) ([]db.AdminEvidenceConflictsRow, error)
	AdminCreditedMinutes(ctx context.Context, arg db.AdminCreditedMinutesParams) ([]db.AdminCreditedMinutesRow, error)
	AdminSubmissions(ctx context.Context, arg db.AdminSubmissionsParams) ([]db.AdminSubmissionsRow, error)
	AdminSubmissionOwner(ctx context.Context, id uuid.UUID) (db.AdminSubmissionOwnerRow, error)
	AdminSubmissionDecisions(ctx context.Context, ids []uuid.UUID) ([]db.AdminSubmissionDecisionsRow, error)
	AdminSubmissionParts(ctx context.Context, submissionID uuid.UUID) ([]db.AdminSubmissionPartsRow, error)
	AdminSubmissionMinutes(ctx context.Context, submissionID uuid.UUID) ([]db.AdminSubmissionMinutesRow, error)
	AdminAuditLog(ctx context.Context, arg db.AdminAuditLogParams) ([]db.AdminAuditLogRow, error)
	AdminAuditNextID(ctx context.Context) (int64, error)
}

// Evidence reads what a submission stored; *submissions.Checker
// implements it. None of these methods writes.
type Evidence interface {
	// CheckBundle is nil when the bundle can be exported, else
	// submissions.ErrNotFound or submissions.ErrEvidenceDeleted.
	CheckBundle(ctx context.Context, id uuid.UUID) error
	// WriteBundle streams the bundle's JSON to w, one stored part at a time.
	WriteBundle(ctx context.Context, id uuid.UUID, w io.Writer) error
	// PartRows decodes one page of a part's opened rows within a decode slot.
	PartRows(ctx context.Context, id, device uuid.UUID, offset, limit int) ([]submissions.EvidenceRow, int, error)
}

var _ Evidence = (*submissions.Checker)(nil)

// ErrNotFound is a user or submission that doesn't exist (404).
var ErrNotFound = errors.New("not found")

// Service answers the admin API's reads.
type Service struct {
	q   Reads
	ev  Evidence
	now func() time.Time
}

// NewService returns a Service. now may be nil (time.Now).
func NewService(q Reads, ev Evidence, now func() time.Time) *Service {
	if now == nil {
		now = time.Now
	}
	return &Service{q: q, ev: ev, now: func() time.Time { return now().UTC() }}
}

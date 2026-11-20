package ingest

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/apneduniya/logbook/internal/server/ingest/rollup"
	"github.com/apneduniya/logbook/internal/server/store/db"
	"github.com/apneduniya/logbook/pkg/wire"
)

// Heartbeat sources, matching the heartbeats.source check constraint.
const (
	SourceEditor = "editor"
	SourceAgent  = "agent"
	SourceImport = "import"
)

// ErrInvalidSource is returned for a source other than the Source* constants.
var ErrInvalidSource = errors.New("invalid heartbeat source")

// Publisher is told which of a user's minutes changed, after the change is
// committed. internal/server/live.Hub implements it.
type Publisher interface {
	MinutesChanged(userID uuid.UUID, from, to time.Time)
}

// Store is the database access the service needs. *store.Store satisfies it.
type Store interface {
	InTx(ctx context.Context, fn func(q *db.Queries) error) error
	GetUserByID(ctx context.Context, id uuid.UUID) (db.User, error)
	SumUserMinutesByGroup(ctx context.Context, arg db.SumUserMinutesByGroupParams) ([]db.SumUserMinutesByGroupRow, error)
}

// Service ingests heartbeats and maintains user_minutes.
type Service struct {
	store Store
	pub   Publisher
	now   func() time.Time
	locs  sync.Map // timezone name -> *time.Location
}

// Option configures a Service.
type Option func(*Service)

// WithClock overrides the clock used to validate heartbeat times and to find
// "today". Tests use it; production uses time.Now.
func WithClock(now func() time.Time) Option {
	return func(s *Service) { s.now = now }
}

// New returns a Service. pub may be nil when nobody needs change events.
func New(st Store, pub Publisher, opts ...Option) *Service {
	s := &Service{store: st, pub: pub, now: time.Now}
	for _, o := range opts {
		o(s)
	}
	return s
}

// ItemResult is the outcome for one heartbeat of a batch.
type ItemResult struct {
	// Heartbeat is the normalised heartbeat that was (or would have been) stored.
	Heartbeat wire.Heartbeat
	// ID identifies the stored heartbeat: the hex fields hash. Empty when Err is set.
	ID string
	// Err is set when the heartbeat was rejected; it wraps wire.ErrInvalidHeartbeat.
	Err error
}

// BatchResult is the outcome of IngestBatch.
type BatchResult struct {
	// Items has one entry per input heartbeat, in input order.
	Items []ItemResult
	// Inserted counts heartbeats that were new; duplicates are accepted but not counted.
	Inserted int
}

// Ingest stores hbs for userID and returns how many were new. Invalid
// heartbeats are skipped; use IngestBatch to see why. A non-nil err means
// nothing from the batch was stored. The Hackatime importer calls this.
func (s *Service) Ingest(ctx context.Context, userID uuid.UUID, hbs []wire.Heartbeat, source string) (inserted int, err error) {
	res, err := s.IngestBatch(ctx, userID, hbs, source)
	if err != nil {
		return 0, err
	}
	return res.Inserted, nil
}

// IngestBatch validates, deduplicates and stores hbs for userID in one
// transaction, recomputes the affected rollups, and publishes the change
// after commit. Invalid heartbeats are reported per item and don't fail the
// batch.
func (s *Service) IngestBatch(ctx context.Context, userID uuid.UUID, hbs []wire.Heartbeat, source string) (BatchResult, error) {
	switch source {
	case SourceEditor, SourceAgent, SourceImport:
	default:
		return BatchResult{}, fmt.Errorf("%w: %q", ErrInvalidSource, source)
	}

	now := s.now()
	res := BatchResult{Items: make([]ItemResult, len(hbs))}
	rows := make([]heartbeatRow, 0, len(hbs))
	seen := make(map[string]bool, len(hbs))
	for i, raw := range hbs {
		h := normalize(raw)
		res.Items[i].Heartbeat = h
		if err := validate(h, now); err != nil {
			res.Items[i].Err = err
			continue
		}
		hash := h.FieldsHash()
		id := hex.EncodeToString(hash)
		res.Items[i].ID = id
		if seen[id] {
			continue // the hash covers time too, so this is an exact repeat
		}
		seen[id] = true
		rows = append(rows, newHeartbeatRow(h, id))
	}
	if len(rows) == 0 {
		return res, nil
	}
	payload, err := json.Marshal(rows)
	if err != nil {
		return BatchResult{}, fmt.Errorf("encoding heartbeat batch: %w", err)
	}

	var windows []rollup.Range
	err = s.store.InTx(ctx, func(q *db.Queries) error {
		if err := q.LockUserIngest(ctx, userID); err != nil {
			return fmt.Errorf("locking user ingest: %w", err)
		}
		times, err := q.InsertHeartbeats(ctx, db.InsertHeartbeatsParams{Rows: payload, UserID: userID, Source: source})
		if err != nil {
			return fmt.Errorf("inserting heartbeats: %w", err)
		}
		res.Inserted = len(times)
		windows = rollup.Windows(times)
		return recompute(ctx, q, userID, windows)
	})
	if err != nil {
		return BatchResult{}, err
	}
	if len(windows) > 0 && s.pub != nil {
		s.pub.MinutesChanged(userID, windows[0].Start, windows[len(windows)-1].End)
	}
	return res, nil
}

// recompute rebuilds the user's 'you' rollup rows inside windows from the
// stored heartbeats: one read, one write.
func recompute(ctx context.Context, q *db.Queries, userID uuid.UUID, windows []rollup.Range) error {
	if len(windows) == 0 {
		return nil
	}
	readStarts := make([]time.Time, len(windows))
	readEnds := make([]time.Time, len(windows))
	winStarts := make([]time.Time, len(windows))
	winEnds := make([]time.Time, len(windows))
	for i, w := range windows {
		readStarts[i], readEnds[i] = rollup.ReadRange(w)
		winStarts[i], winEnds[i] = w.Start, w.End
	}
	stored, err := q.ListRollupHeartbeats(ctx, db.ListRollupHeartbeatsParams{
		RangeStarts: readStarts,
		RangeEnds:   readEnds,
		UserID:      userID,
		MinTime:     readStarts[0],
		MaxTime:     readEnds[len(readEnds)-1],
	})
	if err != nil {
		return fmt.Errorf("reading heartbeats for rollup: %w", err)
	}
	hbs := make([]rollup.Heartbeat, len(stored))
	for i, h := range stored {
		hbs[i] = rollup.Heartbeat{Time: h.Time, Key: rollup.Key{
			Project: h.Project, Language: h.Language, Editor: h.Editor, Category: h.Category,
		}}
	}
	rows := rollup.Compute(hbs, windows)

	p := db.ReplaceUserMinutesParams{
		WindowStarts: winStarts,
		WindowEnds:   winEnds,
		UserID:       userID,
		Minutes:      make([]time.Time, len(rows)),
		Projects:     make([]string, len(rows)),
		Languages:    make([]string, len(rows)),
		Editors:      make([]string, len(rows)),
		Categories:   make([]string, len(rows)),
		Seconds:      make([]int16, len(rows)),
	}
	for i, r := range rows {
		p.Minutes[i], p.Seconds[i] = r.Minute, r.Seconds
		p.Projects[i], p.Languages[i], p.Editors[i], p.Categories[i] = r.Key.Project, r.Key.Language, r.Key.Editor, r.Key.Category
	}
	if err := q.ReplaceUserMinutes(ctx, p); err != nil {
		return fmt.Errorf("replacing user minutes: %w", err)
	}
	return nil
}

// heartbeatRow is one element of the JSON array InsertHeartbeats decodes
// with jsonb_to_recordset. Keys must match the column list in ingest.sql.
type heartbeatRow struct {
	Time             time.Time `json:"time"`
	FieldsHash       string    `json:"fields_hash"`
	Entity           string    `json:"entity"`
	Type             string    `json:"type"`
	Category         string    `json:"category"`
	Project          string    `json:"project"`
	Branch           string    `json:"branch"`
	Language         string    `json:"language"`
	Editor           string    `json:"editor"`
	Plugin           string    `json:"plugin"`
	Machine          string    `json:"machine"`
	OperatingSystem  string    `json:"operating_system"`
	UserAgent        string    `json:"user_agent"`
	IsWrite          bool      `json:"is_write"`
	Lines            *int      `json:"lines"`
	Lineno           *int      `json:"lineno"`
	Cursorpos        *int      `json:"cursorpos"`
	LineAdditions    *int      `json:"line_additions"`
	LineDeletions    *int      `json:"line_deletions"`
	ProjectRootCount *int      `json:"project_root_count"`
	Dependencies     []string  `json:"dependencies"`
	AIModel          *string   `json:"ai_model"`
	AISession        *string   `json:"ai_session"`
	AIInputTokens    *int64    `json:"ai_input_tokens"`
	AIOutputTokens   *int64    `json:"ai_output_tokens"`
	AIPromptLength   *int      `json:"ai_prompt_length"`
	AILineChanges    *int      `json:"ai_line_changes"`
	HumanLineChanges *int      `json:"human_line_changes"`
}

func newHeartbeatRow(h wire.Heartbeat, fieldsHash string) heartbeatRow {
	deps := h.Dependencies
	if deps == nil {
		deps = []string{}
	}
	return heartbeatRow{
		Time:             h.Timestamp(),
		FieldsHash:       fieldsHash,
		Entity:           h.Entity,
		Type:             h.Type,
		Category:         h.Category,
		Project:          h.Project,
		Branch:           h.Branch,
		Language:         h.Language,
		Editor:           h.Editor,
		Plugin:           h.Plugin,
		Machine:          h.Machine,
		OperatingSystem:  h.OperatingSystem,
		UserAgent:        h.UserAgent,
		IsWrite:          h.IsWrite,
		Lines:            h.Lines,
		Lineno:           h.Lineno,
		Cursorpos:        h.Cursorpos,
		LineAdditions:    h.LineAdditions,
		LineDeletions:    h.LineDeletions,
		ProjectRootCount: h.ProjectRootCount,
		Dependencies:     deps,
		AIModel:          nonEmpty(h.AIModel),
		AISession:        nonEmpty(h.AISession),
		AIInputTokens:    h.AIInputTokens,
		AIOutputTokens:   h.AIOutputTokens,
		AIPromptLength:   h.AIPromptLength,
		AILineChanges:    h.AILineChanges,
		HumanLineChanges: h.HumanLineChanges,
	}
}

// nonEmpty maps "" to NULL for the nullable text columns.
func nonEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

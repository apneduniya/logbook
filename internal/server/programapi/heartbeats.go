package programapi

import (
	"context"
	"encoding/hex"
	"fmt"
	"net/url"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/apneduniya/logbook/internal/server/store/db"
)

// MaxHeartbeats caps one my/heartbeats response. Hackatime has no cap; a
// truncated response says so in logbook.notes.
const MaxHeartbeats = 25_000

// Heartbeat is one heartbeat as Hackatime renders its model to JSON. Empty
// text fields are null, as Hackatime stores what clients leave out. user_id
// is the public id.
type Heartbeat struct {
	ID               int64    `json:"id"`
	UserID           int64    `json:"user_id"`
	Time             float64  `json:"time"`
	Entity           *string  `json:"entity"`
	Type             *string  `json:"type"`
	Category         *string  `json:"category"`
	Project          *string  `json:"project"`
	Branch           *string  `json:"branch"`
	Language         *string  `json:"language"`
	Editor           *string  `json:"editor"`
	OperatingSystem  *string  `json:"operating_system"`
	Machine          *string  `json:"machine"`
	UserAgent        *string  `json:"user_agent"`
	IsWrite          bool     `json:"is_write"`
	Lines            *int32   `json:"lines"`
	Lineno           *int32   `json:"lineno"`
	Cursorpos        *int32   `json:"cursorpos"`
	LineAdditions    *int32   `json:"line_additions"`
	LineDeletions    *int32   `json:"line_deletions"`
	ProjectRootCount *int32   `json:"project_root_count"`
	Dependencies     []string `json:"dependencies"`
	SourceType       string   `json:"source_type"`
	AIModel          *string  `json:"ai_model"`
	AISession        *string  `json:"ai_session"`
	AIInputTokens    *int64   `json:"ai_input_tokens"`
	AIOutputTokens   *int64   `json:"ai_output_tokens"`
	AIPromptLength   *int32   `json:"ai_prompt_length"`
	AILineChanges    *int32   `json:"ai_line_changes"`
	HumanLineChanges *int32   `json:"human_line_changes"`
	FieldsHash       string   `json:"fields_hash"`
	CreatedAt        string   `json:"created_at"`
	UpdatedAt        string   `json:"updated_at"`
	DeletedAt        *string  `json:"deleted_at"`
}

// HeartbeatsBody is the my/heartbeats response, plus credited_seconds and
// raw_seconds.
type HeartbeatsBody struct {
	StartTime       string      `json:"start_time"`
	EndTime         string      `json:"end_time"`
	TotalSeconds    int64       `json:"total_seconds"`
	Heartbeats      []Heartbeat `json:"heartbeats"`
	CreditedSeconds int64       `json:"credited_seconds"`
	RawSeconds      int64       `json:"raw_seconds"`
	Logbook         *Extension  `json:"logbook,omitempty"`
}

// HeartbeatsRequest is a parsed my/heartbeats request.
type HeartbeatsRequest struct {
	From, To time.Time
	Raw      bool
}

// ParseHeartbeatsRequest reads start_time and end_time (defaults: the start
// and end of today, UTC). Hackatime fails with a 500 on an unparsable time;
// Logbook answers 422 {"error": "Invalid start_time"}.
func ParseHeartbeatsRequest(q url.Values, now time.Time) (HeartbeatsRequest, error) {
	y, m, d := now.UTC().Date()
	req := HeartbeatsRequest{From: time.Date(y, m, d, 0, 0, 0, 0, time.UTC), To: endOfDay(now), Raw: isTrue(q, "raw")}
	for _, p := range []struct {
		name string
		dst  *time.Time
	}{{"start_time", &req.From}, {"end_time", &req.To}} {
		t, present, ok := timeParam(q, p.name)
		if present && !ok {
			return HeartbeatsRequest{}, &ParamError{Message: "Invalid " + p.name}
		}
		if present {
			*p.dst = t
		}
	}
	return req, nil
}

// MyHeartbeats answers my/heartbeats for u: the heartbeats in [From, To]
// and the time over [From, To).
func (s *Service) MyHeartbeats(ctx context.Context, u User, req HeartbeatsRequest) (HeartbeatsBody, error) {
	st, err := s.Settings(ctx, u.ID)
	if err != nil {
		return HeartbeatsBody{}, err
	}
	rows, err := s.Heartbeats(ctx, u.ID, req.From, req.To, st.Hidden, MaxHeartbeats+1)
	if err != nil {
		return HeartbeatsBody{}, err
	}
	rng := Range{From: req.From, To: req.To}
	raw, err := s.Raw(ctx, u.ID, rng, RawQuery{ExcludeProjects: st.Hidden})
	if err != nil {
		return HeartbeatsBody{}, err
	}
	credited, err := s.Credited(ctx, u.ID, rng, nil, st.Hidden)
	if err != nil {
		return HeartbeatsBody{}, err
	}
	body := HeartbeatsBody{
		StartTime: railsTime(req.From), EndTime: railsTime(req.To),
		RawSeconds: sumRaw(raw), CreditedSeconds: sumCredited(credited),
		Heartbeats: make([]Heartbeat, 0, min(len(rows), MaxHeartbeats)),
	}
	body.TotalSeconds = pick(req.Raw, body.RawSeconds, body.CreditedSeconds)
	if len(rows) > MaxHeartbeats {
		rows = rows[:MaxHeartbeats]
		body.Logbook = &Extension{Notes: []string{
			fmt.Sprintf("only the first %d heartbeats are listed; ask for a shorter range", MaxHeartbeats),
		}}
	}
	for _, r := range rows {
		body.Heartbeats = append(body.Heartbeats, toHeartbeat(r, u.PublicID))
	}
	return body, nil
}

func toHeartbeat(r db.ProgramHeartbeatsRow, publicID int64) Heartbeat {
	deps := r.Dependencies
	if deps == nil {
		deps = []string{}
	}
	source := "direct_entry"
	if r.Source == "import" {
		source = "wakapi_import"
	}
	created := railsTime(r.CreatedAt)
	return Heartbeat{
		ID: r.ID, UserID: publicID, Time: unixSeconds(r.Time),
		Entity: nullStr(r.Entity), Type: nullStr(r.Type), Category: nullStr(r.Category),
		Project: nullStr(r.Project), Branch: nullStr(r.Branch), Language: nullStr(r.Language),
		Editor: nullStr(r.Editor), OperatingSystem: nullStr(r.OperatingSystem), Machine: nullStr(r.Machine),
		UserAgent: nullStr(r.UserAgent), IsWrite: r.IsWrite,
		Lines: optInt32(r.Lines), Lineno: optInt32(r.Lineno), Cursorpos: optInt32(r.Cursorpos),
		LineAdditions: optInt32(r.LineAdditions), LineDeletions: optInt32(r.LineDeletions),
		ProjectRootCount: optInt32(r.ProjectRootCount), Dependencies: deps, SourceType: source,
		AIModel: optText(r.AiModel), AISession: optText(r.AiSession),
		AIInputTokens: optInt64(r.AiInputTokens), AIOutputTokens: optInt64(r.AiOutputTokens),
		AIPromptLength: optInt32(r.AiPromptLength), AILineChanges: optInt32(r.AiLineChanges),
		HumanLineChanges: optInt32(r.HumanLineChanges),
		FieldsHash:       hex.EncodeToString(r.FieldsHash), CreatedAt: created, UpdatedAt: created,
	}
}

func nullStr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func optText(t pgtype.Text) *string {
	if !t.Valid {
		return nil
	}
	return &t.String
}

func optInt32(v pgtype.Int4) *int32 {
	if !v.Valid {
		return nil
	}
	return &v.Int32
}

func optInt64(v pgtype.Int8) *int64 {
	if !v.Valid {
		return nil
	}
	return &v.Int64
}

// SpansRequest is a parsed heartbeats/spans request.
type SpansRequest struct {
	From, To time.Time
	Projects []string // nil means all, empty means none
}

// ParseSpansRequest reads start_date and end_date as the stats call does,
// and project (one name, or the project[] array) or else filter_by_project
// (a comma list).
func ParseSpansRequest(q url.Values, now time.Time) (SpansRequest, error) {
	from := now.UTC().AddDate(-10, 0, 0)
	if t, present, ok := timeParam(q, "start_date"); present && !ok {
		return SpansRequest{}, &ParamError{Message: "Invalid start_date"}
	} else if present {
		from = t
	}
	to := endOfDay(now)
	if t, present, ok := timeParam(q, "end_date"); present && !ok {
		return SpansRequest{}, &ParamError{Message: "Invalid end_date"}
	} else if present {
		to = t
	}
	req := SpansRequest{From: from, To: to}
	if p := q.Get("project"); p != "" {
		req.Projects = []string{p}
	} else if ps := q["project[]"]; len(ps) > 0 {
		req.Projects = ps // Stardance sends project[]=a&project[]=b; Rails matches any
	} else {
		req.Projects = listParam(q, "filter_by_project")
	}
	if !validList(req.Projects) {
		return SpansRequest{}, &ParamError{Message: "Invalid filter"}
	}
	return req, nil
}

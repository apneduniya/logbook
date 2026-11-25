package admin

import (
	"context"
	"encoding/hex"
	"fmt"
	"math"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/apneduniya/logbook/internal/server/store/db"
)

// user/heartbeats' page size (limit: default 1000, 1 to 5000).
const (
	defaultHeartbeats = 1000
	maxHeartbeats     = 5000
)

// HeartbeatsQuery is a parsed user/heartbeats request.
type HeartbeatsQuery struct {
	Range                                      timeRange
	Project, Language, Entity, Editor, Machine string // "" is no filter
	Limit, Offset                              int64
	IncludeAI                                  bool
}

// parseHeartbeats reads user/heartbeats' parameters as Hackatime does; now
// completes dates that leave out the year, month or day.
func parseHeartbeats(p params, now time.Time) (HeartbeatsQuery, error) {
	q := HeartbeatsQuery{
		Limit:     p.intParam("limit", defaultHeartbeats, 1, maxHeartbeats),
		Offset:    p.intParam("offset", 0, 0, math.MaxInt32),
		IncludeAI: p.boolParam("include_ai"),
	}
	rg, bad := p.applyTimeRange(now)
	if bad != "" {
		return q, unprocessable("invalid " + bad)
	}
	q.Range = rg
	for _, f := range []struct {
		name string
		dst  *string
	}{{"project", &q.Project}, {"language", &q.Language}, {"entity", &q.Entity}, {"editor", &q.Editor}, {"machine", &q.Machine}} {
		if p.present(f.name) {
			*f.dst = p[f.name]
		}
	}
	return q, nil
}

func filter(s string) pgtype.Text { return pgtype.Text{String: s, Valid: s != ""} }

// heartbeatJSON is one user/heartbeats row: Hackatime's
// HEARTBEAT_RESPONSE_COLUMNS, its AI columns with include_ai, hidden and
// ja4, in its order, then a logbook object. Fields Logbook doesn't record
// (ip_address, ai_subscription_plan, ja4) are null; empty text is null, as
// Hackatime stores what a client leaves out; nothing is hidden.
type heartbeatJSON struct {
	ID               int64    `json:"id"`
	Time             float64  `json:"time"`
	CreatedAt        string   `json:"created_at"`
	Lineno           *int32   `json:"lineno"`
	Cursorpos        *int32   `json:"cursorpos"`
	IsWrite          bool     `json:"is_write"`
	Project          *string  `json:"project"`
	Language         *string  `json:"language"`
	Entity           *string  `json:"entity"`
	Branch           *string  `json:"branch"`
	Category         *string  `json:"category"`
	Dependencies     []string `json:"dependencies"`
	Editor           *string  `json:"editor"`
	Machine          *string  `json:"machine"`
	OperatingSystem  *string  `json:"operating_system"`
	Type             *string  `json:"type"`
	ProjectRootCount *int32   `json:"project_root_count"`
	UserAgent        *string  `json:"user_agent"`
	LineAdditions    *int32   `json:"line_additions"`
	LineDeletions    *int32   `json:"line_deletions"`
	IPAddress        *string  `json:"ip_address"`
	Lines            *int32   `json:"lines"`
	SourceType       string   `json:"source_type"`
	*aiJSON
	Hidden  bool            `json:"hidden"`
	JA4     *string         `json:"ja4"`
	Logbook heartbeatExtras `json:"logbook"`
}

// aiJSON are Heartbeat.ai_indexed_attributes, present with include_ai.
type aiJSON struct {
	AIModel            *string `json:"ai_model"`
	AISession          *string `json:"ai_session"`
	AISubscriptionPlan *string `json:"ai_subscription_plan"`
	AIInputTokens      *int64  `json:"ai_input_tokens"`
	AIOutputTokens     *int64  `json:"ai_output_tokens"`
	AIPromptLength     *int32  `json:"ai_prompt_length"`
	AILineChanges      *int32  `json:"ai_line_changes"`
	HumanLineChanges   *int32  `json:"human_line_changes"`
}

// heartbeatExtras is what Logbook adds to a heartbeat.
type heartbeatExtras struct {
	// Source is editor, agent or import (from Hackatime).
	Source     string  `json:"source"`
	Plugin     *string `json:"plugin"`
	FieldsHash string  `json:"fields_hash"`
}

// HeartbeatsBody is the user/heartbeats response.
type HeartbeatsBody struct {
	UserID     int64           `json:"user_id"`
	Heartbeats []heartbeatJSON `json:"heartbeats"`
	TotalCount int64           `json:"total_count"`
	HasMore    bool            `json:"has_more"`
}

// Heartbeats answers user/heartbeats for u: one page in (time, id) order,
// how many match, and whether offset + limit is below that.
func (s *Service) Heartbeats(ctx context.Context, u UserRow, q HeartbeatsQuery) (HeartbeatsBody, error) {
	count := db.AdminUserHeartbeatCountParams{
		UserID: u.ID, FromTime: q.Range.From, ToTime: q.Range.To,
		Project: filter(q.Project), Language: filter(q.Language), Entity: filter(q.Entity),
		Editor: filter(q.Editor), Machine: filter(q.Machine),
	}
	total, err := s.q.AdminUserHeartbeatCount(ctx, count)
	if err != nil {
		return HeartbeatsBody{}, fmt.Errorf("counting heartbeats: %w", err)
	}
	rows, err := s.q.AdminUserHeartbeats(ctx, db.AdminUserHeartbeatsParams{
		UserID: count.UserID, FromTime: count.FromTime, ToTime: count.ToTime,
		Project: count.Project, Language: count.Language, Entity: count.Entity, Editor: count.Editor, Machine: count.Machine,
		MaxRows: int32(q.Limit), SkipRows: int32(q.Offset), //nolint:gosec // both clamped to int32
	})
	if err != nil {
		return HeartbeatsBody{}, fmt.Errorf("reading heartbeats: %w", err)
	}
	body := HeartbeatsBody{
		UserID: u.PublicID, Heartbeats: make([]heartbeatJSON, len(rows)), TotalCount: total,
		HasMore: q.Offset+q.Limit < total,
	}
	for i, r := range rows {
		body.Heartbeats[i] = toHeartbeat(r, q.IncludeAI)
	}
	return body, nil
}

func toHeartbeat(r db.AdminUserHeartbeatsRow, ai bool) heartbeatJSON {
	deps := r.Dependencies
	if deps == nil {
		deps = []string{}
	}
	source := "direct_entry"
	if r.Source == "import" {
		source = "wakapi_import"
	}
	h := heartbeatJSON{
		ID: r.ID, Time: unixSeconds(r.Time), CreatedAt: railsTime(r.CreatedAt),
		Lineno: optInt4(r.Lineno), Cursorpos: optInt4(r.Cursorpos), IsWrite: r.IsWrite,
		Project: nullStr(r.Project), Language: nullStr(r.Language), Entity: nullStr(r.Entity),
		Branch: nullStr(r.Branch), Category: nullStr(r.Category), Dependencies: deps,
		Editor: nullStr(r.Editor), Machine: nullStr(r.Machine), OperatingSystem: nullStr(r.OperatingSystem),
		Type: nullStr(r.Type), ProjectRootCount: optInt4(r.ProjectRootCount), UserAgent: nullStr(r.UserAgent),
		LineAdditions: optInt4(r.LineAdditions), LineDeletions: optInt4(r.LineDeletions), Lines: optInt4(r.Lines),
		SourceType: source,
		Logbook:    heartbeatExtras{Source: r.Source, Plugin: nullStr(r.Plugin), FieldsHash: hex.EncodeToString(r.FieldsHash)},
	}
	if ai {
		h.aiJSON = &aiJSON{
			AIModel: optText(r.AiModel), AISession: optText(r.AiSession),
			AIInputTokens: optInt8(r.AiInputTokens), AIOutputTokens: optInt8(r.AiOutputTokens),
			AIPromptLength: optInt4(r.AiPromptLength), AILineChanges: optInt4(r.AiLineChanges),
			HumanLineChanges: optInt4(r.HumanLineChanges),
		}
	}
	return h
}

func optInt4(v pgtype.Int4) *int32 {
	if !v.Valid {
		return nil
	}
	return &v.Int32
}

func (h *Handler) userHeartbeats(w http.ResponseWriter, r *http.Request, c *call) error {
	u, err := h.user(r.Context(), c)
	if err != nil {
		return err
	}
	q, err := parseHeartbeats(c.p, h.svc.now())
	if err != nil {
		return err
	}
	body, err := h.svc.Heartbeats(r.Context(), u, q)
	if err != nil {
		return err
	}
	return writeJSON(w, r, c, body, u.ID)
}

// projectJSON is one user/projects row. repo_mapping_id is null: Logbook
// keeps a project's repo URL on the project itself.
type projectJSON struct {
	Name            string        `json:"name"`
	TotalHeartbeats int64         `json:"total_heartbeats"`
	TotalDuration   int64         `json:"total_duration"`
	FirstHeartbeat  float64       `json:"first_heartbeat"`
	LastHeartbeat   float64       `json:"last_heartbeat"`
	Languages       []string      `json:"languages"`
	Repo            *string       `json:"repo"`
	RepoMappingID   *int64        `json:"repo_mapping_id"`
	Archived        bool          `json:"archived"`
	Logbook         projectExtras `json:"logbook"`
}

// projectExtras is what Logbook adds to a project: its counted (capped
// credited) seconds over the same span, and whether the user hid it.
type projectExtras struct {
	CreditedSeconds int64 `json:"credited_seconds"`
	Hidden          bool  `json:"hidden"`
}

// ProjectsBody is the user/projects response. username is Hackatime's
// display_name.
type ProjectsBody struct {
	UserID        int64         `json:"user_id"`
	Username      string        `json:"username"`
	Projects      []projectJSON `json:"projects"`
	TotalProjects int           `json:"total_projects"`
}

// Projects answers user/projects for u over rg (open bounds without
// start_date and end_date).
func (s *Service) Projects(ctx context.Context, u UserRow, rg timeRange) (ProjectsBody, error) {
	rows, err := s.q.AdminUserProjects(ctx, db.AdminUserProjectsParams{UserID: u.ID, FromTime: rg.From, ToTime: rg.To})
	if err != nil {
		return ProjectsBody{}, fmt.Errorf("reading projects: %w", err)
	}
	body := ProjectsBody{UserID: u.PublicID, Username: displayName(u), Projects: make([]projectJSON, len(rows)), TotalProjects: len(rows)}
	for i, r := range rows {
		langs := r.Languages
		if langs == nil {
			langs = []string{}
		}
		body.Projects[i] = projectJSON{
			Name: r.Project, TotalHeartbeats: r.Heartbeats, TotalDuration: int64(r.Duration),
			FirstHeartbeat: unixSeconds(r.FirstTime), LastHeartbeat: unixSeconds(r.LastTime),
			Languages: langs, Repo: nullStr(r.RepoUrl), Archived: r.Archived,
			Logbook: projectExtras{CreditedSeconds: r.CreditedSeconds, Hidden: r.Hidden},
		}
	}
	return body, nil
}

func (h *Handler) userProjects(w http.ResponseWriter, r *http.Request, c *call) error {
	u, err := h.user(r.Context(), c)
	if err != nil {
		return err
	}
	var rg timeRange
	if c.p.present("start_date") || c.p.present("end_date") {
		var bad string
		if rg, bad = c.p.defaultTimeRange(h.svc.now()); bad != "" {
			return unprocessable("invalid " + bad)
		}
	}
	body, err := h.svc.Projects(r.Context(), u, rg)
	if err != nil {
		return err
	}
	return writeJSON(w, r, c, body, u.ID)
}

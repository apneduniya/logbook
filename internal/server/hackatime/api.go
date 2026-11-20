package hackatime

import (
	"context"
	"fmt"
	"math"
	"net/url"
	"strconv"
	"time"
)

// TrustLevel is Hackatime's trust classification of a user.
type TrustLevel string

// Hackatime's trust levels (User.trust_levels in Hackatime), plus Unknown for
// "not checked yet".
const (
	TrustUnknown TrustLevel = "unknown"
	TrustBlue    TrustLevel = "blue"   // unscored
	TrustRed     TrustLevel = "red"    // convicted (banned)
	TrustGreen   TrustLevel = "green"  // trusted
	TrustYellow  TrustLevel = "yellow" // suspected; only shown to verified confidential apps
)

// trustValues maps each level to Hackatime's numeric enum value.
var trustValues = map[TrustLevel]int{TrustBlue: 0, TrustRed: 1, TrustGreen: 2, TrustYellow: 3}

// Banned reports whether the user is convicted on Hackatime and must not
// submit hours.
func (t TrustLevel) Banned() bool { return t == TrustRed }

// Public masks "suspected" as "unscored", exactly as Hackatime does for the
// user themselves. Show users this, never the raw level.
func (t TrustLevel) Public() TrustLevel {
	if t == TrustYellow {
		return TrustBlue
	}
	return t
}

type trustFactor struct {
	TrustLevel TrustLevel `json:"trust_level"`
	TrustValue *int       `json:"trust_value"`
}

func (f trustFactor) validate() error {
	want, ok := trustValues[f.TrustLevel]
	if !ok {
		return fmt.Errorf("%w: unknown trust level %q", ErrInvalidResponse, f.TrustLevel)
	}
	if f.TrustValue == nil || *f.TrustValue != want {
		return fmt.Errorf("%w: trust value does not match level %q", ErrInvalidResponse, f.TrustLevel)
	}
	return nil
}

// Me is the authenticated Hackatime user, from GET /api/v1/authenticated/me.
// Emails and Slack id are deliberately not kept.
type Me struct {
	ID             int64
	GitHubUsername string
	Trust          TrustLevel
}

type meResponse struct {
	ID             *int64      `json:"id"`
	GitHubUsername *string     `json:"github_username"`
	TrustFactor    trustFactor `json:"trust_factor"`
}

// Me returns the user that owns token (scope "profile").
func (c *Client) Me(ctx context.Context, token string) (Me, error) {
	var r meResponse
	if err := c.getJSON(ctx, "/api/v1/authenticated/me", nil, token, maxSmallBody, &r); err != nil {
		return Me{}, fmt.Errorf("fetching hackatime profile: %w", err)
	}
	if r.ID == nil || *r.ID <= 0 {
		return Me{}, fmt.Errorf("fetching hackatime profile: %w: missing user id", ErrInvalidResponse)
	}
	if err := r.TrustFactor.validate(); err != nil {
		return Me{}, fmt.Errorf("fetching hackatime profile: %w", err)
	}
	me := Me{ID: *r.ID, Trust: r.TrustFactor.TrustLevel}
	if r.GitHubUsername != nil && len(*r.GitHubUsername) <= 39 {
		me.GitHubUsername = *r.GitHubUsername
	}
	return me, nil
}

// TrustFactor reads a user's public trust level by numeric Hackatime id, from
// GET /api/v1/users/:id/trust_factor. It needs no token. Hackatime masks
// "yellow" as "blue" here.
func (c *Client) TrustFactor(ctx context.Context, hackatimeUserID int64) (TrustLevel, error) {
	if hackatimeUserID <= 0 {
		return "", fmt.Errorf("fetching hackatime trust factor: invalid user id %d", hackatimeUserID)
	}
	var r trustFactor
	path := "/api/v1/users/" + strconv.FormatInt(hackatimeUserID, 10) + "/trust_factor"
	if err := c.getJSON(ctx, path, nil, "", maxSmallBody, &r); err != nil {
		return "", fmt.Errorf("fetching hackatime trust factor: %w", err)
	}
	if err := r.validate(); err != nil {
		return "", fmt.Errorf("fetching hackatime trust factor: %w", err)
	}
	return r.TrustLevel, nil
}

// RemoteHeartbeat is one heartbeat as Hackatime renders it (an ActiveRecord
// row). Only the fields Logbook keeps are decoded; ip_address, ja4_id and
// other server-side columns are ignored.
type RemoteHeartbeat struct {
	Time             *float64 `json:"time"`
	Entity           string   `json:"entity"`
	Type             string   `json:"type"`
	Category         string   `json:"category"`
	Project          string   `json:"project"`
	Branch           string   `json:"branch"`
	Language         string   `json:"language"`
	Dependencies     []string `json:"dependencies"`
	Editor           string   `json:"editor"`
	Machine          string   `json:"machine"`
	OperatingSystem  string   `json:"operating_system"`
	UserAgent        string   `json:"user_agent"`
	IsWrite          *bool    `json:"is_write"`
	Lines            *int     `json:"lines"`
	Lineno           *int     `json:"lineno"`
	Cursorpos        *int     `json:"cursorpos"`
	LineAdditions    *int     `json:"line_additions"`
	LineDeletions    *int     `json:"line_deletions"`
	ProjectRootCount *int     `json:"project_root_count"`
	SourceType       string   `json:"source_type"`

	AIModel          string `json:"ai_model"`
	AISession        string `json:"ai_session"`
	AIInputTokens    *int64 `json:"ai_input_tokens"`
	AIOutputTokens   *int64 `json:"ai_output_tokens"`
	AIPromptLength   *int   `json:"ai_prompt_length"`
	AILineChanges    *int   `json:"ai_line_changes"`
	HumanLineChanges *int   `json:"human_line_changes"`
}

type heartbeatsResponse struct {
	Heartbeats *[]RemoteHeartbeat `json:"heartbeats"`
}

// Heartbeats returns the token owner's heartbeats with from <= time <= to,
// from GET /api/v1/my/heartbeats (scope "read"). Hackatime returns the whole
// range in one response with no pagination, so callers keep ranges small and
// split them on ErrResponseTooLarge.
func (c *Client) Heartbeats(ctx context.Context, token string, from, to time.Time) ([]RemoteHeartbeat, error) {
	if !from.Before(to) {
		return nil, fmt.Errorf("fetching hackatime heartbeats: empty range %v..%v", from, to)
	}
	q := url.Values{
		"start_time": {from.UTC().Format(time.RFC3339Nano)},
		"end_time":   {to.UTC().Format(time.RFC3339Nano)},
	}
	var r heartbeatsResponse
	if err := c.getJSON(ctx, "/api/v1/my/heartbeats", q, token, c.maxHBBody, &r); err != nil {
		return nil, fmt.Errorf("fetching hackatime heartbeats: %w", err)
	}
	if r.Heartbeats == nil {
		return nil, fmt.Errorf("fetching hackatime heartbeats: %w: missing heartbeats", ErrInvalidResponse)
	}
	for i, h := range *r.Heartbeats {
		if h.Time == nil || math.IsNaN(*h.Time) || math.IsInf(*h.Time, 0) {
			return nil, fmt.Errorf("fetching hackatime heartbeats: %w: heartbeat %d has no time", ErrInvalidResponse, i)
		}
	}
	return *r.Heartbeats, nil
}

package admin

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/apneduniya/logbook/internal/server/store"
	"github.com/apneduniya/logbook/internal/server/store/db"
)

// UserRow is a user as the admin API reads them, with their Hackatime link.
type UserRow = db.AdminUsersRow

// User loads a user by public id: ErrNotFound when none has it.
func (s *Service) User(ctx context.Context, publicID int64) (UserRow, error) {
	rows, err := s.q.AdminUsers(ctx, []int64{publicID})
	if err != nil {
		return UserRow{}, fmt.Errorf("reading user: %w", err)
	}
	if len(rows) == 0 {
		return UserRow{}, ErrNotFound
	}
	return rows[0], nil
}

// Hackatime's trust levels; anything else stored (unknown) or no link
// reads as blue, Hackatime's default.
var hackatimeLevels = map[string]bool{"blue": true, "red": true, "green": true, "yellow": true}

// trustLevel is the user's unmasked Hackatime trust level and whether it
// was read from a Hackatime link (verified).
func trustLevel(u UserRow) (level string, verified bool) {
	if u.TrustLevel.Valid && hackatimeLevels[u.TrustLevel.String] {
		return u.TrustLevel.String, true
	}
	return "blue", false
}

// adminLevel is Hackatime's admin_level for a Logbook role.
func adminLevel(role string) string {
	if role == RoleAdmin {
		return "admin"
	}
	return "default"
}

// displayName is Hackatime's display_name: the display name, else the
// username.
func displayName(u UserRow) string {
	if u.DisplayName != "" {
		return u.DisplayName
	}
	return u.Username
}

func optText(t pgtype.Text) *string {
	if !t.Valid || t.String == "" {
		return nil
	}
	return &t.String
}

func optInt8(v pgtype.Int8) *int64 {
	if !v.Valid {
		return nil
	}
	return &v.Int64
}

func nullStr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// railsTime formats t as Rails renders a time in JSON:
// "2024-03-20T00:00:00.000Z".
func railsTime(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000Z07:00") }

// unixSeconds is t as Hackatime's float heartbeat time.
func unixSeconds(t time.Time) float64 { return float64(t.UnixMicro()) / 1e6 }

// InfoStats is user/info's stats.
type InfoStats struct {
	TotalHeartbeats  int64 `json:"total_heartbeats"`
	TotalCodingTime  int64 `json:"total_coding_time"`
	LanguagesUsed    int64 `json:"languages_used"`
	ProjectsWorkedOn int64 `json:"projects_worked_on"`
	DaysActive       int64 `json:"days_active"`
}

// InfoExtra is user/info's logbook object.
type InfoExtra struct {
	Username string `json:"username"`
	// HackatimeID is the linked Hackatime user id, null without a link.
	HackatimeID *int64 `json:"hackatime_id"`
	// TrustVerified is false when trust_level is a stand-in (no link, or
	// the level couldn't be read); HackatimeTrustLevel is what is stored
	// ("unknown" then), null without a link.
	TrustVerified       bool       `json:"trust_verified"`
	HackatimeTrustLevel *string    `json:"hackatime_trust_level"`
	TrustCheckedAt      *time.Time `json:"trust_checked_at"`
	Role                string     `json:"role"`
	ProfilePublic       bool       `json:"profile_public"`
	Devices             struct {
		Active  int64 `json:"active"`
		Revoked int64 `json:"revoked"`
	} `json:"devices"`
	// LastCreditedMinute is the newest minute with credit, null without one.
	LastCreditedMinute *time.Time `json:"last_credited_minute"`
}

// Info is user/info's user (user_info in user_utilities.rb). Logbook has
// no Slack, country or admin levels beyond its role: those are null.
type Info struct {
	ID              int64     `json:"id"`
	Username        string    `json:"username"`
	DisplayName     string    `json:"display_name"`
	SlackUID        *string   `json:"slack_uid"`
	SlackUsername   *string   `json:"slack_username"`
	GitHubUsername  *string   `json:"github_username"`
	Timezone        string    `json:"timezone"`
	CountryCode     *string   `json:"country_code"`
	AdminLevel      string    `json:"admin_level"`
	TrustLevel      string    `json:"trust_level"`
	Suspected       bool      `json:"suspected"`
	Banned          bool      `json:"banned"`
	CreatedAt       string    `json:"created_at"`
	UpdatedAt       string    `json:"updated_at"`
	LastHeartbeatAt *float64  `json:"last_heartbeat_at"`
	EmailAddresses  []string  `json:"email_addresses"`
	APIKeysCount    int64     `json:"api_keys_count"`
	Stats           InfoStats `json:"stats"`
	Logbook         InfoExtra `json:"logbook"`
}

// Info answers user/info for u.
func (s *Service) Info(ctx context.Context, u UserRow) (Info, error) {
	st, err := s.q.AdminHeartbeatStats(ctx, u.ID)
	if err != nil {
		return Info{}, fmt.Errorf("reading heartbeat stats: %w", err)
	}
	n, err := s.q.AdminUserCounts(ctx, u.ID)
	if err != nil {
		return Info{}, fmt.Errorf("reading user counts: %w", err)
	}
	level, verified := trustLevel(u)
	emails := []string{}
	if u.Email != "" {
		emails = append(emails, u.Email)
	}
	info := Info{
		ID: u.PublicID, Username: u.Username, DisplayName: displayName(u), GitHubUsername: optText(u.HackatimeUsername),
		Timezone: u.Timezone, AdminLevel: adminLevel(u.Role), TrustLevel: level,
		Suspected: level == "yellow", Banned: level == "red",
		CreatedAt: railsTime(u.CreatedAt), UpdatedAt: railsTime(u.UpdatedAt),
		EmailAddresses: emails, APIKeysCount: n.ApiKeys,
		Stats: InfoStats{
			TotalHeartbeats: st.TotalHeartbeats, TotalCodingTime: int64(st.TotalCodingTime),
			LanguagesUsed: st.LanguagesUsed, ProjectsWorkedOn: st.ProjectsWorkedOn, DaysActive: st.DaysActive,
		},
		Logbook: InfoExtra{
			Username: u.Username, HackatimeID: optInt8(u.HackatimeUserID), TrustVerified: verified,
			HackatimeTrustLevel: optText(u.TrustLevel), TrustCheckedAt: utcPtr(u.TrustCheckedAt),
			Role: u.Role, ProfilePublic: u.ProfilePublic,
		},
	}
	if st.LastHeartbeatAt >= 0 {
		lht := math.Round(st.LastHeartbeatAt*1e6) / 1e6
		info.LastHeartbeatAt = &lht
	}
	info.Logbook.Devices.Active, info.Logbook.Devices.Revoked = n.ActiveDevices, n.RevokedDevices
	if n.LastCreditedMinute.Unix() > 0 {
		t := n.LastCreditedMinute.UTC()
		info.Logbook.LastCreditedMinute = &t
	}
	return info, nil
}

func utcPtr(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	u := t.UTC()
	return &u
}

func (h *Handler) userInfo(w http.ResponseWriter, r *http.Request, c *call) error {
	u, err := h.user(r.Context(), c)
	if err != nil {
		return err
	}
	info, err := h.svc.Info(r.Context(), u)
	if err != nil {
		return err
	}
	return writeJSON(w, r, c, map[string]any{"user": info}, u.ID)
}

// BatchUser is one user of user/info_batch: the columns and methods
// Hackatime's as_json picks, in its order, plus a logbook object.
type BatchUser struct {
	ID              int64      `json:"id"`
	Username        string     `json:"username"`
	SlackUID        *string    `json:"slack_uid"`
	SlackUsername   *string    `json:"slack_username"`
	GitHubUsername  *string    `json:"github_username"`
	Timezone        string     `json:"timezone"`
	CountryCode     *string    `json:"country_code"`
	TrustLevel      string     `json:"trust_level"`
	SlackAvatarURL  *string    `json:"slack_avatar_url"`
	GitHubAvatarURL *string    `json:"github_avatar_url"`
	DisplayName     string     `json:"display_name"`
	AvatarURL       *string    `json:"avatar_url"`
	Logbook         BatchExtra `json:"logbook"`
	userID          uuid.UUID  // the audit subject
}

// BatchExtra is a batch user's logbook object.
type BatchExtra struct {
	Username      string `json:"username"`
	HackatimeID   *int64 `json:"hackatime_id"`
	TrustVerified bool   `json:"trust_verified"`
}

// maxBatch is how many ids user/info_batch reads.
const maxBatch = 2000

// parseBatchIDs is params[:ids].to_s.split(",").map(&:strip).map(&:to_i)
// .uniq.take(2000).
func parseBatchIDs(raw string) []int64 {
	parts := strings.Split(raw, ",")
	for len(parts) > 0 && parts[len(parts)-1] == "" {
		parts = parts[:len(parts)-1] // Ruby's split drops trailing empty strings
	}
	seen := map[int64]bool{}
	var ids []int64
	for _, p := range parts {
		id := toI(strings.TrimSpace(p))
		if seen[id] {
			continue
		}
		seen[id] = true
		ids = append(ids, id)
		if len(ids) == maxBatch {
			break
		}
	}
	return ids
}

// InfoBatch answers user/info_batch: the users with those public ids, by
// public id.
func (s *Service) InfoBatch(ctx context.Context, ids []int64) ([]BatchUser, error) {
	rows, err := s.q.AdminUsers(ctx, ids)
	if err != nil {
		return nil, fmt.Errorf("reading users: %w", err)
	}
	out := make([]BatchUser, len(rows))
	for i, u := range rows {
		level, verified := trustLevel(u)
		out[i] = BatchUser{
			ID: u.PublicID, Username: u.Username, GitHubUsername: optText(u.HackatimeUsername),
			Timezone: u.Timezone, TrustLevel: level, DisplayName: displayName(u), AvatarURL: nullStr(u.AvatarUrl),
			Logbook: BatchExtra{Username: u.Username, HackatimeID: optInt8(u.HackatimeUserID), TrustVerified: verified},
			userID:  u.ID,
		}
	}
	return out, nil
}

func (h *Handler) userInfoBatch(w http.ResponseWriter, r *http.Request, c *call) error {
	if !c.p.present("ids") {
		return unprocessable("ids parameter required")
	}
	ids := parseBatchIDs(c.p["ids"])
	if len(ids) == 0 {
		return unprocessable("no valid ids provided")
	}
	users, err := h.svc.InfoBatch(r.Context(), ids)
	if err != nil {
		return err
	}
	subjects := make([]uuid.UUID, len(users))
	for i, u := range users {
		subjects[i] = u.userID
	}
	return writeJSON(w, r, c, map[string]any{"users": users}, subjects...)
}

func (h *Handler) userByEmail(w http.ResponseWriter, r *http.Request, c *call) error {
	if !c.p.present("email") {
		return unprocessable("bro dont have a email")
	}
	u, err := h.svc.q.AdminUserByEmail(r.Context(), c.p["email"])
	if store.IsNotFound(err) {
		return notFound("email not found")
	}
	if err != nil {
		return fmt.Errorf("looking up email: %w", err)
	}
	return writeJSON(w, r, c, map[string]any{"user_id": u.PublicID}, u.ID)
}

// SearchUser is one user/search_fuzzy result. Logbook has no Slack and no
// avatars from GitHub or Slack: those are null.
type SearchUser struct {
	ID              int64     `json:"id"`
	Username        string    `json:"username"`
	SlackUsername   *string   `json:"slack_username"`
	GitHubUsername  *string   `json:"github_username"`
	SlackAvatarURL  *string   `json:"slack_avatar_url"`
	GitHubAvatarURL *string   `json:"github_avatar_url"`
	Email           string    `json:"email"`
	RankScore       int64     `json:"rank_score"`
	userID          uuid.UUID // the audit subject
}

// likeEscape is Rails' sanitize_sql_like: \, % and _ escaped with \.
func likeEscape(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s)
}

// Search answers user/search_fuzzy (User.fuzzy_ranked_search with limit
// 10, then the email gate): see the AdminSearchUsers query.
func (s *Service) Search(ctx context.Context, term string) ([]SearchUser, error) {
	term = strings.TrimSpace(term)
	if term == "" {
		return []SearchUser{}, nil
	}
	esc := likeEscape(term)
	arg := db.AdminSearchUsersParams{Exact: term, IlikeExact: esc, Prefix: esc + "%", Contains: "%" + esc + "%"}
	if isDigits(term) {
		if n, ok := leadingInt(term); ok && n != math.MaxInt64 {
			arg.NumericID = pgtype.Int8{Int64: n, Valid: true}
		}
	}
	rows, err := s.q.AdminSearchUsers(ctx, arg)
	if err != nil {
		return nil, fmt.Errorf("searching users: %w", err)
	}
	out := make([]SearchUser, 0, len(rows))
	for _, u := range rows {
		if u.Email == "" {
			continue // Hackatime lists only users with an email
		}
		out = append(out, SearchUser{
			ID: u.PublicID, Username: u.Username, GitHubUsername: nullStr(u.GithubUsername),
			Email: u.Email, RankScore: u.RankScore, userID: u.ID,
		})
	}
	return out, nil
}

func (h *Handler) searchFuzzy(w http.ResponseWriter, r *http.Request, c *call) error {
	if !c.p.present("query") {
		return unprocessable("bro dont have a query")
	}
	users, err := h.svc.Search(r.Context(), c.p["query"])
	if err != nil {
		return err
	}
	subjects := make([]uuid.UUID, len(users))
	for i, u := range users {
		subjects[i] = u.userID
	}
	return writeJSON(w, r, c, map[string]any{"users": users}, subjects...)
}

package ingest

import (
	"fmt"
	"math"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/apneduniya/logbook/internal/server/ingest/useragent"
	"github.com/apneduniya/logbook/pkg/wire"
)

// maxDependencyLen bounds each dependency name; wire bounds only their count.
const maxDependencyLen = 256

// maxHeaderValue matches wire's limit on short string fields.
const maxHeaderValue = 256

// Placeholders some WakaTime clients send instead of a real value ("use
// whatever I sent last"). Hackatime stores them literally and filters them
// out of reports. We store "" instead, so the time rolls up under Unknown
// (project) or Other (language) and no projects row named after a
// placeholder is ever created. Resolving them to the user's previous value
// would make a heartbeat's stored fields, and so its dedup hash, depend on
// what else arrived first.
var placeholders = map[string]bool{
	"<<LAST_PROJECT>>":  true,
	"<<LAST_LANGUAGE>>": true,
	"<<LAST_BRANCH>>":   true,
}

// Client describes the request a batch arrived in: the values wakatime-cli
// sends in headers rather than in each heartbeat body. Callers that have no
// request (the Hackatime importer) pass the zero value.
//
// Every heartbeat, whichever path it arrives by, is attributed the same way
// (Hackatime's precedence): user_agent is the body's user_agent, else the
// body's plugin, else Client.UserAgent; the editor and OS that user agent
// names override the body's editor and operating_system; its plugin token
// fills an empty plugin; a non-empty Client.Machine overrides the body's
// machine.
type Client struct {
	// UserAgent is the request's User-Agent header.
	UserAgent string
	// Machine is the decoded X-Machine-Name header.
	Machine string
}

// normalize cleans a heartbeat before it is hashed and stored: it applies
// the client attribution rules, removes NUL bytes, strips control characters
// from the project, clears placeholders and fills defaults. It never
// rejects; validate does that.
func normalize(h wire.Heartbeat, c Client) wire.Heartbeat {
	h = attribute(h, c)
	for _, p := range []*string{
		&h.Entity, &h.Type, &h.Category, &h.Project, &h.Branch, &h.Language, &h.Editor,
		&h.Plugin, &h.Machine, &h.OperatingSystem, &h.UserAgent, &h.AIModel, &h.AISession,
	} {
		*p = strings.ReplaceAll(*p, "\x00", "")
	}
	if len(h.Dependencies) > 0 {
		deps := make([]string, len(h.Dependencies))
		for i, d := range h.Dependencies {
			deps[i] = strings.ReplaceAll(d, "\x00", "")
		}
		h.Dependencies = deps
	}
	h.Project = strings.TrimSpace(strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, h.Project))
	for _, p := range []*string{&h.Project, &h.Language, &h.Branch} {
		if placeholders[*p] {
			*p = ""
		}
	}
	if strings.TrimSpace(h.Category) == "" {
		h.Category = "coding"
	}
	if strings.TrimSpace(h.Type) == "" {
		h.Type = "file"
	}
	return h
}

// attribute applies the precedence documented on Client.
func attribute(h wire.Heartbeat, c Client) wire.Heartbeat {
	if h.UserAgent == "" {
		h.UserAgent = h.Plugin
	}
	if h.UserAgent == "" {
		h.UserAgent = truncate(c.UserAgent)
	}
	parsed := useragent.Parse(h.UserAgent)
	if parsed.Editor != "" {
		h.Editor = parsed.Editor
	}
	if parsed.OS != "" {
		h.OperatingSystem = parsed.OS
	}
	if h.Plugin == "" {
		h.Plugin = parsed.Plugin
	}
	if c.Machine != "" {
		h.Machine = truncate(c.Machine)
	}
	return h
}

// truncate cuts a header-derived value to maxHeaderValue bytes on a rune
// boundary, so a long header never makes an otherwise valid heartbeat fail
// validation. Body fields are not truncated; validate rejects them instead.
func truncate(s string) string {
	if len(s) <= maxHeaderValue {
		return s
	}
	s = s[:maxHeaderValue]
	for !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s
}

// validate applies wire.Validate plus the limits our columns need.
func validate(h wire.Heartbeat, now time.Time) error {
	if err := h.Validate(now); err != nil {
		return err
	}
	type intField struct {
		name string
		v    *int
	}
	counts := []intField{
		{"lines", h.Lines}, {"lineno", h.Lineno}, {"cursorpos", h.Cursorpos},
		{"line_additions", h.LineAdditions}, {"line_deletions", h.LineDeletions},
		{"project_root_count", h.ProjectRootCount}, {"ai_prompt_length", h.AIPromptLength},
	}
	for _, f := range counts {
		if f.v != nil && (*f.v < 0 || *f.v > math.MaxInt32) {
			return fmt.Errorf("%w: %s must be between 0 and %d", wire.ErrInvalidHeartbeat, f.name, math.MaxInt32)
		}
	}
	// Line changes are net deltas: wakatime-cli reports a negative value when
	// an edit removes more lines than it adds. Only the column range applies.
	deltas := []intField{{"ai_line_changes", h.AILineChanges}, {"human_line_changes", h.HumanLineChanges}}
	for _, f := range deltas {
		if f.v != nil && (*f.v < math.MinInt32 || *f.v > math.MaxInt32) {
			return fmt.Errorf("%w: %s must be between %d and %d", wire.ErrInvalidHeartbeat, f.name, math.MinInt32, math.MaxInt32)
		}
	}
	if (h.AIInputTokens != nil && *h.AIInputTokens < 0) || (h.AIOutputTokens != nil && *h.AIOutputTokens < 0) {
		return fmt.Errorf("%w: ai token counts must not be negative", wire.ErrInvalidHeartbeat)
	}
	for _, d := range h.Dependencies {
		if len(d) > maxDependencyLen {
			return fmt.Errorf("%w: dependency longer than %d bytes", wire.ErrInvalidHeartbeat, maxDependencyLen)
		}
	}
	return nil
}

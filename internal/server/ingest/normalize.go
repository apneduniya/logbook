package ingest

import (
	"fmt"
	"math"
	"strings"
	"time"
	"unicode"

	"github.com/apneduniya/logbook/pkg/wire"
)

// maxDependencyLen bounds each dependency name; wire bounds only their count.
const maxDependencyLen = 256

// normalize cleans a heartbeat before it is hashed and stored: it removes NUL bytes,
// strips control characters from the project and fills defaults. It never
// rejects; validate does that.
func normalize(h wire.Heartbeat) wire.Heartbeat {
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
	if strings.TrimSpace(h.Category) == "" {
		h.Category = "coding"
	}
	if strings.TrimSpace(h.Type) == "" {
		h.Type = "file"
	}
	return h
}

// validate applies wire.Validate plus the limits our columns need.
func validate(h wire.Heartbeat, now time.Time) error {
	if err := h.Validate(now); err != nil {
		return err
	}
	ints := []struct {
		name string
		v    *int
	}{
		{"lines", h.Lines}, {"lineno", h.Lineno}, {"cursorpos", h.Cursorpos},
		{"line_additions", h.LineAdditions}, {"line_deletions", h.LineDeletions},
		{"project_root_count", h.ProjectRootCount}, {"ai_prompt_length", h.AIPromptLength},
		{"ai_line_changes", h.AILineChanges}, {"human_line_changes", h.HumanLineChanges},
	}
	for _, f := range ints {
		if f.v != nil && (*f.v < 0 || *f.v > math.MaxInt32) {
			return fmt.Errorf("%w: %s must be between 0 and %d", wire.ErrInvalidHeartbeat, f.name, math.MaxInt32)
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

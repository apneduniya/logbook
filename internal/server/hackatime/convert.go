package hackatime

import (
	"time"

	"github.com/apneduniya/logbook/pkg/wire"
)

// toWire converts a Hackatime heartbeat into Logbook's wire format, keeping
// every AI field. It reports false for heartbeats that must not be imported:
// Hackatime test entries, heartbeats outside the requested window (Hackatime
// returned something we didn't ask for) and anything that fails validation.
func toWire(h RemoteHeartbeat, from, to, now time.Time) (wire.Heartbeat, bool) {
	if h.SourceType == "test_entry" || h.Time == nil {
		return wire.Heartbeat{}, false
	}
	w := wire.Heartbeat{
		Entity:           h.Entity,
		Type:             h.Type,
		Category:         h.Category,
		Time:             *h.Time,
		Project:          h.Project,
		ProjectRootCount: h.ProjectRootCount,
		Branch:           h.Branch,
		Language:         h.Language,
		Dependencies:     h.Dependencies,
		Lines:            h.Lines,
		LineAdditions:    h.LineAdditions,
		LineDeletions:    h.LineDeletions,
		Lineno:           h.Lineno,
		Cursorpos:        h.Cursorpos,
		IsWrite:          h.IsWrite != nil && *h.IsWrite,
		Editor:           h.Editor,
		Machine:          h.Machine,
		OperatingSystem:  h.OperatingSystem,
		UserAgent:        h.UserAgent,
		AIModel:          h.AIModel,
		AISession:        h.AISession,
		AIInputTokens:    h.AIInputTokens,
		AIOutputTokens:   h.AIOutputTokens,
		AIPromptLength:   h.AIPromptLength,
		AILineChanges:    h.AILineChanges,
		HumanLineChanges: h.HumanLineChanges,
	}
	ts := w.Timestamp()
	// Allow a second of slack for float rounding at the window edges.
	if ts.Before(from.Add(-time.Second)) || ts.After(to.Add(time.Second)) {
		return wire.Heartbeat{}, false
	}
	if w.Validate(now) != nil {
		return wire.Heartbeat{}, false
	}
	return w, true
}

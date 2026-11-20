package struggle

import (
	"time"

	"github.com/apneduniya/logbook/pkg/wire"
)

// RulesVersion identifies the rule set. Bump it on any change to labelling,
// cuts, flags or thresholds.
const RulesVersion = "v1"

// Label is a minute's classification.
type Label string

// Minute labels. Only LabelYou earns credit.
const (
	LabelYou   Label = "you"
	LabelAgent Label = "agent"
	LabelIdle  Label = "idle"
)

// Cut codes: rules that remove credit. See docs/STRUGGLE-RULES.md.
const (
	CutIdle             = "idle"
	CutAgentAlone       = "agent_alone"
	CutRubberStamp      = "rubber_stamp"
	CutLoop             = "loop"
	CutBackgroundEditor = "background_editor"
)

// Flag codes: observations for reviewers that don't change credit.
const (
	FlagScriptedInput    = "scripted_input"
	FlagAutomatedSession = "automated_session"
	FlagOTelMismatch     = "otel_mismatch"
	FlagLongDay          = "long_day"
	FlagLowSignal        = "low_signal"
	FlagMultiDevice      = "multi_device" // set by the server only
)

// Input is everything the engine looks at for one device. Slices need not be
// sorted; the engine sorts copies. Window bounds which minutes are emitted;
// events outside it are context only (e.g. "input in the last 3 minutes").
//
// A minute's result is final, and the same however the window is chosen, once
// Input holds every event from ContextBefore before the minute to Lookahead
// after it (ADR 0012). The agent meets this by computing the last 60 minutes
// from 90 minutes of events: its rewrites of a minute made when the minute is
// 25 to 30 minutes old are final. The server re-running closed history passes
// events from ContextBefore before Window.From to 30 minutes after Window.To,
// and sets DataEnd.
type Input struct {
	Window Window
	// DataEnd, if set, says the events are complete up to this time even if
	// the latest of them is earlier: an agent turn whose last event is more
	// than 10 minutes before DataEnd has ended. Zero means the later of
	// Window.To and the latest event. Added in v1 for server re-runs over
	// closed history; the agent leaves it zero.
	DataEnd    time.Time
	Activity   []wire.ActivitySample
	Heartbeats []Heartbeat
	AI         []wire.AIEvent
	ActiveTime []ActiveTime
	// Repos maps a working directory or file path prefix to a project name, so
	// AI events (which know only cwd) and heartbeats (which know project) agree.
	Repos []Repo
	// TZ is the user's time zone, for per-day flags such as long_day.
	TZ *time.Location
}

// Window is a half-open time range [From, To), truncated to whole minutes by the engine.
type Window struct {
	From, To time.Time
}

// Heartbeat is the part of an editor heartbeat the engine uses.
type Heartbeat struct {
	Time      time.Time
	Project   string
	Entity    string // file path; used only to match repos, never emitted
	IsWrite   bool
	Lineno    *int
	Cursorpos *int
	// AI is true when the editor attributed the change to an AI tool
	// (category "ai coding" or ai_* fields present).
	AI bool
}

// ActiveTime is one Claude Code OpenTelemetry active-time datapoint
// (claude_code.active_time.total), converted to a delta over [From, To).
type ActiveTime struct {
	From, To   time.Time
	SessionID  string
	UserSecs   float64
	CLISecs    float64
	Harness    wire.Harness
	ProjectCwd string
}

// Repo maps a path prefix to a project name.
type Repo struct {
	Path    string
	Project string
}

// Minute is the engine's output for one (minute, project) pair. Idle minutes
// with no project are not emitted: a minute missing from a Result is idle.
type Minute struct {
	Minute  time.Time // UTC, truncated to the minute
	Project string    // "" when the minute has no project (idle with nothing open)
	Label   Label
	// Seconds credited, 0..60. Across all projects of one minute the total is ≤ 60.
	Credited int
	// Cuts lists the rules that removed credit from this minute, sorted.
	Cuts []string
	// Flags lists reviewer observations for this minute, sorted.
	Flags []string
}

// Result is the engine's full output for a window.
type Result struct {
	RulesVersion string
	Minutes      []Minute
}

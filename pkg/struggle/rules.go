package struggle

import "time"

// Finality contract (ADR 0012). A minute's label, credit, cuts and
// window-independent flags depend only on events from ContextBefore before
// the minute's start to Lookahead after it, so any computation that has
// those events gives the minute its final value.
const (
	// ContextBefore is how much history before a minute the rules read.
	ContextBefore = 60 * time.Minute
	// Lookahead is how far after a minute's start the rules read. It is
	// less than the 30-minute finality age (ADR 0010), so the agent's last
	// rewrite of a minute, made 25 to 30 minutes after it, is final.
	Lookahead = 25 * time.Minute
)

// Thresholds of rules v1. docs/STRUGGLE-RULES.md explains each one; changing
// any of them changes credited time, so it needs a new RulesVersion.
const (
	// graceMinutes is how many minutes after the last input a person still
	// counts as engaged (thinking and reading time).
	graceMinutes = 3
	// activeSessionWindow is how recent one of the person's own signals (a
	// human heartbeat, a prompt, a permission answer) must be for browser or
	// unknown-focus time to count, and how far back a minute looks for a
	// project.
	activeSessionWindow = 15 * time.Minute
	// turnTimeout ends a turn that has had no events for this long.
	turnTimeout = 10 * time.Minute
	// projectLookback is how far back a minute looks for its project.
	projectLookback = 5 * time.Minute
	// lowSignalRadius: a minute with no activity sample within this distance
	// has no working idle source and falls back to heartbeats.
	lowSignalRadius = 2 * time.Minute
	// backgroundEditorMinutes: on a low-signal minute, heartbeats with no write
	// or cursor movement in this many minutes (the minute included) are cut.
	backgroundEditorMinutes = 5

	// rubber_stamp: a reply within rubberStampGap of turn_end, at most
	// rubberStampMaxChars long; three in a row make a streak.
	rubberStampGap      = 15 * time.Second
	rubberStampMaxChars = 25

	loopRepeats = 3
	loopWindow  = 30 * time.Minute

	// automatedLookback: a prompt with no input in this long before it was
	// not started by a person.
	automatedLookback = 2 * time.Minute
	// automatedTolerance allows for the Enter key being read by the sampler
	// slightly after the hook stamped the prompt.
	automatedTolerance = 2 * time.Second

	// scriptedRun is the number of consecutive intervals checked for
	// regularity; scriptedMaxStdDev is the standard deviation (seconds) under
	// which they are too regular for a human.
	scriptedRun       = 30
	scriptedMaxStdDev = 0.2
	// Heartbeat intervals outside [scriptedHBShortest, scriptedHBLongest] break a run.
	// WakaTime plugins throttle to one heartbeat per file every 2 minutes, so
	// steady typing yields near-identical ~120 s intervals; those are excluded.
	scriptedHBShortest = 1 * time.Second
	scriptedHBLongest  = 115 * time.Second
	// An OS input instant takes part in the activity regularity check only
	// when a later sample saw at least scriptedQuietIdle of quiet after it
	// (continuous typing is seen at the sampler's own rhythm and is skipped).
	scriptedQuietIdle     = 2 * time.Second
	scriptedInputShortest = 2 * time.Second
	scriptedInputLongest  = 10 * time.Minute
	// instantMergeGap merges input instants that are the same input observed
	// by several samples.
	instantMergeGap = 500 * time.Millisecond

	// otel_mismatch: Claude Code's active time (user + cli) is below
	// otelMaxRatio of the credited seconds and short by more than otelMinGap.
	otelMaxRatio = 0.5
	otelMinGap   = 10 * time.Minute

	longDay = 12 * time.Hour

	// lookaheadClip: a rule that decides an earlier minute from a later
	// event (the rubber-stamp streak's second stamp, a scripted-input run)
	// reaches back at most this far from that event, so the event is less
	// than Lookahead after the start of every minute it changes.
	lookaheadClip = Lookahead - time.Minute

	// turnRuleReach: a cut or flag decided at a turn's prompt (loop,
	// rubber_stamp, automated_session) covers the turn's minutes up to this
	// long after the prompt. With the 30-minute loop and streak windows this
	// keeps every rule within ContextBefore of the minute it decides.
	turnRuleReach = 25 * time.Minute
	// rubberStampStreakWindow: three stamps make a streak only when the
	// first and third prompts are at most this far apart.
	rubberStampStreakWindow = 30 * time.Minute
	// positionMemory: a heartbeat's line or cursor is compared with the last
	// human heartbeat on the same file within this long.
	positionMemory = 30 * time.Minute
	// presentIdle: a sample with OS idle time below this makes its minute
	// present.
	presentIdle = 60 * time.Second

	// MaxWindow caps the window Compute evaluates, bounding memory and time.
	// A longer window is cut to its first MaxWindow.
	MaxWindow = 7 * 24 * time.Hour

	// maxIdleSeconds caps an idle reading so durations can't overflow.
	maxIdleSeconds = 1e7
)

// Timeline margins, in minutes: how much context before Window.From and
// lookahead after Window.To the per-minute arrays hold.
const (
	marginBefore = 16 // active-session window (15 min) plus one
	marginAfter  = 3  // low-signal radius (2 min) plus one
)

package struggle

import "time"

// Thresholds of rules v1. docs/STRUGGLE-RULES.md explains each one; changing
// any of them changes credited time, so it needs a new RulesVersion.
const (
	// graceMinutes is how many minutes after the last input a person still
	// counts as engaged (thinking and reading time).
	graceMinutes = 3
	// activeSessionWindow is how recent an editor heartbeat or AI event must
	// be for browser time to count.
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

	rubberStampGap      = 15 * time.Second
	rubberStampMaxChars = 25
	rubberStampStreak   = 3

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

	// otel_mismatch: Claude Code's user time is below otelMaxRatio of the
	// credited seconds and short by more than otelMinGap.
	otelMaxRatio = 0.5
	otelMinGap   = 10 * time.Minute

	longDay = 12 * time.Hour

	// finalityHorizon is the age at which an uploaded minute becomes final.
	// No rule looks further ahead than this, so the agent's final minutes and
	// a server re-run over the full day agree.
	finalityHorizon = 30 * time.Minute

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

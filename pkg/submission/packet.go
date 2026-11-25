package submission

import (
	"cmp"
	"fmt"
	"maps"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/apneduniya/logbook/pkg/evidence"
	"github.com/apneduniya/logbook/pkg/projectname"
	"github.com/apneduniya/logbook/pkg/struggle"
)

// Packet is the review packet (ADR 0020, docs/api/submissions.md), built
// once per verification and stored without the link. Trust.Now,
// Trust.CheckedAt, Justification and RawRowsDeletedAfter are filled in
// when it is served.
type Packet struct {
	// RulesVersion is the frozen rows' rules version; several are joined
	// with ", ".
	RulesVersion string `json:"rules_version"`
	// CountedSeconds is the sum of the frozen capped seconds.
	CountedSeconds int64 `json:"counted_seconds"`
	Trust          Trust `json:"trust"`
	// RawSeconds and RawAISeconds are time at the keyboard for the project
	// and days, counted as /api/v1/me/summary counts it, and its part
	// attributed to AI (category "ai coding").
	RawSeconds   int64 `json:"raw_seconds"`
	RawAISeconds int64 `json:"raw_ai_seconds"`
	// OneThirdRuleSeconds is raw - raw_ai + raw_ai / 3, rounded down.
	OneThirdRuleSeconds int64          `json:"one_third_rule_seconds"`
	Minutes             Counts         `json:"minutes"`
	Devices             []PacketDevice `json:"devices"`
	MadeOf              MadeOf         `json:"made_of"`
	Cuts                []CutSummary   `json:"cuts"`
	Flags               []FlagSummary  `json:"flags"`
	Days                []PacketDay    `json:"days"`
	AISessions          []AISession    `json:"ai_sessions"`
	// Mismatches lists at most MaxMismatches mismatched rows, newest
	// first.
	Mismatches []PacketMismatch `json:"mismatches"`
	// Justification is Justification(packet, link), set when served.
	Justification string `json:"justification,omitempty"`
	// RawRowsDeletedAfter is the day the revealed rows are deleted, set
	// when served.
	RawRowsDeletedAfter string `json:"raw_rows_deleted_after,omitempty"`
}

// Trust is the submitter's Hackatime trust level, masked as Hackatime
// masks it publicly. Only AtSubmission and HackatimeLinked (whether
// Hackatime was linked at submission) are stored; Now, CheckedAt and
// HackatimeLinkedNow are set when the packet is served.
type Trust struct {
	AtSubmission       string     `json:"at_submission"`
	Now                string     `json:"now,omitempty"`
	CheckedAt          *time.Time `json:"checked_at,omitempty"`
	HackatimeLinked    bool       `json:"hackatime_linked"`
	HackatimeLinkedNow *bool      `json:"hackatime_linked_now,omitempty"`
}

// Counts counts the submitted minutes by result. A minute counts once,
// whatever its rows: mismatch if any of its rows mismatches, else
// unbacked if any is unbacked, else verified. Minutes of every label
// count, so cut and idle minutes too.
type Counts struct {
	Verified int `json:"verified"`
	Mismatch int `json:"mismatch"`
	Unbacked int `json:"unbacked"`
}

// PacketDevice is a device with frozen rows. Gaps are the days of the
// range, from the device's first sealed day on, that it never sealed (the
// agent was off).
type PacketDevice struct {
	Name string   `json:"name"`
	OS   string   `json:"os"`
	Gaps []string `json:"gaps"`
}

// MadeOf splits the counted seconds by what the person was doing (display
// only; it never changes credit).
type MadeOf struct {
	Editing int64 `json:"editing"`
	WithAI  int64 `json:"with_ai"`
	Other   int64 `json:"other"`
}

// CutSummary is one cut code: the minutes it zeroed (as seconds) and
// their ranges, with the rule in plain words.
type CutSummary struct {
	Code    string         `json:"code"`
	Rule    string         `json:"rule"`
	Seconds int64          `json:"seconds"`
	Ranges  [][2]time.Time `json:"ranges"`
}

// FlagSummary is one flag code: the minutes it marks and their ranges,
// with the rule in plain words.
type FlagSummary struct {
	Code    string         `json:"code"`
	Rule    string         `json:"rule"`
	Minutes int            `json:"minutes"`
	Ranges  [][2]time.Time `json:"ranges"`
}

// PacketDay is one UTC day of the range.
type PacketDay struct {
	Day string `json:"day"`
	// Strip has one character per minute of the day (StripCounted and the
	// other Strip* constants).
	Strip          string   `json:"strip"`
	CountedSeconds int64    `json:"counted_seconds"`
	RawSeconds     int64    `json:"raw_seconds"`
	Commits        []Commit `json:"commits"`
}

// Strip characters, one per minute (docs/api/submissions.md).
const (
	StripCounted    = 'y' // counted
	StripAgentAlone = 'a' // the AI ran alone
	StripCut        = 'c' // a counted label with no credit: cut
	StripIdle       = 'i' // idle or nothing
	StripUnbacked   = 'u' // counted, but unbacked
	StripMismatch   = 'm' // the replay disagrees
)

// Commit is one commit of the project from the revealed git rows.
type Commit struct {
	At      time.Time `json:"at"`
	Hash    string    `json:"hash"` // the first 7 characters
	Files   int       `json:"files"`
	Added   int       `json:"added"`
	Removed int       `json:"removed"`
	// URL links the commit on GitHub when the project's repo_url is a
	// GitHub repository.
	URL string `json:"url,omitempty"`
}

// AISession is one AI session of the project, from its revealed events.
// Its session id is never shown.
type AISession struct {
	Harness string    `json:"harness"`
	Model   string    `json:"model"`
	From    time.Time `json:"from"`
	To      time.Time `json:"to"`
	// Prompts counts prompt_submit events, Turns turn_end events.
	Prompts int `json:"prompts"`
	Turns   int `json:"turns"`
	// CountedSeconds are the counted seconds of the session's minutes,
	// AgentAloneSeconds 60 per minute of it labelled agent.
	CountedSeconds    int64 `json:"counted_seconds"`
	AgentAloneSeconds int64 `json:"agent_alone_seconds"`
}

// PacketMismatch is one mismatched row: the device's frozen value and the
// server's replay.
type PacketMismatch struct {
	Minute time.Time `json:"minute"`
	Device Credit    `json:"device"`
	Server Credit    `json:"server"`
}

// CutRules and FlagRules say each code's rule in plain words
// (docs/STRUGGLE-RULES.md).
var (
	CutRules = map[string]string{
		struggle.CutIdle:             "Nobody was at the keyboard and no AI agent was working",
		struggle.CutAgentAlone:       "The AI agent worked while the person was away",
		struggle.CutRubberStamp:      "Three or more short approvals in a row, each within 15 s of the agent finishing, with no editing between them",
		struggle.CutLoop:             "The same prompt sent three or more times within 30 minutes with no editing between them",
		struggle.CutBackgroundEditor: "Editor heartbeats with no writing or cursor movement for over 5 minutes, on a device with no input signal",
	}
	FlagRules = map[string]string{
		struggle.FlagScriptedInput:    "Input or heartbeat timing too regular for a person",
		struggle.FlagAutomatedSession: "AI turns started with no input at the keyboard before them",
		struggle.FlagOTelMismatch:     "Claude Code measured far less active time than the minutes credited in its sessions",
		struggle.FlagLongDay:          "More than 12 credited hours in one local day",
		struggle.FlagLowSignal:        "Credited without an OS input signal, from editor activity alone",
		struggle.FlagMultiDevice:      "Time credited on two or more devices in the same minute",
	}
)

// DeviceInfo describes a device for the packet.
type DeviceInfo struct {
	ID   string
	Name string
	OS   string
	// Sealed are the days the device has an evidence record for (any
	// order; days outside the range only matter for the first one).
	Sealed []string
}

// DeviceRows are the rows a device's part revealed (Checked.Revealed).
type DeviceRows struct {
	DeviceID string
	Rows     []evidence.Row
}

// RawDay is a day's time at the keyboard for the project.
type RawDay struct {
	Day       string
	Seconds   int64
	AISeconds int64
}

// PacketInput is everything BuildPacket reads.
type PacketInput struct {
	Project string
	Range   Range
	// Results are every frozen row with its result (VerifyPart, Unbacked).
	Results []RowResult
	// Capped are the frozen capped seconds of the project.
	Capped []CappedMinute
	// CapInputs are the frozen cap inputs; they say which minutes were
	// credited on two or more devices (the multi_device flag).
	CapInputs []CapInput
	// Revealed are the rows each device's part revealed.
	Revealed []DeviceRows
	Devices  []DeviceInfo
	Raw      []RawDay
	// RepoURL is the project's repo_url, or "".
	RepoURL string
	// Trust is the trust level at submission and whether Hackatime is
	// linked.
	Trust Trust
}

// BuildPacket builds the review packet. It is pure: the same input gives
// the same packet, so re-running the re-check is idempotent. It never
// puts file paths, other projects, prompt hashes or session ids in the
// packet. It fails when a result or capped minute lies outside the range.
func BuildPacket(in PacketInput) (Packet, error) {
	for _, r := range in.Results {
		if !in.Range.Contains(r.Row.Minute) {
			return Packet{}, fmt.Errorf("%w: row at %s is outside the range", ErrMalformed, r.Row.Minute.UTC().Format(time.RFC3339))
		}
	}
	capped := make(map[int64]int64, len(in.Capped))
	for _, c := range in.Capped {
		if !in.Range.Contains(c.Minute) {
			return Packet{}, fmt.Errorf("%w: capped minute %s is outside the range", ErrMalformed, c.Minute.UTC().Format(time.RFC3339))
		}
		capped[c.Minute.Unix()] = c.Seconds
	}
	ms := groupMinutes(in.Results)
	act := newActivity(in.Project, in.Revealed, in.Results)

	p := Packet{
		RulesVersion: rulesVersions(in.Results),
		Trust:        Trust{AtSubmission: in.Trust.AtSubmission, HackatimeLinked: in.Trust.HackatimeLinked},
		Minutes:      CountMinutes(in.Results),
		Devices:      devices(in.Devices, in.Results, in.Range),
		Cuts:         []CutSummary{},
		Flags:        []FlagSummary{},
		AISessions:   act.sessions(in.Range, capped, ms),
		Mismatches:   mismatches(in.Results),
	}
	for _, c := range in.Capped {
		p.CountedSeconds += c.Seconds
	}
	raw := map[string]int64{}
	for _, r := range in.Raw {
		p.RawSeconds += r.Seconds
		p.RawAISeconds += r.AISeconds
		raw[r.Day] += r.Seconds
	}
	p.OneThirdRuleSeconds = p.RawSeconds - p.RawAISeconds + p.RawAISeconds/3

	for _, m := range slices.Sorted(maps.Keys(ms)) {
		if secs := capped[m]; secs > 0 {
			switch act.madeOf(ms[m]) {
			case kindEditing:
				p.MadeOf.Editing += secs
			case kindWithAI:
				p.MadeOf.WithAI += secs
			default:
				p.MadeOf.Other += secs
			}
		}
	}
	p.Cuts, p.Flags = codes(ms, in.CapInputs)
	commits := act.commits(in.Range, in.RepoURL)
	for _, day := range in.Range.Days() {
		start, _, _ := evidence.DayBounds(day) // a day of the range: always valid
		pd := PacketDay{Day: day, RawSeconds: raw[day], Commits: commits[day]}
		if pd.Commits == nil {
			pd.Commits = []Commit{}
		}
		var strip strings.Builder
		strip.Grow(24 * 60)
		for i := range 24 * 60 {
			m := start.Add(time.Duration(i) * time.Minute).Unix()
			pd.CountedSeconds += capped[m]
			strip.WriteByte(stripChar(ms[m], capped[m]))
		}
		pd.Strip = strip.String()
		p.Days = append(p.Days, pd)
	}
	return p, nil
}

// CountMinutes counts the distinct minutes of results by result (Counts).
func CountMinutes(results []RowResult) Counts {
	var c Counts
	for _, rows := range groupMinutes(results) {
		switch worst(rows) {
		case ResultMismatch:
			c.Mismatch++
		case ResultUnbacked:
			c.Unbacked++
		default:
			c.Verified++
		}
	}
	return c
}

// groupMinutes groups results by minute (Unix seconds).
func groupMinutes(results []RowResult) map[int64][]RowResult {
	out := map[int64][]RowResult{}
	for _, r := range results {
		m := r.Row.Minute.Unix()
		out[m] = append(out[m], r)
	}
	return out
}

func worst(rows []RowResult) Result {
	res := ResultVerified
	for _, r := range rows {
		switch {
		case r.Result == ResultMismatch:
			return ResultMismatch
		case r.Result != ResultVerified:
			res = ResultUnbacked
		}
	}
	return res
}

// stripChar is a minute's strip character (PacketDay.Strip).
func stripChar(rows []RowResult, capped int64) byte {
	if worst(rows) == ResultMismatch && len(rows) > 0 {
		return StripMismatch
	}
	counted, unbacked, cut, agent := capped > 0, false, false, false
	for _, r := range rows {
		if r.Row.Credited > 0 {
			counted = true
			unbacked = unbacked || r.Result == ResultUnbacked
		}
		cut = cut || (r.Row.Label == string(struggle.LabelYou) && r.Row.Credited == 0 && len(r.Row.Cuts) > 0)
		agent = agent || r.Row.Label == string(struggle.LabelAgent)
	}
	switch {
	case counted && unbacked:
		return StripUnbacked
	case counted:
		return StripCounted
	case cut:
		return StripCut
	case agent:
		return StripAgentAlone
	default:
		return StripIdle
	}
}

func rulesVersions(results []RowResult) string {
	var vs []string
	for _, r := range results {
		if !slices.Contains(vs, r.Row.RulesVersion) {
			vs = append(vs, r.Row.RulesVersion)
		}
	}
	slices.Sort(vs)
	return strings.Join(vs, ", ")
}

// devices lists the devices with frozen rows, in the order given, with
// their gaps.
func devices(infos []DeviceInfo, results []RowResult, rg Range) []PacketDevice {
	has := map[string]bool{}
	for _, r := range results {
		has[r.Row.DeviceID] = true
	}
	out := []PacketDevice{}
	for _, d := range infos {
		if !has[d.ID] {
			continue
		}
		sealed := map[string]bool{}
		first := ""
		for _, s := range d.Sealed {
			sealed[s] = true
			if first == "" || s < first {
				first = s
			}
		}
		pd := PacketDevice{Name: d.Name, OS: d.OS, Gaps: []string{}}
		for _, day := range rg.Days() {
			if !sealed[day] && (first == "" || day >= first) {
				pd.Gaps = append(pd.Gaps, day)
			}
		}
		out = append(out, pd)
	}
	return out
}

func mismatches(results []RowResult) []PacketMismatch {
	var mm []RowResult
	for _, r := range results {
		if r.Result == ResultMismatch {
			mm = append(mm, r)
		}
	}
	slices.SortFunc(mm, func(a, b RowResult) int {
		return cmp.Or(b.Row.Minute.Compare(a.Row.Minute), cmp.Compare(a.Row.DeviceID, b.Row.DeviceID))
	})
	out := []PacketMismatch{}
	for _, r := range mm[:min(len(mm), MaxMismatches)] {
		pm := PacketMismatch{
			Minute: r.Row.Minute.UTC(),
			Device: Credit{Label: r.Row.Label, Credited: r.Row.Credited, Cuts: sortedCopy(r.Row.Cuts)},
			Server: Credit{Label: string(struggle.LabelIdle), Cuts: []string{}},
		}
		if r.Server != nil {
			pm.Server = Credit{Label: r.Server.Label, Credited: r.Server.Credited, Cuts: sortedCopy(r.Server.Cuts)}
		}
		out = append(out, pm)
	}
	return out
}

// codes summarises cut and flag codes over the frozen rows, plus the
// multi_device flag from the cap inputs: a submitted minute credited on
// two or more devices.
func codes(ms map[int64][]RowResult, inputs []CapInput) ([]CutSummary, []FlagSummary) {
	cuts, flags := map[string][]int64{}, map[string][]int64{}
	for m, rows := range ms {
		seenCut, seenFlag := map[string]bool{}, map[string]bool{}
		for _, r := range rows {
			for _, c := range r.Row.Cuts {
				if !seenCut[c] {
					seenCut[c] = true
					cuts[c] = append(cuts[c], m)
				}
			}
			for _, f := range r.Row.Flags {
				if !seenFlag[f] {
					seenFlag[f] = true
					flags[f] = append(flags[f], m)
				}
			}
		}
	}
	credited := map[int64]map[string]bool{}
	for _, c := range inputs {
		m := c.Minute.Unix()
		if c.Credited <= 0 || ms[m] == nil {
			continue
		}
		if credited[m] == nil {
			credited[m] = map[string]bool{}
		}
		credited[m][c.DeviceID] = true
	}
	for m, devs := range credited {
		if len(devs) >= 2 && !slices.Contains(flags[struggle.FlagMultiDevice], m) {
			flags[struggle.FlagMultiDevice] = append(flags[struggle.FlagMultiDevice], m)
		}
	}
	cs, fs := []CutSummary{}, []FlagSummary{}
	for _, code := range slices.Sorted(maps.Keys(cuts)) {
		cs = append(cs, CutSummary{Code: code, Rule: CutRules[code], Seconds: 60 * int64(len(cuts[code])), Ranges: ranges(cuts[code])})
	}
	for _, code := range slices.Sorted(maps.Keys(flags)) {
		fs = append(fs, FlagSummary{Code: code, Rule: FlagRules[code], Minutes: len(flags[code]), Ranges: ranges(flags[code])})
	}
	return cs, fs
}

// ranges merges minutes (Unix seconds) into [start, end) ranges of
// consecutive minutes.
func ranges(minutes []int64) [][2]time.Time {
	ms := slices.Sorted(slices.Values(minutes))
	out := [][2]time.Time{}
	for i := 0; i < len(ms); {
		j := i
		for j+1 < len(ms) && ms[j+1] == ms[j]+60 {
			j++
		}
		out = append(out, [2]time.Time{time.Unix(ms[i], 0).UTC(), time.Unix(ms[j]+60, 0).UTC()})
		i = j + 1
	}
	return out
}

// commitURL returns the GitHub URL of a commit when repoURL is a GitHub
// repository (https://github.com/owner/repo, with or without .git or a
// trailing slash), else "".
func commitURL(repoURL, hash string) string {
	u, err := url.Parse(strings.TrimSpace(repoURL))
	if err != nil || u.Scheme != "https" || (u.Host != "github.com" && u.Host != "www.github.com") ||
		u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return ""
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return ""
	}
	repo := strings.TrimSuffix(parts[1], ".git")
	if repo == "" || !safeSegment(parts[0]) || !safeSegment(repo) || !safeSegment(hash) {
		return ""
	}
	return "https://github.com/" + parts[0] + "/" + repo + "/commit/" + hash
}

// safeSegment reports whether s is made only of characters GitHub allows
// in owner, repository and commit names.
func safeSegment(s string) bool {
	allowed := func(c rune) bool {
		return 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9' || c == '-' || c == '_' || c == '.'
	}
	return s != "" && s != "." && s != ".." && !strings.ContainsFunc(s, func(c rune) bool { return !allowed(c) })
}

// normalProject is how a git event's project compares with the
// submission's.
func normalProject(p string) string { return projectname.Normalize(p) }

package crediting

import (
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/apneduniya/logbook/pkg/evidence"
)

// fakeServer implements the crediting endpoints by the server contract:
// device registration, minute upserts with their computation keys, the
// 30-minute finality by server clock over the whole device-minute (a final
// refusal carries the rows the server holds), the per-minute 60-second cap,
// and evidence days with signature, chain and conflict checks.
type fakeServer struct {
	now func() time.Time

	mu       sync.Mutex
	users    map[string]string            // API key → user
	keys     map[string]ed25519.PublicKey // device id → key
	byKey    map[string]string            // public key → device id
	owner    map[string]string            // device id → user
	revoked  map[string]bool
	since    map[string]time.Time    // device id → midnight UTC of the day it registered
	devices  int                     // devices ever registered, for fresh ids
	minutes  map[string]serverMinute // device|minute|project
	firstAt  map[string]time.Time    // device|minute| → when its first row was written
	days     map[string]map[string]storedDay
	requests []string
	// status, when non-zero, answers every minutes/evidence request;
	// dayStatus only evidence requests.
	status, dayStatus int
	// dropResponse applies a minutes batch but answers 502, as if the
	// answer was lost on the way back.
	dropResponse bool
	// rejectProject makes the server reject minutes of that project.
	rejectProject string
	// forceDayConflict answers 409 for that day.
	forceDayConflict string
	// maxBatch, when non-zero, refuses larger minute batches with 413.
	maxBatch int
	// rulesVersion, when set, is the only rules version the server knows.
	rulesVersion string
	// deviceLimit, when set, refuses new devices with the limit 409.
	deviceLimit   bool
	minuteBatches [][]serverMinute
	// offset is how far the server's clock is from the agent's (the
	// agent's clock is the wrong one).
	offset time.Duration
	// serverTime, when set, replaces server_time in minutes responses
	// ("-" leaves it out).
	serverTime string
	// finalRefusals counts rows refused as final with the stored rows.
	finalRefusals int
}

// serverNow is the server's clock, sent as server_time in minutes responses
// (and in every response's Date header, which the agent must ignore).
func (f *fakeServer) serverNow() time.Time { return f.now().Add(f.offset) }

// serverMinute is an uploaded row as the server stores it. A row stored
// before keys has a zero ComputedAt and a nil Snapshot.
type serverMinute struct {
	Minute     time.Time `json:"minute"`
	Project    string    `json:"project"`
	Label      string    `json:"label"`
	Credited   int       `json:"credited"`
	Cuts       []string  `json:"cuts"`
	Flags      []string  `json:"flags"`
	ComputedAt time.Time `json:"computed_at"`
	Snapshot   *int64    `json:"snapshot"`
}

type storedDay struct {
	Day           string         `json:"day"`
	PrevChainHash string         `json:"prev_chain_hash"`
	ContentHash   string         `json:"content_hash"`
	ChainHash     string         `json:"chain_hash"`
	Signature     []byte         `json:"signature"`
	RulesVersion  string         `json:"rules_version"`
	EventCounts   map[string]int `json:"event_counts"`
	SeqFrom       int64          `json:"seq_from"`
	SeqTo         int64          `json:"seq_to"`
	Buckets       int            `json:"buckets"`
	TimeFrom      *time.Time     `json:"time_from"`
	TimeTo        *time.Time     `json:"time_to"`
}

func newFakeServer(now func() time.Time) *fakeServer {
	return &fakeServer{now: now,
		// key-1 and key-1b are two API keys of one user; key-2 is another user.
		users: map[string]string{"key-1": "u1", "key-1b": "u1", "key-2": "u2"},
		keys:  map[string]ed25519.PublicKey{}, byKey: map[string]string{}, owner: map[string]string{},
		revoked: map[string]bool{}, since: map[string]time.Time{}, minutes: map[string]serverMinute{}, firstAt: map[string]time.Time{},
		days: map[string]map[string]storedDay{}}
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (f *fakeServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, r.URL.Path)
	w.Header().Set("Date", f.serverNow().UTC().Format(http.TimeFormat))
	user, ok := f.users[strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")]
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if r.URL.Path == "/api/v1/devices" {
		var req struct {
			PublicKey []byte `json:"public_key"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if id, ok := f.byKey[string(req.PublicKey)]; ok {
			if f.owner[id] != user {
				writeErr(w, http.StatusConflict, "public key is registered to another account")
				return
			}
			writeJSON(w, http.StatusOK, map[string]string{"id": id})
			return
		}
		if f.deviceLimit {
			writeErr(w, http.StatusConflict, "you already have 20 active devices; revoke one first")
			return
		}
		f.devices++
		id := fmt.Sprintf("00000000-0000-4000-8000-%012d", f.devices)
		f.keys[id], f.byKey[string(req.PublicKey)], f.owner[id] = req.PublicKey, id, user
		f.since[id] = f.serverNow().UTC().Truncate(24 * time.Hour)
		writeJSON(w, http.StatusCreated, map[string]string{"id": id})
		return
	}
	rest, ok := strings.CutPrefix(r.URL.Path, "/api/v1/devices/")
	if !ok {
		writeErr(w, http.StatusNotFound, "not found")
		return
	}
	id, what, _ := strings.Cut(rest, "/")
	if f.status != 0 || (f.dayStatus != 0 && what == "evidence-days") {
		writeErr(w, max(f.status, f.dayStatus), "scripted failure")
		return
	}
	if _, ok := f.keys[id]; !ok || f.owner[id] != user {
		writeErr(w, http.StatusNotFound, "device not found")
		return
	}
	if f.revoked[id] {
		writeErr(w, http.StatusForbidden, "device revoked")
		return
	}
	switch what {
	case "minutes":
		f.handleMinutes(w, r, id)
	case "evidence-days":
		f.handleDay(w, r, id)
	default:
		writeErr(w, http.StatusNotFound, "not found")
	}
}

func (f *fakeServer) handleMinutes(w http.ResponseWriter, r *http.Request, id string) {
	var req struct {
		RulesVersion string         `json:"rules_version"`
		Minutes      []serverMinute `json:"minutes"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.RulesVersion == "" || len(req.Minutes) > 1000 {
		writeErr(w, http.StatusBadRequest, "bad batch")
		return
	}
	if f.maxBatch > 0 && len(req.Minutes) > f.maxBatch {
		writeErr(w, http.StatusRequestEntityTooLarge, "too many minutes")
		return
	}
	if f.rulesVersion != "" && req.RulesVersion != f.rulesVersion {
		writeErr(w, http.StatusBadRequest, "unknown rules_version")
		return
	}
	if len(req.Minutes) > 0 { // an empty batch only asks for the server clock
		f.minuteBatches = append(f.minuteBatches, req.Minutes)
	}
	var rejected []rejection
	accepted := 0
	// Like the server, apply removals, then decreases, then the rest (then
	// by index), so a project can replace another and credit can move
	// between projects of a minute in one request.
	key := func(m serverMinute) string { return minuteKey(id, m.Minute) + m.Project }
	rank := func(i int) int {
		m := req.Minutes[i]
		switch {
		case isRemoval(m):
			return 0
		case m.Credited < f.minutes[key(m)].Credited:
			return 1
		}
		return 2
	}
	order := make([]int, len(req.Minutes))
	for i := range order {
		order[i] = i
	}
	slices.SortStableFunc(order, func(a, b int) int { return rank(a) - rank(b) })
	now := f.serverNow()
	for _, i := range order {
		m := req.Minutes[i]
		k := key(m)
		old, exists := f.minutes[k]
		held := f.held(id, m.Minute)
		sum, projects := m.Credited, 1
		for _, o := range held {
			if o.Project != m.Project {
				sum += o.Credited
				projects++
			}
		}
		_, sealed := f.days[id][m.Minute.UTC().Format(time.DateOnly)]
		dm := minuteKey(id, m.Minute)
		final := now.Sub(m.Minute) >= 30*time.Minute
		// A minute first written after it was final (an offline catch-up)
		// takes more projects of the same computation for 5 minutes, like
		// the server.
		first, written := f.firstAt[dm]
		catchUp := written && !first.Before(m.Minute.Add(30*time.Minute)) && now.Sub(first) < 5*time.Minute &&
			!exists && sameKey(held, m)
		switch {
		case isRemoval(m) && !exists:
			accepted++ // nothing to remove
		case m.Minute.Before(f.since[id]):
			rejected = append(rejected, rejection{Index: i, Error: "minute is before the UTC day the device was registered"})
		case sealed:
			// Like the server: an evidence record seals its day.
			rejected = append(rejected, rejection{Index: i, Error: errSealedText})
		case m.Project == f.rejectProject && f.rejectProject != "":
			rejected = append(rejected, rejection{Index: i, Error: "project refused"})
		case m.Cuts == nil || m.Flags == nil:
			rejected = append(rejected, rejection{Index: i, Error: "cuts and flags must be arrays"})
		case m.ComputedAt.IsZero() || m.Snapshot == nil:
			rejected = append(rejected, rejection{Index: i, Error: "computed_at and snapshot are required"})
		case m.Minute.After(now.Add(2 * time.Minute)):
			rejected = append(rejected, rejection{Index: i, Error: "in the future"})
		case exists && final && equalMinute(old, m):
			accepted++ // an exact repeat, key included
		case final && len(held) > 0 && !catchUp:
			// Final: the server answers with the rows it holds.
			rejected = append(rejected, rejection{Index: i, Error: "minute is final", Stored: storedRows(held)})
			f.finalRefusals++
		case isRemoval(m):
			delete(f.minutes, k)
			accepted++
		case !exists && projects > 8:
			rejected = append(rejected, rejection{Index: i, Error: "a minute can have at most 8 projects per device"})
		case sum > 60:
			rejected = append(rejected, rejection{Index: i, Error: "more than 60 seconds in a minute"})
		default:
			f.minutes[k] = m
			if !written {
				f.firstAt[dm] = now
			}
			accepted++
		}
	}
	if f.dropResponse {
		// The write is applied, but the answer never reaches the agent.
		writeErr(w, http.StatusBadGateway, "response lost")
		return
	}
	res := map[string]any{"accepted": accepted, "rejected": rejected,
		"server_time": now.UTC().Format(time.RFC3339Nano)}
	switch f.serverTime {
	case "":
	case "-":
		delete(res, "server_time")
	default:
		res["server_time"] = f.serverTime
	}
	writeJSON(w, http.StatusOK, res)
}

// rejection is one per-item refusal, as the server sends it.
type rejection struct {
	Index  int         `json:"index"`
	Error  string      `json:"error"`
	Stored []storedRow `json:"stored,omitempty"`
}

// storedRow is a held row in a refusal: the key is null for a row stored
// without one.
type storedRow struct {
	Project    string     `json:"project"`
	Label      string     `json:"label"`
	Credited   int        `json:"credited"`
	Cuts       []string   `json:"cuts"`
	Flags      []string   `json:"flags"`
	ComputedAt *time.Time `json:"computed_at"`
	Snapshot   *int64     `json:"snapshot"`
}

func storedRows(held []serverMinute) []storedRow {
	out := make([]storedRow, 0, len(held))
	for _, m := range held {
		r := storedRow{Project: m.Project, Label: m.Label, Credited: m.Credited, Cuts: m.Cuts, Flags: m.Flags, Snapshot: m.Snapshot}
		if !m.ComputedAt.IsZero() {
			r.ComputedAt = &m.ComputedAt
		}
		out = append(out, r)
	}
	return out
}

func minuteKey(id string, at time.Time) string { return id + "|" + at.UTC().Format(time.RFC3339) + "|" }

// held returns the rows the server holds for a device-minute, by project.
func (f *fakeServer) held(id string, at time.Time) []serverMinute {
	var out []serverMinute
	prefix := minuteKey(id, at)
	for k, m := range f.minutes {
		if strings.HasPrefix(k, prefix) {
			out = append(out, m)
		}
	}
	slices.SortFunc(out, func(a, b serverMinute) int { return strings.Compare(a.Project, b.Project) })
	return out
}

// errSealedText is the server's rejection of a minute in a day the device
// has sealed (internal/server/credited).
const errSealedText = "minute is in a UTC day this device has already sealed with an evidence record"

// isRemoval reports whether m removes its row: idle, no credit, cuts or
// flags (internal/server/credited.IsRemoval).
func isRemoval(m serverMinute) bool {
	return m.Label == "idle" && m.Credited == 0 && len(m.Cuts) == 0 && len(m.Flags) == 0
}

// sameKey reports whether every held row carries m's key.
func sameKey(held []serverMinute, m serverMinute) bool {
	for _, h := range held {
		if !h.ComputedAt.Equal(m.ComputedAt) || h.Snapshot == nil || m.Snapshot == nil || *h.Snapshot != *m.Snapshot {
			return false
		}
	}
	return true
}

// equalMinute reports whether b repeats a exactly, key included.
func equalMinute(a, b serverMinute) bool {
	return a.Label == b.Label && a.Credited == b.Credited && slices.Equal(a.Cuts, b.Cuts) && slices.Equal(a.Flags, b.Flags) &&
		a.ComputedAt.Equal(b.ComputedAt) && a.Snapshot != nil && b.Snapshot != nil && *a.Snapshot == *b.Snapshot
}

func (f *fakeServer) handleDay(w http.ResponseWriter, r *http.Request, id string) {
	var d storedDay
	if err := json.NewDecoder(r.Body).Decode(&d); err != nil {
		writeErr(w, http.StatusBadRequest, "bad body")
		return
	}
	ed := evidence.Day{DeviceID: id, Day: d.Day, PrevChainHash: d.PrevChainHash, ContentHash: d.ContentHash, RulesVersion: d.RulesVersion,
		SeqFrom: d.SeqFrom, SeqTo: d.SeqTo, Buckets: d.Buckets, TimeFrom: d.TimeFrom, TimeTo: d.TimeTo}
	if err := evidence.Verify(f.keys[id], ed, d.ChainHash, d.Signature); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	days := f.days[id]
	if days == nil {
		days = map[string]storedDay{}
		f.days[id] = days
	}
	if d.Day == f.forceDayConflict {
		writeErr(w, http.StatusConflict, "conflicting evidence for this day")
		return
	}
	if old, ok := days[d.Day]; ok {
		if old.ChainHash == d.ChainHash {
			writeJSON(w, http.StatusOK, map[string]string{})
			return
		}
		writeErr(w, http.StatusConflict, "conflicting evidence for this day")
		return
	}
	want := ""
	latest := ""
	for day, s := range days {
		if day < d.Day && day > latest {
			latest, want = day, s.ChainHash
		}
	}
	// Each block starts where the previous one ended (ADR 0019 point 2).
	if d.PrevChainHash != want || latest != "" && d.SeqFrom != days[latest].SeqTo {
		writeErr(w, http.StatusConflict, "chain mismatch")
		return
	}
	days[d.Day] = d
	writeJSON(w, http.StatusCreated, map[string]string{})
}

func (f *fakeServer) minute(id string, at time.Time, project string) (serverMinute, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	m, ok := f.minutes[id+"|"+at.UTC().Format(time.RFC3339)+"|"+project]
	return m, ok
}

func (f *fakeServer) storedDays(id string) []storedDay {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []storedDay
	for _, d := range f.days[id] {
		out = append(out, d)
	}
	slices.SortFunc(out, func(a, b storedDay) int { return strings.Compare(a.Day, b.Day) })
	return out
}

func (f *fakeServer) set(fn func(f *fakeServer)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

func (f *fakeServer) requestCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.requests)
}

func (f *fakeServer) batches() [][]serverMinute {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.minuteBatches)
}

// heldBetween returns the rows the server holds for a device with minutes
// in [from, to).
func (f *fakeServer) heldBetween(id string, from, to time.Time) []serverMinute {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []serverMinute
	for k, m := range f.minutes {
		if strings.HasPrefix(k, id+"|") && !m.Minute.Before(from) && m.Minute.Before(to) {
			out = append(out, m)
		}
	}
	return out
}

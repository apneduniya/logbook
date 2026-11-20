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
// device registration, minute upserts with the 30-minute rewrite rule by
// server clock and the per-minute 60-second cap, and evidence days with
// signature, chain and conflict checks.
type fakeServer struct {
	now func() time.Time

	mu       sync.Mutex
	keys     map[string]ed25519.PublicKey // device id → key
	byKey    map[string]string            // public key → device id
	revoked  map[string]bool
	minutes  map[string]serverMinute // device|minute|project
	days     map[string]map[string]storedDay
	requests []string
	// status, when non-zero, answers every minutes/evidence request.
	status int
	// rejectProject makes the server reject minutes of that project.
	rejectProject string
	// forceDayConflict answers 409 for that day.
	forceDayConflict string
	// maxBatch, when non-zero, refuses larger minute batches with 413.
	maxBatch      int
	minuteBatches [][]serverMinute
}

type serverMinute struct {
	Minute   time.Time `json:"minute"`
	Project  string    `json:"project"`
	Label    string    `json:"label"`
	Credited int       `json:"credited"`
	Cuts     []string  `json:"cuts"`
	Flags    []string  `json:"flags"`
}

type storedDay struct {
	Day           string         `json:"day"`
	PrevChainHash string         `json:"prev_chain_hash"`
	ContentHash   string         `json:"content_hash"`
	ChainHash     string         `json:"chain_hash"`
	Signature     []byte         `json:"signature"`
	RulesVersion  string         `json:"rules_version"`
	EventCounts   map[string]int `json:"event_counts"`
}

func newFakeServer(now func() time.Time) *fakeServer {
	return &fakeServer{now: now, keys: map[string]ed25519.PublicKey{}, byKey: map[string]string{},
		revoked: map[string]bool{}, minutes: map[string]serverMinute{}, days: map[string]map[string]storedDay{}}
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
	if r.Header.Get("Authorization") != "Bearer key-1" {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if r.URL.Path == "/api/v1/devices" {
		var req struct {
			PublicKey []byte `json:"public_key"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if id, ok := f.byKey[string(req.PublicKey)]; ok {
			writeJSON(w, http.StatusOK, map[string]string{"id": id})
			return
		}
		id := fmt.Sprintf("00000000-0000-4000-8000-%012d", len(f.keys)+1)
		f.keys[id], f.byKey[string(req.PublicKey)] = req.PublicKey, id
		writeJSON(w, http.StatusCreated, map[string]string{"id": id})
		return
	}
	rest, ok := strings.CutPrefix(r.URL.Path, "/api/v1/devices/")
	if !ok {
		writeErr(w, http.StatusNotFound, "not found")
		return
	}
	id, what, _ := strings.Cut(rest, "/")
	if f.status != 0 {
		writeErr(w, f.status, "scripted failure")
		return
	}
	if _, ok := f.keys[id]; !ok {
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
	f.minuteBatches = append(f.minuteBatches, req.Minutes)
	type rej struct {
		Index int    `json:"index"`
		Error string `json:"error"`
	}
	var rejected []rej
	accepted := 0
	for i, m := range req.Minutes {
		k := id + "|" + m.Minute.Format(time.RFC3339) + "|" + m.Project
		old, exists := f.minutes[k]
		sum := m.Credited
		for k2, o := range f.minutes {
			if strings.HasPrefix(k2, id+"|"+m.Minute.Format(time.RFC3339)+"|") && o.Project != m.Project {
				sum += o.Credited
			}
		}
		switch {
		case m.Project == f.rejectProject && f.rejectProject != "":
			rejected = append(rejected, rej{i, "project refused"})
		case m.Cuts == nil || m.Flags == nil:
			rejected = append(rejected, rej{i, "cuts and flags must be arrays"})
		case m.Minute.After(f.now().Add(2 * time.Minute)):
			rejected = append(rejected, rej{i, "in the future"})
		case exists && f.now().Sub(m.Minute) >= 30*time.Minute && !equalMinute(old, m):
			rejected = append(rejected, rej{i, "minute is final"})
		case sum > 60:
			rejected = append(rejected, rej{i, "more than 60 seconds in a minute"})
		default:
			f.minutes[k] = m
			accepted++
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"accepted": accepted, "rejected": rejected})
}

func equalMinute(a, b serverMinute) bool {
	return a.Label == b.Label && a.Credited == b.Credited && slices.Equal(a.Cuts, b.Cuts) && slices.Equal(a.Flags, b.Flags)
}

func (f *fakeServer) handleDay(w http.ResponseWriter, r *http.Request, id string) {
	var d storedDay
	if err := json.NewDecoder(r.Body).Decode(&d); err != nil {
		writeErr(w, http.StatusBadRequest, "bad body")
		return
	}
	ed := evidence.Day{DeviceID: id, Day: d.Day, PrevChainHash: d.PrevChainHash, ContentHash: d.ContentHash, RulesVersion: d.RulesVersion}
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
	if d.PrevChainHash != want {
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

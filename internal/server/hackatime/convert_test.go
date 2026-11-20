package hackatime

import (
	"testing"
	"time"
)

func ptr[T any](v T) *T { return &v }

func TestToWire(t *testing.T) {
	from := time.Date(2025, 3, 1, 0, 0, 0, 0, time.UTC)
	to := from.Add(24 * time.Hour)
	now := from.Add(48 * time.Hour)
	at := float64(from.Add(time.Hour).Unix()) + 0.25

	full := RemoteHeartbeat{
		Time: &at, Entity: "/src/main.go", Type: "file", Category: "coding", Project: "logbook",
		Branch: "main", Language: "Go", Dependencies: []string{"fmt"}, Editor: "vscode", Machine: "m",
		OperatingSystem: "linux", UserAgent: "wakatime/1", IsWrite: ptr(true), Lines: ptr(10), Lineno: ptr(2),
		Cursorpos: ptr(3), LineAdditions: ptr(4), LineDeletions: ptr(5), ProjectRootCount: ptr(6),
		SourceType: "direct_entry", AIModel: "claude", AISession: "s1", AIInputTokens: ptr[int64](100),
		AIOutputTokens: ptr[int64](200), AIPromptLength: ptr(30), AILineChanges: ptr(7), HumanLineChanges: ptr(8),
	}
	w, ok := toWire(full, from, to, now)
	if !ok {
		t.Fatal("toWire(full) rejected")
	}
	if w.Time != at || w.Entity != full.Entity || !w.IsWrite || *w.Lines != 10 || *w.ProjectRootCount != 6 ||
		w.AIModel != "claude" || w.AISession != "s1" || *w.AIInputTokens != 100 || *w.AIOutputTokens != 200 ||
		*w.AIPromptLength != 30 || *w.AILineChanges != 7 || *w.HumanLineChanges != 8 || *w.LineDeletions != 5 {
		t.Fatalf("toWire(full) = %+v", w)
	}

	tests := []struct {
		name string
		mod  func(h *RemoteHeartbeat)
		ok   bool
	}{
		{"plain", func(*RemoteHeartbeat) {}, true},
		{"nil is_write", func(h *RemoteHeartbeat) { h.IsWrite = nil }, true},
		{"test entry", func(h *RemoteHeartbeat) { h.SourceType = "test_entry" }, false},
		{"no time", func(h *RemoteHeartbeat) { h.Time = nil }, false},
		{"before window", func(h *RemoteHeartbeat) { h.Time = ptr(float64(from.Add(-time.Hour).Unix())) }, false},
		{"after window", func(h *RemoteHeartbeat) { h.Time = ptr(float64(to.Add(time.Hour).Unix())) }, false},
		{"empty entity", func(h *RemoteHeartbeat) { h.Entity = "" }, false},
		{"huge entity", func(h *RemoteHeartbeat) { h.Entity = string(make([]byte, 5000)) }, false},
		{"huge project", func(h *RemoteHeartbeat) { h.Project = string(make([]byte, 300)) }, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := full
			tt.mod(&h)
			if _, ok := toWire(h, from, to, now); ok != tt.ok {
				t.Fatalf("toWire() ok = %v, want %v", ok, tt.ok)
			}
		})
	}
}

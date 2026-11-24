package statuspage

import (
	"strings"
	"testing"
	"time"

	"github.com/apneduniya/logbook/internal/agent/crediting"
	"github.com/apneduniya/logbook/internal/agent/localdb"
	"github.com/apneduniya/logbook/internal/agent/status"
	"github.com/apneduniya/logbook/internal/agent/upload"
)

func TestRender(t *testing.T) {
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	r, err := New(func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name      string
		snap      status.Snapshot
		want, not []string
	}{
		{
			name: "paused and unknown hooks",
			snap: status.Snapshot{
				Version: "v1", ServerURL: "https://lb.example", DaemonRunning: true, StartedAt: now.Add(-10 * time.Minute),
				Logbook: status.Uploader{Queue: localdb.QueueStats{Pending: 7, Dropped: 2},
					Worker: upload.State{Paused: upload.ReasonReLogin, LastError: "server answered 401 <b>"}},
				Today: status.Today{CodingText: "1 hr 2 mins"},
			},
			want: []string{"https://lb.example", "re-login needed", "unknown", "1 hr 2 mins", "&lt;b&gt;", "10 min ago", ">no<"},
			not:  []string{"<b>"},
		},
		{
			name: "hooks reported",
			snap: status.Snapshot{SignedIn: true, MirrorHost: "hackatime.hackclub.com",
				Hooks: []status.Hook{{Name: "claude-code", State: "installed"}}},
			want: []string{"claude-code", "installed", "on (hackatime.hackclub.com)", "not running"},
			not:  []string{"unknown", "<script"},
		},
		{
			name: "crediting",
			snap: status.Snapshot{Crediting: crediting.Status{
				Device:   crediting.DeviceStatus{ID: "0b7c2f1e-3a4d-4e5f-8a9b-0c1d2e3f4a5b", RevokedAt: now.Add(-time.Hour)},
				Minutes:  crediting.MinutesStatus{Waiting: 12, Rejected: 3, Paused: "device revoked"},
				Evidence: crediting.EvidenceStatus{LastDay: "2026-11-19", ChainHash: "4592bad6f7249eaaf98f", State: "conflict", Error: "refused <x>"},
			}},
			want: []string{"0b7c2f1e-3a4d-4e5f-8a9b-0c1d2e3f4a5b", "Revoked", "device revoked", "signed through 2026-11-19",
				"4592bad6f724<", "Evidence stopped", "refused &lt;x&gt;", ">12<", ">3<"},
			not: []string{"<x>", "Minutes on hold", "Sealing failed", "Sealing day"},
		},
		{
			name: "crediting held and sealing failing",
			snap: status.Snapshot{Crediting: crediting.Status{
				Minutes:  crediting.MinutesStatus{Refused: "the server refused credited minutes (unknown rules_version)"},
				Evidence: crediting.EvidenceStatus{LocalError: "reading active time: disk I/O error"},
			}},
			want: []string{"Minutes on hold", "unknown rules_version", "Sealing failed (retrying)", "disk I/O error"},
		},
		{
			name: "a day being sealed",
			snap: status.Snapshot{Crediting: crediting.Status{
				Evidence: crediting.EvidenceStatus{LastDay: "2026-11-18", Sealing: "2026-11-19"},
			}},
			want: []string{"Sealing day", "2026-11-19", "closed to new minutes"},
		},
		{
			name: "clock behind the last sealed day",
			snap: status.Snapshot{Crediting: crediting.Status{
				Minutes: crediting.MinutesStatus{Clock: "the system clock is behind the last sealed day"},
			}},
			want: []string{">Clock<", "behind the last sealed day"},
		},
		{
			name: "no hooks detected",
			snap: status.Snapshot{Hooks: []status.Hook{}},
			want: []string{"no AI tools detected"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var b strings.Builder
			if err := r.Render(&b, tt.snap); err != nil {
				t.Fatal(err)
			}
			for _, w := range tt.want {
				if !strings.Contains(b.String(), w) {
					t.Errorf("page lacks %q", w)
				}
			}
			for _, n := range tt.not {
				if strings.Contains(b.String(), n) {
					t.Errorf("page contains %q", n)
				}
			}
		})
	}
}

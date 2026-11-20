package statuspage

import (
	"strings"
	"testing"
	"time"

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

package ingest_test

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/apneduniya/logbook/internal/server/ingest"
	"github.com/apneduniya/logbook/pkg/wire"
)

const vscodeUA = "wakatime/v1.102.1 (linux-6.8.0-49-generic-x86_64) go1.23.4 vscode/1.95.3 vscode-wakatime/24.9.1"

// TestIngestAttribution pins the attribution rule that every caller gets,
// whether heartbeats arrive over HTTP (with a Client) or from the importer
// (zero Client).
func TestIngestAttribution(t *testing.T) {
	type stored struct{ UserAgent, Editor, OS, Plugin, Machine string }
	tests := []struct {
		name   string
		edit   func(*wire.Heartbeat)
		client ingest.Client
		want   stored
	}{
		{
			name: "body user_agent wins over header and overrides editor and os",
			edit: func(h *wire.Heartbeat) {
				h.UserAgent, h.Editor, h.OperatingSystem = vscodeUA, "body-editor", "body-os"
			},
			client: ingest.Client{UserAgent: "wakatime/v1.0.0 (darwin-23.0.0-arm64) go1.22 neovim/0.10.0 vim-wakatime/11.0.0"},
			want:   stored{vscodeUA, "vscode", "linux", "vscode-wakatime/24.9.1", ""},
		},
		{
			name:   "importer path without a client still parses the body user_agent",
			edit:   func(h *wire.Heartbeat) { h.UserAgent, h.Editor = vscodeUA, "" },
			client: ingest.Client{},
			want:   stored{vscodeUA, "vscode", "linux", "vscode-wakatime/24.9.1", ""},
		},
		{
			name:   "body plugin stands in for a missing user_agent",
			edit:   func(h *wire.Heartbeat) { h.Editor, h.Plugin = "", vscodeUA },
			client: ingest.Client{UserAgent: "curl/8.0"},
			want:   stored{vscodeUA, "vscode", "linux", vscodeUA, ""},
		},
		{
			name:   "header user agent fills in when the body has neither",
			edit:   func(h *wire.Heartbeat) { h.Editor = "body-editor" },
			client: ingest.Client{UserAgent: vscodeUA},
			want:   stored{vscodeUA, "vscode", "linux", "vscode-wakatime/24.9.1", ""},
		},
		{
			name:   "unparseable user agent keeps the body editor",
			edit:   func(h *wire.Heartbeat) { h.UserAgent, h.Editor = "wakatime/13.0.6", "body-editor" },
			client: ingest.Client{},
			want:   stored{"wakatime/13.0.6", "body-editor", "", "", ""},
		},
		{
			name:   "client machine overrides the body machine",
			edit:   func(h *wire.Heartbeat) { h.UserAgent, h.Editor, h.Machine = "wakatime/13.0.6", "", "body-box" },
			client: ingest.Client{Machine: "my laptop"},
			want:   stored{"wakatime/13.0.6", "", "", "", "my laptop"},
		},
		{
			name:   "empty client machine keeps the body machine",
			edit:   func(h *wire.Heartbeat) { h.UserAgent, h.Editor, h.Machine = "wakatime/13.0.6", "", "body-box" },
			client: ingest.Client{},
			want:   stored{"wakatime/13.0.6", "", "", "", "body-box"},
		},
		{
			name:   "long header values are truncated instead of failing validation",
			edit:   func(h *wire.Heartbeat) { h.Editor = "" },
			client: ingest.Client{UserAgent: "wakatime/" + strings.Repeat("1", 300), Machine: strings.Repeat("é", 200)},
			// 256 bytes for the user agent; the machine is cut on a rune boundary (2-byte runes).
			want: stored{"wakatime/" + strings.Repeat("1", 247), "", "", "", strings.Repeat("é", 128)},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc, st, _ := newService(t)
			user := newUser(t, st, "UTC")
			h := beat(base, "p")
			tt.edit(&h)
			res, err := svc.IngestBatch(context.Background(), user, []wire.Heartbeat{h}, ingest.SourceEditor, tt.client)
			if err != nil {
				t.Fatal(err)
			}
			if res.Items[0].Err != nil {
				t.Fatalf("item rejected: %v", res.Items[0].Err)
			}
			var got stored
			err = st.Pool.QueryRow(context.Background(),
				`SELECT user_agent, editor, operating_system, plugin, machine FROM heartbeats WHERE user_id = $1`, user).
				Scan(&got.UserAgent, &got.Editor, &got.OS, &got.Plugin, &got.Machine)
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Fatalf("stored %+v\nwant   %+v", got, tt.want)
			}
		})
	}
}

// TestIngestPathsDeduplicateTogether proves a heartbeat that arrived live and
// the same heartbeat re-sent by the importer get the same fields hash.
func TestIngestPathsDeduplicateTogether(t *testing.T) {
	svc, st, _ := newService(t)
	ctx := context.Background()
	user := newUser(t, st, "UTC")
	h := beat(base, "p")
	h.Editor = "" // the live plugin leaves it to the user agent
	live, err := svc.IngestBatch(ctx, user, []wire.Heartbeat{h}, ingest.SourceEditor, ingest.Client{UserAgent: vscodeUA})
	if err != nil || live.Inserted != 1 {
		t.Fatalf("live IngestBatch() = %+v, %v", live, err)
	}
	// What the Hackatime export returns for it: user_agent recorded, editor
	// and OS from Hackatime's own parsing (possibly differently spelled).
	imported := h
	imported.UserAgent, imported.Editor, imported.OperatingSystem = vscodeUA, "VS Code", "Linux"
	n, err := svc.Ingest(ctx, user, []wire.Heartbeat{imported}, ingest.SourceImport)
	if err != nil || n != 0 {
		t.Fatalf("import Ingest() = %d, %v; want 0 new", n, err)
	}
}

func TestIngestClearsPlaceholders(t *testing.T) {
	svc, st, _ := newService(t)
	ctx := context.Background()
	user := newUser(t, st, "UTC")
	h1 := beat(base, "alpha")
	h2 := beat(base.Add(time.Minute), "<<LAST_PROJECT>>")
	h2.Language, h2.Branch = "<<LAST_LANGUAGE>>", "<<LAST_BRANCH>>"
	h3 := beat(base.Add(2*time.Minute), "alpha")
	if _, err := svc.Ingest(ctx, user, []wire.Heartbeat{h1, h2, h3}, ingest.SourceEditor); err != nil {
		t.Fatal(err)
	}
	if n := countRows(t, st, "SELECT count(*) FROM projects WHERE user_id = $1 AND name LIKE '<<%'", user); n != 0 {
		t.Fatalf("created %d placeholder projects", n)
	}
	var project, language, branch string
	err := st.Pool.QueryRow(ctx,
		`SELECT project, language, branch FROM heartbeats WHERE user_id = $1 AND time = $2`, user, base.Add(time.Minute)).
		Scan(&project, &language, &branch)
	if err != nil {
		t.Fatal(err)
	}
	if project != "" || language != "" || branch != "" {
		t.Fatalf("stored project=%q language=%q branch=%q, want all empty", project, language, branch)
	}
	want := []minuteRow{
		// Each gap is credited to the later heartbeat (Hackatime's rule).
		{Minute: base, Project: "Unknown", Language: "", Editor: "vscode", Category: "coding", Label: "you", Seconds: 60},
		{Minute: base.Add(time.Minute), Project: "alpha", Language: "Go", Editor: "vscode", Category: "coding", Label: "you", Seconds: 60},
	}
	if got := minutesOf(t, st, user); !reflect.DeepEqual(got, want) {
		t.Fatalf("user_minutes =\n%+v\nwant\n%+v", got, want)
	}
}

// TestIngestLeavesUnchangedMinutesAlone checks that recomputing a window
// doesn't rewrite rows whose seconds didn't change.
func TestIngestLeavesUnchangedMinutesAlone(t *testing.T) {
	svc, st, _ := newService(t)
	ctx := context.Background()
	user := newUser(t, st, "UTC")
	var hbs []wire.Heartbeat
	for i := range 10 {
		hbs = append(hbs, beat(base.Add(time.Duration(i)*30*time.Second), "alpha"))
	}
	if _, err := svc.Ingest(ctx, user, hbs, ingest.SourceEditor); err != nil {
		t.Fatal(err)
	}
	versions := func() map[time.Time]string {
		rows, err := st.Pool.Query(ctx, `SELECT minute, xmin::text FROM user_minutes WHERE user_id = $1`, user)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		out := map[time.Time]string{}
		for rows.Next() {
			var m time.Time
			var x string
			if err := rows.Scan(&m, &x); err != nil {
				t.Fatal(err)
			}
			out[m.UTC()] = x
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		return out
	}
	before := versions()
	rowsBefore := minutesOf(t, st, user)

	// Same time and attributes as an existing heartbeat, different entity:
	// a new row that changes no minute's seconds.
	extra := beat(base.Add(2*time.Minute), "alpha")
	extra.Entity = "/home/me/alpha/other.go"
	if n, err := svc.Ingest(ctx, user, []wire.Heartbeat{extra}, ingest.SourceEditor); err != nil || n != 1 {
		t.Fatalf("Ingest() = %d, %v", n, err)
	}
	if after := minutesOf(t, st, user); !reflect.DeepEqual(after, rowsBefore) {
		t.Fatalf("minutes changed:\n%+v\nwant\n%+v", after, rowsBefore)
	}
	if after := versions(); !reflect.DeepEqual(after, before) {
		t.Fatalf("unchanged rows were rewritten: xmin %v -> %v", before, after)
	}
}

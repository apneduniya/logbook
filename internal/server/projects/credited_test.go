package projects_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/apneduniya/logbook/internal/server/projects"
	"github.com/apneduniya/logbook/internal/server/store/seedtest"
)

func credited(p projects.Project) any {
	if p.CreditedSeconds == nil {
		return nil
	}
	return *p.CreditedSeconds
}

func TestProjectsCreditedNullWithoutDevices(t *testing.T) {
	f := setup(t)
	got := decode[struct{ Projects []projects.Project }](t, f.do(t, http.MethodGet, "/api/v1/me/projects", f.a, "")).Projects
	for _, p := range got {
		if p.CreditedSeconds != nil {
			t.Fatalf("%s credited = %v, want null", p.Name, credited(p))
		}
	}
}

func TestProjectsCredited(t *testing.T) {
	f := setup(t)
	base := time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC)
	laptop := seedtest.Device(t, f.st, f.a, "laptop")
	desktop := seedtest.Device(t, f.st, f.a, "desktop")
	seedtest.CreditedSpan(t, f.st, f.a, laptop, base, 10, seedtest.Credited{Seconds: 50})
	// Overlap on another device and project: each minute is capped at 60 s,
	// split 50:20 → 42 / 17 (rounded down).
	seedtest.CreditedSpan(t, f.st, f.a, desktop, base, 10, seedtest.Credited{Project: "a/b", Seconds: 20})
	// A project only the agent saw, with no heartbeats: listed, raw total 0.
	seedtest.CreditedSpan(t, f.st, f.a, laptop, base.Add(5*time.Hour), 3, seedtest.Credited{Project: "ai-only", Seconds: 60})
	// Bob's device crediting a project named like Alice's doesn't leak.
	bobDevice := seedtest.Device(t, f.st, f.b, "bob")
	seedtest.CreditedSpan(t, f.st, f.b, bobDevice, base, 30, seedtest.Credited{Seconds: 60})

	got := decode[struct{ Projects []projects.Project }](t, f.do(t, http.MethodGet, "/api/v1/me/projects", f.a, "")).Projects
	byName := map[string]projects.Project{}
	for _, p := range got {
		byName[p.Name] = p
	}
	want := map[string]any{"logbook": int64(420), "a/b": int64(170), "ai-only": int64(180), "agentonly": int64(0)}
	if len(byName) != len(want) {
		t.Fatalf("projects = %+v", got)
	}
	for name, w := range want {
		if c := credited(byName[name]); c != w {
			t.Errorf("%s credited = %v, want %v", name, c, w)
		}
	}
	if p := byName["ai-only"]; p.TotalSeconds != 0 || p.LastActivity == nil || got[0].Name != "ai-only" {
		t.Errorf("ai-only = %+v, order %s first", p, got[0].Name)
	}

	rec := f.do(t, http.MethodGet, "/api/v1/me/projects/logbook", f.a, "")
	if p := decode[projects.Project](t, rec); credited(p) != int64(420) || p.TotalSeconds != 2400 {
		t.Fatalf("get logbook = %+v (credited %v)", p, credited(p))
	}
	// Bob has a device; his own project shows his own credit only.
	rec = f.do(t, http.MethodGet, "/api/v1/me/projects/logbook", f.b, "")
	if p := decode[projects.Project](t, rec); credited(p) != int64(1800) || p.TotalSeconds != 0 {
		t.Fatalf("bob's logbook = %+v (credited %v)", p, credited(p))
	}
}

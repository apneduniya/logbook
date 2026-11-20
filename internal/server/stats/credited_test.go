package stats

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/apneduniya/logbook/internal/server/httpapi/httpapitest"
	"github.com/apneduniya/logbook/internal/server/store/seedtest"
	"github.com/apneduniya/logbook/internal/server/store/storetest"
)

func i64(v int64) *int64 { return &v }

func val(p *int64) any {
	if p == nil {
		return nil
	}
	return *p
}

func TestCreditedIsNullWithoutDevices(t *testing.T) {
	st := storetest.New(t)
	u := seedtest.User(t, st, "alice", "UTC", true)
	seedtest.Span(t, st, u, utc(2026, 11, 20, 9, 0), 10, seedtest.Minute{})

	h := NewHandler(NewService(st), httpapitest.StubSession)
	mux := http.NewServeMux()
	h.Register(mux)
	h.svc.now = func() time.Time { return utc(2026, 11, 20, 12, 0) }
	for _, target := range []string{"/api/v1/me/today", "/api/v1/me/summary?start=2026-11-20&end=2026-11-20"} {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httpapitest.Request(t, http.MethodGet, target, u, ""))
		if rec.Code != http.StatusOK {
			t.Fatalf("%s = %d %s", target, rec.Code, rec.Body)
		}
		var body map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		for _, k := range []string{"credited_seconds", "credited_labels", "multi_device_minutes"} {
			v, ok := body[k]
			if !ok || v != nil {
				t.Errorf("%s: %s = %v (present %v), want null", target, k, v, ok)
			}
		}
		if body["total_seconds"] != float64(600) {
			t.Errorf("%s: raw total_seconds = %v, want 600", target, body["total_seconds"])
		}
		p := body["projects"].([]any)[0].(map[string]any)
		if v, ok := p["credited_seconds"]; !ok || v != nil {
			t.Errorf("%s: project credited_seconds = %v, want null", target, v)
		}
	}
}

func TestCreditedSummary(t *testing.T) {
	st := storetest.New(t)
	ctx := context.Background()
	alice := seedtest.User(t, st, "alice", "UTC", true)
	bob := seedtest.User(t, st, "bob", "UTC", true)
	laptop := seedtest.Device(t, st, alice, "laptop")
	desktop := seedtest.Device(t, st, alice, "desktop")
	bobs := seedtest.Device(t, st, bob, "bob")

	// Raw time for comparison: 10 minutes on logbook.
	seedtest.Span(t, st, alice, utc(2026, 11, 20, 9, 0), 10, seedtest.Minute{})
	// 09:00-09:09 on the laptop: 45 s each on logbook.
	seedtest.CreditedSpan(t, st, alice, laptop, utc(2026, 11, 20, 9, 0), 10, seedtest.Credited{Seconds: 45})
	// 09:05-09:09 the desktop also credits 'you' on site: 45 + 45 > 60, so
	// those 5 minutes are capped and split 30/30.
	seedtest.CreditedSpan(t, st, alice, desktop, utc(2026, 11, 20, 9, 5), 5, seedtest.Credited{Project: "site", Seconds: 45})
	// 10:00-10:02 agent minutes on the laptop: no credit.
	seedtest.CreditedSpan(t, st, alice, laptop, utc(2026, 11, 20, 10, 0), 3, seedtest.Credited{Label: "agent"})
	// 10:00 the desktop sees an idle minute too: the minute counts once, as agent.
	seedtest.CreditedSpan(t, st, alice, desktop, utc(2026, 11, 20, 10, 0), 1, seedtest.Credited{Label: "idle"})
	// Next day, after the range.
	seedtest.CreditedSpan(t, st, alice, laptop, utc(2026, 11, 21, 9, 0), 5, seedtest.Credited{Seconds: 60})
	// Bob's minutes never count for Alice.
	seedtest.CreditedSpan(t, st, bob, bobs, utc(2026, 11, 20, 9, 0), 30, seedtest.Credited{Seconds: 60})

	svc := NewService(st)
	sum, err := svc.Summary(ctx, alice, utc(2026, 11, 19, 0, 0), utc(2026, 11, 21, 0, 0), time.UTC, Filter{})
	if err != nil {
		t.Fatal(err)
	}
	// logbook: 5 × 45 + 5 × 30 = 375; site: 5 × 30 = 150.
	if val(sum.CreditedSeconds) != int64(525) {
		t.Errorf("CreditedSeconds = %v, want 525", val(sum.CreditedSeconds))
	}
	if sum.TotalSeconds != 600 {
		t.Errorf("raw TotalSeconds = %d, want 600", sum.TotalSeconds)
	}
	if sum.CreditedLabels == nil || *sum.CreditedLabels != (Labels{You: 600, Agent: 180}) {
		t.Errorf("CreditedLabels = %+v", sum.CreditedLabels)
	}
	if val(sum.MultiDeviceMinutes) != int64(5) {
		t.Errorf("MultiDeviceMinutes = %v, want 5", val(sum.MultiDeviceMinutes))
	}
	want := []ProjectItem{
		{Name: "logbook", Seconds: 600, CreditedSeconds: i64(375)},
		{Name: "site", Seconds: 0, CreditedSeconds: i64(150)},
	}
	if len(sum.Projects) != len(want) {
		t.Fatalf("Projects = %+v", sum.Projects)
	}
	for i := range want {
		g := sum.Projects[i]
		if g.Name != want[i].Name || g.Seconds != want[i].Seconds || val(g.CreditedSeconds) != val(want[i].CreditedSeconds) {
			t.Errorf("Projects[%d] = %+v (credited %v), want %+v (credited %v)", i, g, val(g.CreditedSeconds), want[i], val(want[i].CreditedSeconds))
		}
	}
	if len(sum.Days) != 2 || val(sum.Days[0].CreditedSeconds) != int64(0) || val(sum.Days[1].CreditedSeconds) != int64(525) {
		t.Errorf("Days = %+v", sum.Days)
	}

	// A project filter keeps the cap computed over the whole minute.
	site := "site"
	filtered, err := svc.Summary(ctx, alice, utc(2026, 11, 20, 0, 0), utc(2026, 11, 21, 0, 0), time.UTC, Filter{Project: &site})
	if err != nil {
		t.Fatal(err)
	}
	if val(filtered.CreditedSeconds) != int64(150) || val(filtered.MultiDeviceMinutes) != int64(0) {
		t.Errorf("filtered credited = %v, multi = %v", val(filtered.CreditedSeconds), val(filtered.MultiDeviceMinutes))
	}

	// A user with a device but nothing in range gets zeros, not nulls.
	empty, err := svc.Summary(ctx, alice, utc(2026, 1, 1, 0, 0), utc(2026, 1, 2, 0, 0), time.UTC, Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if val(empty.CreditedSeconds) != int64(0) || empty.CreditedLabels == nil || val(empty.MultiDeviceMinutes) != int64(0) {
		t.Errorf("empty range = %+v", empty)
	}
}

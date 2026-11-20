package profiles_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/apneduniya/logbook/internal/server/store/seedtest"
)

func ptr(p *int64) any {
	if p == nil {
		return nil
	}
	return *p
}

func TestPublicProfileCreditedIsNullWithoutDevices(t *testing.T) {
	f := setup(t)
	rec := f.do(t, http.MethodGet, "/api/v1/users/alice/profile", uuid.Nil, "")
	var body struct {
		Totals   map[string]any   `json:"totals"`
		Projects []map[string]any `json:"projects"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"credited_all_time_seconds", "credited_last_7_days_seconds"} {
		if v, ok := body.Totals[k]; !ok || v != nil {
			t.Errorf("totals.%s = %v (present %v), want null", k, v, ok)
		}
	}
	if v, ok := body.Projects[0]["credited_seconds"]; !ok || v != nil {
		t.Errorf("project credited_seconds = %v, want null", v)
	}
}

func TestPublicProfileCredited(t *testing.T) {
	f := setup(t)
	now := time.Now().UTC().Truncate(time.Minute)
	laptop := seedtest.Device(t, f.st, f.pub, "laptop")
	desktop := seedtest.Device(t, f.st, f.pub, "desktop")
	// 30 days ago: 10 minutes at 60 s.
	seedtest.CreditedSpan(t, f.st, f.pub, laptop, now.Add(-30*24*time.Hour), 10, seedtest.Credited{Seconds: 60})
	// An hour ago: both devices credit the same 5 minutes; capped at 60 s.
	seedtest.CreditedSpan(t, f.st, f.pub, laptop, now.Add(-time.Hour), 5, seedtest.Credited{Seconds: 60})
	seedtest.CreditedSpan(t, f.st, f.pub, desktop, now.Add(-time.Hour), 5, seedtest.Credited{Seconds: 60})
	// Hidden project: never shown.
	seedtest.CreditedSpan(t, f.st, f.pub, laptop, now.Add(-2*time.Hour), 5, seedtest.Credited{Project: "secret-project", Seconds: 60})
	// A project with credited time and no raw time.
	seedtest.CreditedSpan(t, f.st, f.pub, laptop, now.Add(-3*time.Hour), 2, seedtest.Credited{Project: "notes", Seconds: 30})
	// Bob's device and minutes never show on Alice's profile.
	bobDevice := seedtest.Device(t, f.st, f.priv, "bob")
	seedtest.CreditedSpan(t, f.st, f.priv, bobDevice, now.Add(-time.Hour), 30, seedtest.Credited{Seconds: 60})

	p, err := f.svc.Public(context.Background(), "alice")
	if err != nil {
		t.Fatal(err)
	}
	if ptr(p.Totals.CreditedAllTimeSeconds) != int64(15*60+60) || ptr(p.Totals.CreditedLast7DaysSeconds) != int64(5*60+60) {
		t.Errorf("credited totals = %v / %v", ptr(p.Totals.CreditedAllTimeSeconds), ptr(p.Totals.CreditedLast7DaysSeconds))
	}
	if p.Totals.AllTimeSeconds != 90*60 {
		t.Errorf("raw total changed: %d", p.Totals.AllTimeSeconds)
	}
	got := map[string]any{}
	for _, pp := range p.Projects {
		if strings.Contains(pp.Name, "secret") {
			t.Fatalf("hidden project shown: %+v", pp)
		}
		got[pp.Name] = ptr(pp.CreditedSeconds)
	}
	if got["logbook"] != int64(15*60) || got["notes"] != int64(60) || len(got) != 2 {
		t.Errorf("project credited = %v", got)
	}
}

// Credited uploads must move the public version so live viewers refetch.
func TestPublicVersionTracksCredited(t *testing.T) {
	f := setup(t)
	d := seedtest.Device(t, f.st, f.pub, "laptop")
	before, _, err := f.svc.PublicVersion(context.Background(), f.pub, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	seedtest.CreditedSpan(t, f.st, f.pub, d, time.Now().Add(-time.Hour), 1, seedtest.Credited{Seconds: 60})
	after, _, err := f.svc.PublicVersion(context.Background(), f.pub, time.Now().Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if before == after {
		t.Fatal("version did not change after a credited minute")
	}
}

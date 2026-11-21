// Regression tests from the cross-model re-check of ADR 0017's review fixes: deletion floor, migration probes, minute keys and device replacement.

package e2e

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/apneduniya/logbook/internal/agent/crediting"
	"github.com/apneduniya/logbook/internal/agent/devicekey"
	"github.com/apneduniya/logbook/internal/agent/serverapi"
	"github.com/apneduniya/logbook/internal/server/devices"
	"github.com/apneduniya/logbook/internal/server/store/seedtest"
	"github.com/apneduniya/logbook/pkg/struggle"
	"github.com/google/uuid"
)

type recheckRegistrar struct {
	r   *rig
	svc *devices.Service
}

func (s recheckRegistrar) RegisterDevice(ctx context.Context, _ serverapi.Target, d serverapi.DeviceRequest) (string, error) {
	out, err := s.svc.Register(ctx, s.r.user, devices.Registration{Name: d.Name, OS: d.OS, PublicKey: d.PublicKey})
	if errors.Is(err, devices.ErrKeyTaken) {
		return "", &serverapi.Error{Kind: serverapi.KindConflict, Status: http.StatusConflict, Message: err.Error()}
	}
	return out.ID.String(), err
}

func TestRecheckCrashAfterReplacementPersistence(t *testing.T) {
	ctx := context.Background()
	r := newRig(t, m(1).Add(5*time.Second))
	keyPath := filepath.Join(t.TempDir(), "identity.key")
	reg := recheckRegistrar{r, devices.NewService(r.st, devices.WithClock(r.clock))}
	identityOpts := devicekey.Options{KeyPath: keyPath, Store: r.db, Registrar: reg, Name: "laptop", OS: "linux", Now: r.clock}
	id := devicekey.NewIdentity(identityOpts)
	old, err := id.Ensure(ctx, r.target)
	if err != nil {
		t.Fatal(err)
	}
	r.device, r.key = uuid.MustParse(old.ID), old.Key
	r.opts.Identity = id
	r.restart()
	r.present(m(-5), m(1))
	r.edit(m(0).Add(20*time.Second), "previous-account-project")
	r.with(func(r *rig) { r.down = true })
	r.runAny() // The previous account has an unacknowledged minute.
	if len(r.mustStored(m(0))) != 1 {
		t.Fatal("missing setup minute")
	}

	r.set(m(2).Add(5 * time.Second))
	r.user = seedtest.User(t, r.st, "bob", "UTC", true)
	r.target.APIKey = "bob-key"
	if err := id.Forget(ctx); err != nil {
		t.Fatal(err)
	}
	replacement, err := id.Ensure(ctx, r.target)
	if err != nil || !replacement.Replaced {
		t.Fatalf("replacement=%+v, %v", replacement, err)
	}
	// Crash here: Ensure persisted the identity, but upload never retired minutes.
	id = devicekey.NewIdentity(identityOpts)
	next, err := id.Ensure(ctx, r.target)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("after restart: Replaced=%v, pending minute before registration=%s", next.Replaced, m(0))
	r.device, r.key = uuid.MustParse(next.ID), next.Key
	r.with(func(r *rig) { r.down = false })
	r.opts.Identity = id
	r.restart()
	r.runAny()
	if rows, total := r.serverRows(m(0)); len(rows) > 0 {
		t.Fatalf("previous account's minute uploaded to replacement device: %v (%d seconds), Replaced=%v", rows, total, next.Replaced)
	}
}

func TestRecheckRemovalWithDifferentKey(t *testing.T) {
	ctx := context.Background()
	r := newRig(t, m(1).Add(5*time.Second))
	r.present(m(-5), m(10))
	r.edit(m(0).Add(20*time.Second), "a", "b")
	r.run()
	key := r.keyOf(m(0))
	if rows, _ := r.serverRows(m(0)); len(rows) != 2 {
		t.Fatalf("setup: %v", rows)
	}
	r.set(m(2).Add(5 * time.Second))
	result, err := r.client.UploadMinutes(ctx, r.target, r.device.String(), serverapi.MinutesRequest{RulesVersion: "v1", Minutes: []serverapi.Minute{{Minute: m(0), Project: "a", Label: "idle", Cuts: []string{}, Flags: []string{}, ComputedAt: m(2), Snapshot: key.H}}})
	if err != nil {
		t.Fatal(err)
	}
	replay, err := crediting.ReplayMinute(ctx, r.db, m(0), key, nil, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("answer=%+v, replay at remaining row's key=%s", result, creditText(replay))
	if rows, total := r.serverRows(m(0)); len(result.Rejected) == 0 && len(rows) == 1 {
		t.Fatalf("new-key removal accepted beside untouched old-key row: server=%v (%d seconds), replay=%s", rows, total, creditText(replay))
	}
}

func recheckLegacy(t *testing.T, r *rig, localProject string, serverRows map[string]int, target time.Time) {
	t.Helper()
	legacyDB("2026-11-19")(r)
	raw, err := sql.Open("sqlite", r.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = raw.Close() }()
	if _, err := raw.ExecContext(context.Background(), `INSERT INTO credited_minutes (minute, project, label, credited, cuts, flags, rules_version, digest, sent_digest, sent_final, cutoff) VALUES (?, ?, 'you', 60, '[]', '[]', 'v1', 'sixty', 'sixty', 1, 0)`, target.Unix(), localProject); err != nil {
		t.Fatal(err)
	}
	for p, n := range serverRows {
		if _, err := r.st.Pool.Exec(context.Background(), `INSERT INTO credited_minutes (device_id,user_id,minute,project,label,credited,cuts,flags,rules_version,updated_at) VALUES ($1,$2,$3,$4,'you',$5,'{}','{}','v1',$3)`, r.device, r.user, target, p, n); err != nil {
			t.Fatal(err)
		}
	}
}

func TestRecheckMigrationProbeProjectMissing(t *testing.T) {
	// Reachable legacy history: A's upload commits but its response is lost;
	// A still has sent_digest=''. A later result replaces A with B, deleting
	// unsent A locally. B's upload is refused, but the legacy agent marks B
	// sent anyway. Local now knows B only, while the server holds A.
	r := newRig(t, m(31).Add(5*time.Second), registeredOn(m(-1440)), beforeOpen(func(r *rig) {
		recheckLegacy(t, r, "new-name", map[string]int{"old-name": 30}, m(0))
	}))
	r.run()
	local, server := r.mustStored(m(0)), r.serverMinutes(m(0), m(1))
	if len(local) != len(server) {
		t.Fatalf("probe for absent project lost held minute: local=%+v server=%+v", local, server)
	}
}

func TestRecheckMigrationProbeSealedDay(t *testing.T) {
	r := newRig(t, m(31).Add(5*time.Second), registeredOn(m(-1440)), beforeOpen(func(r *rig) {
		// The server has the sealed record; the legacy local DB lacks it.
		recheckLegacy(t, r, "app", map[string]int{"app": 30}, m(-1440))
		raw, err := sql.Open("sqlite", r.dbPath)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = raw.Close() }()
		if _, err = raw.ExecContext(context.Background(), `DELETE FROM evidence_days`); err != nil {
			t.Fatal(err)
		}
	}))
	r.runner.RunOnce(context.Background()) // A restored DB cannot capture the old block locally.
	if local, server := r.mustStored(m(-1440)), r.serverMinutes(m(-1440), m(-1439)); len(local) != len(server) {
		t.Fatalf("sealed probe discarded held value: local=%+v server=%+v", local, server)
	}
}

func TestRecheckMigrationBeforeRegistration(t *testing.T) {
	r := newRig(t, m(31).Add(5*time.Second), registeredOn(m(0)), beforeOpen(func(r *rig) {
		recheckLegacyNoEvidence(t, r, m(-1440))
	}))
	r.runAny()
	if rows := r.mustStored(m(-1440)); len(rows) != 0 {
		t.Fatalf("before-registration probe kept=%+v", rows)
	}
}

func recheckLegacyNoEvidence(t *testing.T, r *rig, target time.Time) {
	t.Helper()
	raw, err := sql.Open("sqlite", r.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = raw.Close() }()
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := raw.ExecContext(context.Background(), q, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY, applied_at INTEGER NOT NULL)`)
	files, err := filepath.Glob("../agent/localdb/migrations/000[1-6]_*.sql")
	if err != nil {
		t.Fatal(err)
	}
	for i, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		exec(string(b))
		exec(`INSERT INTO schema_migrations VALUES (?,0)`, i+1)
	}
	exec(`INSERT INTO credited_minutes (minute,project,label,credited,cuts,flags,rules_version,digest,sent_digest,sent_final,cutoff) VALUES (?,'app','you',60,'[]','[]','v1','sixty','sixty',1,0)`, target.Unix())
}

func TestRecheckMigrationProbeOpenHeldCredit(t *testing.T) {
	r := newRig(t, m(5).Add(5*time.Second), registeredOn(m(-1440)), beforeOpen(func(r *rig) { recheckLegacy(t, r, "app", map[string]int{"app": 60}, m(0)) }))
	r.run()
	if local, server := r.mustStored(m(0)), r.serverMinutes(m(0), m(1)); len(local) != 0 || len(server) != 0 {
		t.Fatalf("documented open-minute removal: local=%+v server=%+v", local, server)
	}
}

func TestRecheckMigrationProbeNoHeldRows(t *testing.T) {
	r := newRig(t, m(31).Add(5*time.Second), registeredOn(m(-1440)), beforeOpen(func(r *rig) { recheckLegacyNoEvidence(t, r, m(0)) }))
	r.run()
	if local, server := r.mustStored(m(0)), r.serverMinutes(m(0), m(1)); len(local) != 0 || len(server) != 0 {
		t.Fatalf("empty final minute: local=%+v server=%+v", local, server)
	}
}

func TestRecheckAgentRekeysUnchangedProject(t *testing.T) {
	r := newRig(t, m(1).Add(5*time.Second))
	changed := false
	r.opts.Compute = func(in struggle.Input) struggle.Result {
		res := struggle.Compute(in)
		if changed {
			for i := range res.Minutes {
				if res.Minutes[i].Minute.Equal(m(0)) && res.Minutes[i].Project == "a" {
					res.Minutes[i].Credited = 20
				}
			}
		}
		return res
	}
	r.restart()
	r.present(m(-5), m(10))
	r.edit(m(0).Add(20*time.Second), "a", "b")
	r.run()
	key := r.keyOf(m(0))
	changed = true
	r.set(m(2).Add(5 * time.Second))
	r.run()
	rows := r.serverMinutes(m(0), m(1))
	if len(rows) != 2 || rows[0].Credited != 20 || rows[1].Credited != 30 || rows[0].ComputedAt == nil || rows[1].ComputedAt == nil || !rows[0].ComputedAt.Equal(*rows[1].ComputedAt) || rows[0].ComputedAt.Equal(key.T) {
		t.Fatalf("one-project rewrite stranded: %+v", rows)
	}
}

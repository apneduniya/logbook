package credited_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"testing"

	"github.com/google/uuid"

	"github.com/apneduniya/logbook/internal/server/store/seedtest"
	"github.com/apneduniya/logbook/internal/server/store/storetest"
	"github.com/apneduniya/logbook/pkg/submission"
)

// The offline checker recomputes the cap with submission.Cap (ADR 0020), so
// it must give exactly what the credited_project_minutes view gives. Both
// run the same cases, from pkg/submission/testdata/cap_cases.json.
func TestCapMatchesTheView(t *testing.T) {
	b, err := os.ReadFile("../../../pkg/submission/testdata/cap_cases.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		Name   string                     `json:"name"`
		Inputs []submission.CapInput      `json:"inputs"`
		Want   []submission.CappedProject `json:"want"`
	}
	if err := json.Unmarshal(b, &cases); err != nil {
		t.Fatal(err)
	}
	st := storetest.New(t)
	ctx := context.Background()
	for i, tt := range cases {
		t.Run(tt.Name, func(t *testing.T) {
			user := seedtest.User(t, st, fmt.Sprintf("cap%d", i), "UTC", true)
			devices := map[string]uuid.UUID{}
			for _, in := range tt.Inputs {
				if _, ok := devices[in.DeviceID]; !ok {
					devices[in.DeviceID] = seedtest.Device(t, st, user, in.DeviceID)
				}
				label := "you"
				if in.Credited == 0 {
					label = "agent"
				}
				seedtest.CreditedSpan(t, st, user, devices[in.DeviceID], in.Minute, 1,
					seedtest.Credited{Project: in.Project, Label: label, Seconds: int16(in.Credited)}) //nolint:gosec // 0..60
			}
			rows, err := st.Pool.Query(ctx,
				`SELECT minute, project, seconds FROM credited_project_minutes WHERE user_id = $1 ORDER BY minute, project`, user)
			if err != nil {
				t.Fatal(err)
			}
			defer rows.Close()
			var view []submission.CappedProject
			for rows.Next() {
				var c submission.CappedProject
				if err := rows.Scan(&c.Minute, &c.Project, &c.Seconds); err != nil {
					t.Fatal(err)
				}
				c.Minute = c.Minute.UTC()
				view = append(view, c)
			}
			if err := rows.Err(); err != nil {
				t.Fatal(err)
			}
			got := submission.Cap(tt.Inputs)
			if !reflect.DeepEqual(view, tt.Want) || !reflect.DeepEqual(got, view) {
				t.Fatalf("view %+v\nCap  %+v\nwant %+v", view, got, tt.Want)
			}
		})
	}
}

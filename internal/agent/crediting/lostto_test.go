package crediting

import (
	"testing"

	"github.com/apneduniya/logbook/internal/agent/localdb"
	"github.com/apneduniya/logbook/internal/agent/serverapi"
)

// Adopting the server's rows for a sealed day must still report credit the
// agent held that the server doesn't.
func TestLostTo(t *testing.T) {
	row := localdb.CreditedMinute{Project: "a", Credited: 60}
	for _, tc := range []struct {
		name string
		held []serverapi.StoredMinute
		want bool
	}{
		{"server holds nothing", []serverapi.StoredMinute{}, true},
		{"server holds less", []serverapi.StoredMinute{{Project: "a", Credited: 30}}, true},
		{"server holds the same", []serverapi.StoredMinute{{Project: "a", Credited: 60}}, false},
		{"server holds another project only", []serverapi.StoredMinute{{Project: "b", Credited: 60}}, true},
	} {
		if got := lostTo(row, tc.held); got != tc.want {
			t.Errorf("%s: lostTo = %v, want %v", tc.name, got, tc.want)
		}
	}
	if lostTo(localdb.CreditedMinute{Project: "a"}, nil) {
		t.Error("a row with no credit can't lose any")
	}
}

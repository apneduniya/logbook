package store_test

import (
	"context"
	"errors"
	"testing"

	"github.com/apneduniya/logbook/internal/server/store"
	"github.com/apneduniya/logbook/internal/server/store/db"
	"github.com/apneduniya/logbook/internal/server/store/storetest"
)

func TestInTxRollsBackOnError(t *testing.T) {
	st := storetest.New(t)
	ctx := context.Background()
	boom := errors.New("boom")

	err := st.InTx(ctx, func(q *db.Queries) error {
		if _, err := st.Pool.Exec(ctx, "SELECT 1"); err != nil {
			return err
		}
		if err := q.InsertAuditLog(ctx, db.InsertAuditLogParams{
			Action: "test", SubjectType: "t", SubjectID: "1", Data: []byte("{}"),
		}); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("InTx() err = %v, want boom", err)
	}
	var n int
	if err := st.Pool.QueryRow(ctx, "SELECT count(*) FROM audit_log").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("audit_log has %d rows after rollback", n)
	}
	if _, err := st.GetUserByUsername(ctx, "nobody"); !store.IsNotFound(err) {
		t.Fatalf("GetUserByUsername() err = %v, want not found", err)
	}
}

package admin

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/apneduniya/logbook/internal/server/store/db"
)

// ReadRecord is what a request's admin.read rows say about it.
type ReadRecord struct {
	Method, Path string
	// Params are the request's parameters (query and body).
	Params map[string]string
	// Status is the error status of a request that failed, else 0.
	Status int
}

// Audit row size (docs/api/admin.md "Audit"). Parameters are recorded with
// each value cut to maxAuditValue bytes and at most maxAuditParams bytes
// of keys and values (as JSON) in all. A request that read more than
// maxParamCopies users (only user/info_batch can) records them once, in a
// row about the key, and its rows about users point to it by request id,
// so a batch's audit rows grow with the users it read, not with the square
// of them.
const (
	maxAuditValue  = 1 << 10
	maxAuditMethod = 16
	maxAuditParams = 4 << 10
	maxParamCopies = 10
)

// AuditRead writes the admin.read rows of one request: one per subject
// user, or one about the key when the request read no user, plus one about
// the key holding the parameters when it read more than maxParamCopies
// users. Every row has the request's id.
func (k *Keys) AuditRead(ctx context.Context, a Admin, subjects []uuid.UUID, rec ReadRecord) error {
	path, method := cutUTF8(rec.Path, maxAuditValue), cutUTF8(rec.Method, maxAuditMethod)
	base := map[string]any{"method": method, "path": path, "key": a.KeyPrefix, "request": uuid.NewString()}
	if len(path) < len(rec.Path) || len(method) < len(rec.Method) {
		base["path_truncated"] = true
	}
	if rec.Status != 0 {
		base["status"] = rec.Status
	}
	full := maps.Clone(base)
	recorded, cut := auditParams(rec.Params)
	full["params"] = recorded
	if cut {
		full["params_truncated"] = true
	}
	users := make([]string, len(subjects))
	for i, s := range subjects {
		users[i] = s.String()
	}
	keyIDs := []string{a.KeyID.String()}

	var rows []db.AdminAuditReadsParams
	add := func(subjectType string, ids []string, data map[string]any) error {
		b, err := json.Marshal(data)
		if err != nil {
			return fmt.Errorf("encoding audit data: %w", err)
		}
		rows = append(rows, db.AdminAuditReadsParams{ActorUserID: a.UserID, SubjectType: subjectType, SubjectIds: ids, Data: b})
		return nil
	}
	var err error
	switch {
	case len(users) == 0:
		err = add("admin_key", keyIDs, full)
	case len(users) <= maxParamCopies:
		err = add("user", users, full)
	default:
		full["users"] = len(users)
		if err = add("admin_key", keyIDs, full); err == nil {
			err = add("user", users, base)
		}
	}
	if err != nil {
		return err
	}
	if err := k.st.InTx(ctx, func(q *db.Queries) error {
		for _, r := range rows {
			if err := q.AdminAuditReads(ctx, r); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return fmt.Errorf("writing audit log: %w", err)
	}
	return nil
}

// readNames are the parameters the admin API reads. An audit row records
// them before any other, so other parameters can't crowd them out.
var readNames = []string{
	"id", "user_id", "ids", "email", "query", "part", "start_date", "end_date",
	"project", "language", "entity", "editor", "machine", "include_ai", "limit", "offset",
}

// auditParams is p as an audit row records it: the parameters the API
// reads, then the others by name, each value cut to maxAuditValue bytes,
// until maxAuditParams bytes of JSON. cut reports whether a value was cut
// or a parameter left out.
func auditParams(p map[string]string) (out map[string]string, cut bool) {
	out = map[string]string{}
	size := 0
	order := slices.DeleteFunc(slices.Clone(readNames), func(k string) bool { _, ok := p[k]; return !ok })
	for _, k := range slices.Sorted(maps.Keys(p)) {
		if !slices.Contains(readNames, k) {
			order = append(order, k)
		}
	}
	for _, k := range order {
		v := p[k]
		if len(v) > maxAuditValue {
			v = cutUTF8(v, maxAuditValue)
			cut = true
		}
		n := jsonLen(k) + jsonLen(v)
		if size+n > maxAuditParams {
			cut = true
			continue
		}
		size += n
		out[k] = v
	}
	return out, cut
}

// cutUTF8 is s cut to at most n bytes without splitting a character.
func cutUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

func jsonLen(s string) int {
	b, _ := json.Marshal(s) // a string always encodes
	return len(b)
}

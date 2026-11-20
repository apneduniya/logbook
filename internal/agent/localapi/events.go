package localapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/apneduniya/logbook/pkg/wire"
)

// eventsResult is the answer to POST /local/v1/events.
type eventsResult struct {
	Accepted int `json:"accepted"`
	Rejected int `json:"rejected"`
}

// events stores a batch of wire.LocalEvent from a hook command. Invalid
// events are counted and skipped (resending them would never help); a
// storage failure answers 500 so the hook spools the batch instead.
func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	if !equalSecret(r.Header.Get(TokenHeader), s.opt.Settings().LocalToken) {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	var evs []wire.LocalEvent
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes)).Decode(&evs); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			writeError(w, http.StatusRequestEntityTooLarge, "request body too large")
			return
		}
		writeError(w, http.StatusBadRequest, "body must be a JSON array of events")
		return
	}
	if len(evs) > maxLocalEvents {
		writeError(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("at most %d events per request", maxLocalEvents))
		return
	}
	valid := evs[:0]
	for _, e := range evs {
		if e.Validate() == nil {
			valid = append(valid, e)
		}
	}
	res := eventsResult{Accepted: len(valid), Rejected: len(evs) - len(valid)}
	if res.Rejected > 0 {
		s.opt.Logger.WarnContext(r.Context(), "dropped invalid local events", "count", res.Rejected)
	}
	if len(valid) > 0 {
		if err := s.opt.Store.InsertEvents(r.Context(), valid); err != nil {
			s.opt.Logger.ErrorContext(r.Context(), "storing local events", "count", len(valid), "err", err)
			writeError(w, http.StatusInternalServerError, "could not store events")
			return
		}
	}
	writeJSON(w, http.StatusOK, res)
}

package localapi

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/apneduniya/logbook/pkg/wire"
)

// maxShortField matches wire's limit for user_agent and machine.
const maxShortField = 256

// single handles one heartbeat, sent as an object or a one-element array.
func (s *Server) single(okStatus int) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.authorized(r) {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		items, status, msg := decodeItems(w, r)
		if status != 0 {
			writeError(w, status, msg)
			return
		}
		if len(items) > 1 {
			writeError(w, http.StatusBadRequest, "send more than one heartbeat to heartbeats.bulk")
			return
		}
		hb, err := s.parseHeartbeat(r, items[0])
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		if !s.enqueue(w, r, []wire.Heartbeat{hb}) {
			return
		}
		writeJSON(w, okStatus, map[string]heartbeatData{"data": newHeartbeatData(hb)})
	}
}

// bulk handles up to wire.MaxBulkHeartbeats heartbeats and answers with one
// [body, status] pair per heartbeat, in order, as wakatime-cli expects.
func (s *Server) bulk(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(r) {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	items, status, msg := decodeItems(w, r)
	if status != 0 {
		writeError(w, status, msg)
		return
	}
	if len(items) > wire.MaxBulkHeartbeats {
		writeError(w, http.StatusBadRequest,
			fmt.Sprintf("Too many heartbeats in a single request (max %d)", wire.MaxBulkHeartbeats))
		return
	}
	responses := make([][2]any, len(items))
	valid := make([]wire.Heartbeat, 0, len(items))
	for i, raw := range items {
		hb, err := s.parseHeartbeat(r, raw)
		if err != nil {
			// 400 tells wakatime-cli to drop the heartbeat instead of retrying it.
			responses[i] = [2]any{map[string]string{"error": err.Error()}, http.StatusBadRequest}
			continue
		}
		valid = append(valid, hb)
		responses[i] = [2]any{map[string]heartbeatData{"data": newHeartbeatData(hb)}, http.StatusCreated}
	}
	if !s.enqueue(w, r, valid) {
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"responses": responses})
}

// enqueue stores hbs; on failure it answers 500 so wakatime-cli keeps the
// heartbeats in its own offline queue, and returns false.
func (s *Server) enqueue(w http.ResponseWriter, r *http.Request, hbs []wire.Heartbeat) bool {
	if len(hbs) == 0 {
		return true
	}
	cfg := s.opt.Settings()
	if err := s.opt.Store.Enqueue(r.Context(), hbs, s.opt.MirrorEnabled(cfg)); err != nil {
		s.opt.Logger.ErrorContext(r.Context(), "queueing heartbeats", "count", len(hbs), "err", err)
		writeError(w, http.StatusInternalServerError, "could not queue heartbeats")
		return false
	}
	s.opt.OnEnqueue()
	return true
}

// authorized checks the API key wakatime-cli sends against the configured
// Logbook key.
func (s *Server) authorized(r *http.Request) bool {
	key, ok := apiKeyFromRequest(r)
	return ok && equalSecret(strings.TrimSpace(key), s.opt.Settings().APIKey)
}

// apiKeyFromRequest reads a key from "Authorization: Bearer <key>",
// "Authorization: Basic base64(<key>)" (wakatime-cli's form; "key:" and
// ":key" are tolerated) or the api_key query parameter.
func apiKeyFromRequest(r *http.Request) (string, bool) {
	if h := r.Header.Get("Authorization"); h != "" {
		scheme, cred, found := strings.Cut(h, " ")
		if !found {
			return "", false
		}
		cred = strings.TrimSpace(cred)
		switch strings.ToLower(scheme) {
		case "bearer":
			return cred, cred != ""
		case "basic":
			raw, err := base64.StdEncoding.DecodeString(cred)
			if err != nil {
				return "", false
			}
			user, pass, hasColon := strings.Cut(string(raw), ":")
			switch {
			case !hasColon:
				return user, user != ""
			case user != "":
				return user, true
			default:
				return pass, pass != ""
			}
		}
		return "", false
	}
	if k := r.URL.Query().Get("api_key"); k != "" {
		return k, true
	}
	return "", false
}

// decodeItems reads the body as a heartbeat object or an array of them. A
// non-zero status means the whole request failed with msg.
func decodeItems(w http.ResponseWriter, r *http.Request) (items []json.RawMessage, status int, msg string) {
	var raw json.RawMessage
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err := dec.Decode(&raw); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			return nil, http.StatusRequestEntityTooLarge, "request body too large"
		}
		return nil, http.StatusBadRequest, "invalid JSON body"
	}
	raw = bytes.TrimSpace(raw)
	switch {
	case len(raw) > 0 && raw[0] == '[':
		if err := json.Unmarshal(raw, &items); err != nil {
			return nil, http.StatusBadRequest, "invalid JSON body"
		}
	case len(raw) > 0 && raw[0] == '{':
		items = []json.RawMessage{raw}
	default:
		return nil, http.StatusBadRequest, "body must be a heartbeat object or an array of heartbeats"
	}
	if len(items) == 0 {
		return nil, http.StatusBadRequest, "No data provided"
	}
	return items, 0, ""
}

// parseHeartbeat decodes and validates one heartbeat and applies the
// values wakatime-cli sends in headers, the same way the server would:
// X-Machine-Name wins over the body's machine, and User-Agent fills an empty
// user_agent when the body has no plugin either. Doing it here keeps
// attribution intact when the batch is later sent by the agent.
func (s *Server) parseHeartbeat(r *http.Request, raw json.RawMessage) (wire.Heartbeat, error) {
	var hb wire.Heartbeat
	if err := json.Unmarshal(raw, &hb); err != nil {
		var ute *json.UnmarshalTypeError
		if errors.As(err, &ute) && ute.Field != "" {
			return wire.Heartbeat{}, fmt.Errorf("%w: %s has the wrong type", wire.ErrInvalidHeartbeat, ute.Field)
		}
		return wire.Heartbeat{}, fmt.Errorf("%w: not a heartbeat object", wire.ErrInvalidHeartbeat)
	}
	if m := r.Header.Get("X-Machine-Name"); m != "" {
		if un, err := url.QueryUnescape(m); err == nil {
			m = un
		}
		hb.Machine = truncate(m, maxShortField)
	}
	if hb.UserAgent == "" && hb.Plugin == "" {
		hb.UserAgent = truncate(r.UserAgent(), maxShortField)
	}
	if err := hb.Validate(s.opt.Now()); err != nil {
		return wire.Heartbeat{}, err
	}
	return hb, nil
}

// truncate cuts s to at most n bytes, dropping a split UTF-8 sequence.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return strings.ToValidUTF8(s[:n], "")
}

// heartbeatData is a queued heartbeat as echoed back to wakatime-cli, the
// same shape the Logbook server returns.
type heartbeatData struct {
	ID              string  `json:"id"`
	Entity          string  `json:"entity"`
	Type            string  `json:"type"`
	Category        string  `json:"category"`
	Time            float64 `json:"time"`
	Project         string  `json:"project"`
	Branch          string  `json:"branch"`
	Language        string  `json:"language"`
	Editor          string  `json:"editor"`
	Machine         string  `json:"machine"`
	OperatingSystem string  `json:"operating_system"`
	UserAgent       string  `json:"user_agent"`
	IsWrite         bool    `json:"is_write"`
}

func newHeartbeatData(h wire.Heartbeat) heartbeatData {
	typ, cat := h.Type, h.Category
	if typ == "" {
		typ = "file"
	}
	if cat == "" {
		cat = "coding"
	}
	return heartbeatData{
		ID: hex.EncodeToString(h.FieldsHash()), Entity: h.Entity, Type: typ, Category: cat, Time: h.Time,
		Project: h.Project, Branch: h.Branch, Language: h.Language, Editor: h.Editor, Machine: h.Machine,
		OperatingSystem: h.OperatingSystem, UserAgent: h.UserAgent, IsWrite: h.IsWrite,
	}
}

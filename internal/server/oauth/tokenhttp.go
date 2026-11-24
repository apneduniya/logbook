package oauth

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"strings"

	"github.com/apneduniya/logbook/internal/server/httpapi"
)

// maxTokenBody caps /oauth/token and /oauth/revoke request bodies.
const maxTokenBody = 64 << 10

// tokenForm is the parsed body of /oauth/token or /oauth/revoke plus the
// client credentials, from HTTP Basic or the body.
type tokenForm struct {
	values url.Values
	client Client
	// basic is true when the client authenticated with HTTP Basic, so a
	// failure answers with WWW-Authenticate (RFC 6749 §5.2).
	basic bool
}

// parseTokenForm reads the body as a form (RFC 6749) or, as Doorkeeper
// (Rails) also accepts, a flat JSON object of strings. Query parameters are
// ignored: secrets don't belong in URLs.
func parseTokenForm(w http.ResponseWriter, r *http.Request) (tokenForm, *TokenError) {
	r.Body = http.MaxBytesReader(w, r.Body, maxTokenBody)
	var f tokenForm
	mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")) // an unparsable type is handled as unknown
	switch mt {
	case "application/json":
		var m map[string]any
		if err := json.NewDecoder(r.Body).Decode(&m); err != nil {
			return f, tokenErr("invalid_request", "invalid JSON body")
		}
		f.values = url.Values{}
		for k, v := range m {
			if s, ok := v.(string); ok {
				f.values.Set(k, s)
			}
		}
	default:
		if err := r.ParseForm(); err != nil {
			return f, tokenErr("invalid_request", "invalid form body")
		}
		f.values = r.PostForm
	}

	if h := r.Header.Get("Authorization"); h != "" {
		c, ok := basicClient(h)
		if !ok {
			return tokenForm{basic: true}, errInvalidClient
		}
		f.client, f.basic = c, true
		return f, nil
	}
	f.client = Client{ID: f.values.Get("client_id"), Secret: f.values.Get("client_secret")}
	return f, nil
}

// basicClient decodes "Basic base64(client_id:client_secret)", each part
// form-encoded (RFC 6749 §2.3.1).
func basicClient(h string) (Client, bool) {
	scheme, cred, found := strings.Cut(h, " ")
	if !found || !strings.EqualFold(scheme, "Basic") {
		return Client{}, false
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(cred))
	if err != nil {
		return Client{}, false
	}
	id, secret, found := strings.Cut(string(raw), ":")
	if !found {
		return Client{}, false
	}
	id, err1 := url.QueryUnescape(id)
	secret, err2 := url.QueryUnescape(secret)
	if err1 != nil || err2 != nil {
		return Client{}, false
	}
	return Client{ID: id, Secret: secret}, true
}

// limitClient applies the per-client budget, keyed on the client id the
// request names (or the caller's IP without one). It writes the 429 itself.
func (h *Handler) limitClient(w http.ResponseWriter, r *http.Request, clientID string) bool {
	key := "ip:" + httpapi.LimitKey(httpapi.ClientIP(r, h.trustProxy))
	if clientID != "" {
		key = "client:" + truncate(clientID, 64)
	}
	if !h.clientLimiter.Allow(key) {
		httpapi.WriteTooManyRequests(w, r, h.clientLimiter.RetryAfter(key))
		return false
	}
	return true
}

func (h *Handler) token(w http.ResponseWriter, r *http.Request) {
	f, ferr := parseTokenForm(w, r)
	if !h.limitClient(w, r, f.client.ID) {
		return
	}
	if ferr != nil {
		writeTokenError(w, r, ferr, f.basic)
		return
	}
	v := f.values
	resp, err := h.svc.Token(r.Context(), f.client, TokenRequest{
		GrantType: v.Get("grant_type"), Code: v.Get("code"), RedirectURI: v.Get("redirect_uri"),
		CodeVerifier: v.Get("code_verifier"), RefreshToken: v.Get("refresh_token"), Scope: v.Get("scope"),
	})
	if err != nil {
		writeTokenError(w, r, err, f.basic)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	httpapi.WriteJSON(w, http.StatusOK, resp)
}

func (h *Handler) revoke(w http.ResponseWriter, r *http.Request) {
	f, ferr := parseTokenForm(w, r)
	if !h.limitClient(w, r, f.client.ID) {
		return
	}
	if ferr != nil {
		writeTokenError(w, r, ferr, f.basic)
		return
	}
	if err := h.svc.Revoke(r.Context(), f.client, f.values.Get("token")); err != nil {
		writeTokenError(w, r, err, f.basic)
		return
	}
	httpapi.WriteJSON(w, http.StatusOK, struct{}{})
}

// writeTokenError renders an RFC 6749 §5.2 error. Anything that isn't a
// *TokenError is logged and answered as server_error.
func writeTokenError(w http.ResponseWriter, r *http.Request, err error, basic bool) {
	var te *TokenError
	if !errors.As(err, &te) {
		slog.ErrorContext(r.Context(), "oauth request failed", "err", err, "path", r.URL.Path)
		te = &TokenError{Code: "server_error", Description: "internal error"}
		w.Header().Set("Cache-Control", "no-store")
		httpapi.WriteJSON(w, http.StatusInternalServerError, map[string]string{"error": te.Code, "error_description": te.Description})
		return
	}
	if te.Code == "invalid_client" && basic {
		w.Header().Set("WWW-Authenticate", `Basic realm="logbook"`)
	}
	w.Header().Set("Cache-Control", "no-store")
	httpapi.WriteJSON(w, te.Status(), map[string]string{"error": te.Code, "error_description": te.Description})
}

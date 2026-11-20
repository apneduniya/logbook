package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"golang.org/x/oauth2"
)

// hcaScopes is the least we need: email for the account record and name for
// a display name and username. See HCA's /docs/api for the scope list.
var hcaScopes = []string{"email", "name"}

// maxMeBody caps the /api/v1/me response we read.
const maxMeBody = 64 << 10

// errHCANotConfigured means no HCA client id was configured.
var errHCANotConfigured = errors.New("hack club auth is not configured")

// hcaIdentity is the part of HCA's GET /api/v1/me response we use:
//
//	{"identity": {"id": "ident!abc", "first_name": "...", "last_name": "...",
//	  "primary_email": "...", "slack_id": "..."}, "scopes": ["email", "name"]}
//
// Fields are present only when the matching scope was granted.
type hcaIdentity struct {
	ID           string `json:"id"`
	FirstName    string `json:"first_name"`
	LastName     string `json:"last_name"`
	PrimaryEmail string `json:"primary_email"`
	SlackID      string `json:"slack_id"`
}

type hcaClient struct {
	oauth *oauth2.Config
	meURL string
	http  *http.Client
}

func newHCAClient(cfg HCAConfig, redirectURL string, client *http.Client) *hcaClient {
	base := strings.TrimRight(cfg.BaseURL, "/")
	if base == "" {
		base = "https://auth.hackclub.com"
	}
	if cfg.ClientID == "" {
		return &hcaClient{http: client}
	}
	return &hcaClient{
		oauth: &oauth2.Config{
			ClientID:     cfg.ClientID,
			ClientSecret: cfg.ClientSecret,
			Endpoint: oauth2.Endpoint{
				AuthURL:  base + "/oauth/authorize",
				TokenURL: base + "/oauth/token",
				// HCA (Doorkeeper) documents credentials in the form body.
				AuthStyle: oauth2.AuthStyleInParams,
			},
			RedirectURL: redirectURL,
			Scopes:      hcaScopes,
		},
		meURL: base + "/api/v1/me",
		http:  client,
	}
}

func (c *hcaClient) configured() bool { return c.oauth != nil }

// authURL returns the authorize URL with state and an S256 PKCE challenge.
func (c *hcaClient) authURL(state, verifier string) string {
	return c.oauth.AuthCodeURL(state, oauth2.S256ChallengeOption(verifier))
}

// identity exchanges an authorization code and fetches the signed-in identity.
// The HCA access token is used once here and never stored.
func (c *hcaClient) identity(ctx context.Context, code, verifier string) (hcaIdentity, error) {
	if !c.configured() {
		return hcaIdentity{}, errHCANotConfigured
	}
	ctx, cancel := context.WithTimeout(ctx, outboundTimeout)
	defer cancel()
	ctx = context.WithValue(ctx, oauth2.HTTPClient, c.http)

	tok, err := c.oauth.Exchange(ctx, code, oauth2.VerifierOption(verifier))
	if err != nil {
		return hcaIdentity{}, fmt.Errorf("exchanging HCA code: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.meURL, nil)
	if err != nil {
		return hcaIdentity{}, fmt.Errorf("building HCA me request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	tok.SetAuthHeader(req)
	resp, err := c.http.Do(req) //nolint:gosec // URL comes from server config, not user input
	if err != nil {
		return hcaIdentity{}, fmt.Errorf("fetching HCA identity: %w", err)
	}
	defer func() { _ = resp.Body.Close() }() // read-only body; close error is irrelevant
	if resp.StatusCode != http.StatusOK {
		return hcaIdentity{}, fmt.Errorf("fetching HCA identity: status %d", resp.StatusCode)
	}
	var me struct {
		Identity hcaIdentity `json:"identity"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxMeBody)).Decode(&me); err != nil {
		return hcaIdentity{}, fmt.Errorf("decoding HCA identity: %w", err)
	}
	me.Identity.ID = strings.TrimSpace(me.Identity.ID)
	// "dev:" ids are reserved for dev sign-in and must never come from HCA.
	if me.Identity.ID == "" || len(me.Identity.ID) > 128 || strings.HasPrefix(me.Identity.ID, devHCAPrefix) {
		return hcaIdentity{}, errors.New("HCA identity has no valid id")
	}
	return me.Identity, nil
}

package hackatime

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"golang.org/x/oauth2"
)

// OAuthConfigured reports whether client credentials and a redirect URL are set.
func (c *Client) OAuthConfigured() bool { return c.oauth != nil }

// AuthCodeURL is where to send the browser to authorise Logbook, with the
// given state and an S256 PKCE challenge derived from verifier.
func (c *Client) AuthCodeURL(state, verifier string) (string, error) {
	if c.oauth == nil {
		return "", ErrNotConfigured
	}
	return c.oauth.AuthCodeURL(state, oauth2.S256ChallengeOption(verifier)), nil
}

// Exchange trades an authorization code for an access token. The code is
// single use, so there are no retries.
func (c *Client) Exchange(ctx context.Context, code, verifier string) (string, error) {
	if c.oauth == nil {
		return "", ErrNotConfigured
	}
	if err := c.limit.Wait(ctx); err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	ctx = context.WithValue(ctx, oauth2.HTTPClient, c.http)
	tok, err := c.oauth.Exchange(ctx, code, oauth2.VerifierOption(verifier))
	if err != nil {
		var re *oauth2.RetrieveError
		if errors.As(err, &re) && re.Response != nil && re.Response.StatusCode >= 400 && re.Response.StatusCode < 500 {
			// Don't wrap re: its message embeds the response body.
			return "", fmt.Errorf("%w: status %d, error %q", ErrAuthorizationRejected, re.Response.StatusCode, re.ErrorCode)
		}
		if errors.As(err, &re) && re.Response != nil {
			return "", fmt.Errorf("exchanging hackatime code: %w", &StatusError{Code: re.Response.StatusCode})
		}
		return "", fmt.Errorf("exchanging hackatime code: %w", err)
	}
	if tok.AccessToken == "" || len(tok.AccessToken) > 4096 {
		return "", fmt.Errorf("exchanging hackatime code: %w: bad access token", ErrInvalidResponse)
	}
	if tt := tok.Type(); !strings.EqualFold(tt, "bearer") {
		return "", fmt.Errorf("exchanging hackatime code: %w: token type %q", ErrInvalidResponse, tt)
	}
	// The user may narrow scopes on the consent screen; import needs "read".
	if s, ok := tok.Extra("scope").(string); ok && s != "" && !slices.Contains(strings.Fields(s), "read") {
		return "", fmt.Errorf("%w: granted scope %q lacks read", ErrAuthorizationRejected, s)
	}
	return tok.AccessToken, nil
}

// Revoke asks Hackatime to revoke token (RFC 7009, Doorkeeper's
// POST /oauth/revoke). It is best effort: callers log failures and move on.
func (c *Client) Revoke(ctx context.Context, token string) error {
	if c.oauth == nil {
		return ErrNotConfigured
	}
	if err := c.limit.Wait(ctx); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	form := url.Values{
		"token":           {token},
		"token_type_hint": {"access_token"},
		"client_id":       {c.clientID},
		"client_secret":   {c.clientSecret},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint("/oauth/revoke"), strings.NewReader(form.Encode()))
	if err != nil {
		return fmt.Errorf("building revoke request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("revoking hackatime token: %w", err)
	}
	defer func() { _ = resp.Body.Close() }() // body unused
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("revoking hackatime token: %w", &StatusError{Code: resp.StatusCode})
	}
	return nil
}

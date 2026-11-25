package oauth

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/apneduniya/logbook/internal/server/store"
	"github.com/apneduniya/logbook/internal/server/store/db"
)

// Grant is an app the user has allowed to read their data.
type Grant struct {
	AppID         uuid.UUID `json:"app_id"`
	AppName       string    `json:"app_name"`
	AppSlug       string    `json:"app_slug"`
	HomepageURL   string    `json:"homepage_url"`
	OwnerUsername string    `json:"owner_username"`
	Scopes        []string  `json:"scopes"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

// Grants lists user's active grants to live apps, newest first.
func (s *Service) Grants(ctx context.Context, user uuid.UUID) ([]Grant, error) {
	rows, err := s.st.OAuthGrantsForUser(ctx, user)
	if err != nil {
		return nil, fmt.Errorf("listing grants: %w", err)
	}
	out := make([]Grant, len(rows))
	for i, r := range rows {
		out[i] = Grant{
			AppID: r.AppID, AppName: r.AppName, AppSlug: r.AppSlug, HomepageURL: r.HomepageUrl,
			OwnerUsername: r.OwnerUsername, Scopes: r.Scopes, CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt,
		}
	}
	return out, nil
}

// RevokeGrant withdraws user's grant to an app: every token and unused code
// the app holds for user stops working at once. ErrNotFound when there is
// no active grant. Audited oauth.revoke.
func (s *Service) RevokeGrant(ctx context.Context, user, appID uuid.UUID) error {
	return s.st.InTx(ctx, func(q *db.Queries) error {
		now := s.now()
		// Grant first (which waits for a refresh in flight: it holds the
		// grant FOR SHARE), then codes (which waits for an exchange in
		// flight), then tokens, so a token issued by either is revoked too.
		if _, err := q.OAuthGrantRevoke(ctx, db.OAuthGrantRevokeParams{RevokedAt: now, UserID: user, AppID: appID}); err != nil {
			if store.IsNotFound(err) {
				return ErrNotFound
			}
			return fmt.Errorf("revoking grant: %w", err)
		}
		if err := q.OAuthCodesBurnForGrant(ctx, db.OAuthCodesBurnForGrantParams{UsedAt: now, UserID: user, AppID: appID}); err != nil {
			return fmt.Errorf("burning codes: %w", err)
		}
		n, err := q.OAuthTokensRevokeGrant(ctx, db.OAuthTokensRevokeGrantParams{RevokedAt: now, UserID: pgUUID(user), AppID: appID})
		if err != nil {
			return fmt.Errorf("revoking tokens: %w", err)
		}
		return audit(ctx, q, user, "oauth.revoke", appID, map[string]any{
			"reason": "user_revoke", "tokens_revoked": n,
		})
	})
}

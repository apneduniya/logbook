package oauth

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/apneduniya/logbook/internal/server/store"
	"github.com/apneduniya/logbook/internal/server/store/db"
)

// Member roles.
const (
	RoleOwner    = "owner"
	RoleReviewer = "reviewer"
)

// App is an OAuth app (a program) as its members see it. The client secret
// is never included; see NewApp.
type App struct {
	ID              uuid.UUID  `json:"id"`
	ClientID        string     `json:"client_id"`
	Name            string     `json:"name"`
	Slug            string     `json:"slug"`
	RedirectURIs    []string   `json:"redirect_uris"`
	HomepageURL     string     `json:"homepage_url"`
	Public          bool       `json:"public"`
	Confidential    bool       `json:"confidential"`
	Role            string     `json:"role"`
	OwnerUsername   string     `json:"owner_username"`
	CreatedAt       time.Time  `json:"created_at"`
	UpdatedAt       time.Time  `json:"updated_at"`
	SecretRotatedAt *time.Time `json:"secret_rotated_at"`
}

// NewApp is returned when an app is created or its secret rotated: the
// only times the client secret is shown. ClientSecret is empty for public
// clients.
type NewApp struct {
	App
	ClientSecret string `json:"client_secret,omitempty"`
}

// AppInput is a new app as the owner submits it.
type AppInput struct {
	Name         string
	Slug         string
	RedirectURIs []string
	HomepageURL  string
	Public       bool
	// Confidential apps get a client secret; public clients (no secret)
	// must use PKCE.
	Confidential bool
}

// AppPatch holds the fields an owner may change; nil leaves a field as is.
type AppPatch struct {
	Name         *string
	RedirectURIs *[]string
	HomepageURL  *string
	Public       *bool
}

// Member is one member of an app.
type Member struct {
	Username        string    `json:"username"`
	Role            string    `json:"role"`
	AddedAt         time.Time `json:"added_at"`
	AddedByUsername *string   `json:"added_by"`
}

// Program is a public app as the submit picker lists it.
type Program struct {
	Slug        string `json:"slug"`
	Name        string `json:"name"`
	HomepageURL string `json:"homepage_url"`
}

func toApp(a db.OauthApp, role, owner string) App {
	return App{
		ID: a.ID, ClientID: a.ID.String(), Name: a.Name, Slug: a.Slug,
		RedirectURIs: a.RedirectUris, HomepageURL: a.HomepageUrl, Public: a.Public,
		Confidential: a.SecretHash != nil, Role: role, OwnerUsername: owner,
		CreatedAt: a.CreatedAt, UpdatedAt: a.UpdatedAt, SecretRotatedAt: a.SecretRotatedAt,
	}
}

func memberRowApp(r db.OAuthAppForMemberRow) db.OauthApp {
	return db.OauthApp{
		ID: r.ID, OwnerID: r.OwnerID, Name: r.Name, Slug: r.Slug, RedirectUris: r.RedirectUris,
		HomepageUrl: r.HomepageUrl, Public: r.Public, SecretHash: r.SecretHash,
		SecretRotatedAt: r.SecretRotatedAt, CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt, DeletedAt: r.DeletedAt,
	}
}

// CreateApp registers an app owned by owner and returns it with its client
// secret (for a confidential app). Audited app.create.
func (s *Service) CreateApp(ctx context.Context, owner uuid.UUID, in AppInput) (NewApp, error) {
	name, err := cleanName(in.Name)
	if err != nil {
		return NewApp{}, err
	}
	if err := checkSlug(in.Slug); err != nil {
		return NewApp{}, err
	}
	uris, err := checkRedirectURIs(in.RedirectURIs)
	if err != nil {
		return NewApp{}, err
	}
	if err := checkHomepage(in.HomepageURL); err != nil {
		return NewApp{}, err
	}
	now := s.now()
	var secret string
	var secretHash []byte
	var rotatedAt *time.Time
	if in.Confidential {
		secret = newSecret(secretPrefix)
		secretHash = hash(secret)
		rotatedAt = &now
	}
	var out NewApp
	err = s.st.InTx(ctx, func(q *db.Queries) error {
		// Serialise app creation per user so the cap can't be raced.
		if err := q.LockUser(ctx, owner); err != nil {
			return fmt.Errorf("locking user: %w", err)
		}
		n, err := q.OAuthAppCountOwned(ctx, owner)
		if err != nil {
			return fmt.Errorf("counting apps: %w", err)
		}
		if n >= MaxAppsPerUser {
			return ErrTooManyApps
		}
		a, err := q.OAuthAppInsert(ctx, db.OAuthAppInsertParams{
			OwnerID: owner, Name: name, Slug: in.Slug, RedirectUris: uris, HomepageUrl: in.HomepageURL,
			Public: in.Public, SecretHash: secretHash, SecretRotatedAt: rotatedAt, CreatedAt: now,
		})
		if store.IsNotFound(err) {
			return ErrSlugTaken
		}
		if err != nil {
			return fmt.Errorf("inserting app: %w", err)
		}
		if _, err := q.AppMemberInsert(ctx, db.AppMemberInsertParams{
			AppID: a.ID, UserID: owner, Role: RoleOwner, AddedBy: pgUUID(owner), CreatedAt: now,
		}); err != nil {
			return fmt.Errorf("adding owner: %w", err)
		}
		u, err := q.GetUserByID(ctx, owner)
		if err != nil {
			return fmt.Errorf("reading owner: %w", err)
		}
		out = NewApp{App: toApp(a, RoleOwner, u.Username), ClientSecret: secret}
		return audit(ctx, q, owner, "app.create", a.ID, map[string]any{
			"name": a.Name, "slug": a.Slug, "redirect_uris": a.RedirectUris,
			"public": a.Public, "confidential": in.Confidential,
		})
	})
	if err != nil {
		return NewApp{}, err
	}
	return out, nil
}

// ListApps returns the live apps user owns or reviews for, newest first.
func (s *Service) ListApps(ctx context.Context, user uuid.UUID) ([]App, error) {
	rows, err := s.st.OAuthAppsForMember(ctx, user)
	if err != nil {
		return nil, fmt.Errorf("listing apps: %w", err)
	}
	out := make([]App, len(rows))
	for i, r := range rows {
		out[i] = toApp(db.OauthApp{
			ID: r.ID, OwnerID: r.OwnerID, Name: r.Name, Slug: r.Slug, RedirectUris: r.RedirectUris,
			HomepageUrl: r.HomepageUrl, Public: r.Public, SecretHash: r.SecretHash,
			SecretRotatedAt: r.SecretRotatedAt, CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt,
		}, r.Role, r.OwnerUsername)
	}
	return out, nil
}

// GetApp returns one live app user is a member of, or ErrNotFound.
func (s *Service) GetApp(ctx context.Context, user, appID uuid.UUID) (App, error) {
	r, err := memberApp(ctx, s.st, user, appID)
	if err != nil {
		return App{}, err
	}
	return toApp(memberRowApp(r), r.Role, r.OwnerUsername), nil
}

// memberApp reads appID as user sees it: ErrNotFound unless user is a
// member of the live app.
func memberApp(ctx context.Context, q db.Querier, user, appID uuid.UUID) (db.OAuthAppForMemberRow, error) {
	r, err := q.OAuthAppForMember(ctx, db.OAuthAppForMemberParams{UserID: user, ID: appID})
	if store.IsNotFound(err) {
		return r, ErrNotFound
	}
	if err != nil {
		return r, fmt.Errorf("reading app: %w", err)
	}
	return r, nil
}

// ownedApp is memberApp for writes: ErrNotOwner when user is a reviewer.
// It locks the app for the rest of the transaction.
func ownedApp(ctx context.Context, q *db.Queries, user, appID uuid.UUID) (db.OAuthAppForMemberRow, error) {
	r, err := memberApp(ctx, q, user, appID)
	if err != nil {
		return r, err
	}
	if r.Role != RoleOwner {
		return r, ErrNotOwner
	}
	if _, err := q.OAuthAppLockOwned(ctx, db.OAuthAppLockOwnedParams{ID: appID, OwnerID: user}); err != nil {
		if store.IsNotFound(err) {
			return r, ErrNotFound // deleted since the read above
		}
		return r, fmt.Errorf("locking app: %w", err)
	}
	return r, nil
}

// UpdateApp changes an app's name, redirect URIs, homepage or public flag.
// Owner only. Audited app.update.
func (s *Service) UpdateApp(ctx context.Context, user, appID uuid.UUID, p AppPatch) (App, error) {
	var out App
	err := s.st.InTx(ctx, func(q *db.Queries) error {
		cur, err := ownedApp(ctx, q, user, appID)
		if err != nil {
			return err
		}
		next := db.OAuthAppUpdateParams{
			Name: cur.Name, RedirectUris: cur.RedirectUris, HomepageUrl: cur.HomepageUrl, Public: cur.Public,
			UpdatedAt: s.now(), ID: appID, OwnerID: user,
		}
		if p.Name != nil {
			if next.Name, err = cleanName(*p.Name); err != nil {
				return err
			}
		}
		if p.RedirectURIs != nil {
			if next.RedirectUris, err = checkRedirectURIs(*p.RedirectURIs); err != nil {
				return err
			}
		}
		if p.HomepageURL != nil {
			if err := checkHomepage(*p.HomepageURL); err != nil {
				return err
			}
			next.HomepageUrl = *p.HomepageURL
		}
		if p.Public != nil {
			next.Public = *p.Public
		}
		a, err := q.OAuthAppUpdate(ctx, next)
		if err != nil {
			return fmt.Errorf("updating app: %w", err)
		}
		out = toApp(a, RoleOwner, cur.OwnerUsername)
		return audit(ctx, q, user, "app.update", a.ID, map[string]any{
			"name": a.Name, "redirect_uris": a.RedirectUris, "homepage_url": a.HomepageUrl, "public": a.Public,
		})
	})
	if err != nil {
		return App{}, err
	}
	return out, nil
}

// RotateSecret replaces a confidential app's client secret and returns the
// new one. The old secret stops working at once; issued tokens stay valid.
// Owner only. Audited app.secret_rotate.
func (s *Service) RotateSecret(ctx context.Context, user, appID uuid.UUID) (NewApp, error) {
	secret := newSecret(secretPrefix)
	var out NewApp
	err := s.st.InTx(ctx, func(q *db.Queries) error {
		cur, err := ownedApp(ctx, q, user, appID)
		if err != nil {
			return err
		}
		if cur.SecretHash == nil {
			return ErrNoSecret
		}
		a, err := q.OAuthAppSetSecret(ctx, db.OAuthAppSetSecretParams{
			SecretHash: hash(secret), RotatedAt: s.now(), ID: appID, OwnerID: user,
		})
		if err != nil {
			return fmt.Errorf("rotating secret: %w", err)
		}
		out = NewApp{App: toApp(a, RoleOwner, cur.OwnerUsername), ClientSecret: secret}
		return audit(ctx, q, user, "app.secret_rotate", a.ID, nil)
	})
	if err != nil {
		return NewApp{}, err
	}
	return out, nil
}

// DeleteApp deletes an app: its tokens, codes, pending requests and grants
// stop working at once, and it leaves the program list. The row stays, so
// its slug is never reused. Owner only. Audited app.delete.
func (s *Service) DeleteApp(ctx context.Context, user, appID uuid.UUID) error {
	return s.st.InTx(ctx, func(q *db.Queries) error {
		if _, err := ownedApp(ctx, q, user, appID); err != nil {
			return err
		}
		now := s.now()
		a, err := q.OAuthAppDelete(ctx, db.OAuthAppDeleteParams{DeletedAt: now, ID: appID, OwnerID: user})
		if err != nil {
			return fmt.Errorf("deleting app: %w", err)
		}
		if err := q.OAuthRequestsDeleteForApp(ctx, appID); err != nil {
			return fmt.Errorf("deleting pending requests: %w", err)
		}
		// Grants, then codes, then tokens: the order RevokeGrant takes them
		// in, so the two can't deadlock. Revoking the grants waits for a
		// refresh in flight (it holds its grant FOR SHARE) and burning the
		// codes for an exchange in flight, so the token UPDATE sees what
		// either issued.
		if err := q.OAuthGrantsRevokeApp(ctx, db.OAuthGrantsRevokeAppParams{RevokedAt: now, AppID: appID}); err != nil {
			return fmt.Errorf("revoking grants: %w", err)
		}
		if err := q.OAuthCodesBurnForApp(ctx, db.OAuthCodesBurnForAppParams{UsedAt: now, AppID: appID}); err != nil {
			return fmt.Errorf("burning codes: %w", err)
		}
		n, err := q.OAuthTokensRevokeApp(ctx, db.OAuthTokensRevokeAppParams{RevokedAt: now, AppID: appID})
		if err != nil {
			return fmt.Errorf("revoking tokens: %w", err)
		}
		return audit(ctx, q, user, "app.delete", a.ID, map[string]any{
			"name": a.Name, "slug": a.Slug, "tokens_revoked": n,
		})
	})
}

// Members lists an app's members, owner first. Any member may read it.
func (s *Service) Members(ctx context.Context, user, appID uuid.UUID) ([]Member, error) {
	if _, err := memberApp(ctx, s.st, user, appID); err != nil {
		return nil, err
	}
	rows, err := s.st.AppMembers(ctx, appID)
	if err != nil {
		return nil, fmt.Errorf("listing members: %w", err)
	}
	out := make([]Member, len(rows))
	for i, r := range rows {
		m := Member{Username: r.Username, Role: r.Role, AddedAt: r.CreatedAt}
		if r.AddedByUsername.Valid {
			m.AddedByUsername = &r.AddedByUsername.String
		}
		out[i] = m
	}
	return out, nil
}

// AddMember makes username a reviewer of the app. Owner only, so nobody
// can add themselves to someone else's program. Audited app.member_add.
func (s *Service) AddMember(ctx context.Context, user, appID uuid.UUID, username string) (Member, error) {
	username = strings.TrimSpace(username)
	if username == "" {
		return Member{}, fmt.Errorf("%w: username is required", ErrInvalid)
	}
	var out Member
	err := s.st.InTx(ctx, func(q *db.Queries) error {
		if _, err := ownedApp(ctx, q, user, appID); err != nil {
			return err
		}
		target, err := q.GetUserByUsername(ctx, username)
		if store.IsNotFound(err) {
			return ErrUserNotFound
		}
		if err != nil {
			return fmt.Errorf("finding user: %w", err)
		}
		n, err := q.AppReviewerCount(ctx, appID)
		if err != nil {
			return fmt.Errorf("counting reviewers: %w", err)
		}
		if n >= MaxReviewers {
			return ErrTooManyReviewers
		}
		now := s.now()
		added, err := q.AppMemberInsert(ctx, db.AppMemberInsertParams{
			AppID: appID, UserID: target.ID, Role: RoleReviewer, AddedBy: pgUUID(user), CreatedAt: now,
		})
		if err != nil {
			return fmt.Errorf("adding member: %w", err)
		}
		if added == 0 {
			return ErrAlreadyMember
		}
		actor, err := q.GetUserByID(ctx, user)
		if err != nil {
			return fmt.Errorf("reading owner: %w", err)
		}
		out = Member{Username: target.Username, Role: RoleReviewer, AddedAt: now, AddedByUsername: &actor.Username}
		return audit(ctx, q, user, "app.member_add", appID, map[string]any{
			"user_id": target.ID, "username": target.Username, "role": RoleReviewer,
		})
	})
	if err != nil {
		return Member{}, err
	}
	return out, nil
}

// RemoveMember removes a reviewer. Owner only; the owner can't be removed.
// Audited app.member_remove.
func (s *Service) RemoveMember(ctx context.Context, user, appID uuid.UUID, username string) error {
	return s.st.InTx(ctx, func(q *db.Queries) error {
		app, err := ownedApp(ctx, q, user, appID)
		if err != nil {
			return err
		}
		target, err := q.GetUserByUsername(ctx, strings.TrimSpace(username))
		if store.IsNotFound(err) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("finding user: %w", err)
		}
		if target.ID == app.OwnerID {
			return fmt.Errorf("%w: the owner can't be removed", ErrInvalid)
		}
		if _, err := q.AppReviewerDelete(ctx, db.AppReviewerDeleteParams{AppID: appID, UserID: target.ID}); err != nil {
			if store.IsNotFound(err) {
				return ErrNotFound
			}
			return fmt.Errorf("removing member: %w", err)
		}
		return audit(ctx, q, user, "app.member_remove", appID, map[string]any{
			"user_id": target.ID, "username": target.Username,
		})
	})
}

// Programs lists the live public apps, by name.
func (s *Service) Programs(ctx context.Context) ([]Program, error) {
	rows, err := s.st.PublicPrograms(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing programs: %w", err)
	}
	out := make([]Program, len(rows))
	for i, r := range rows {
		out[i] = Program{Slug: r.Slug, Name: r.Name, HomepageURL: r.HomepageUrl}
	}
	return out, nil
}

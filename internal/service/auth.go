package service

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/gochathub/gochathub-server/internal/id"
	"github.com/gochathub/gochathub-server/internal/pwd"
	"github.com/gochathub/gochathub-server/internal/store"
)

// AuthService handles login, sessions, and bearer resolution.
type AuthService struct {
	App        *App
	SessionTTL time.Duration
}

// Principal identifies an authenticated actor.
type Principal struct {
	UserID    string
	Username  string
	Role      string // "user", "moderator", "admin"
	Kind      string // "session" | "api_token"
	SessionID string
	TokenID   string
}

func (p Principal) IsAdmin() bool { return p.Role == "admin" }
func (p Principal) IsModerator() bool {
	return p.Role == "admin" || p.Role == "moderator"
}

// Login verifies credentials and issues a session. Rate limiting is upstream
// (handler middleware). Failed attempts are audited.
func (a *AuthService) Login(ctx context.Context, username, password, userAgent string, ip *string) (string, store.UserRow, error) {
	row, err := a.App.Store.UserByUsername(ctx, username)
	if errors.Is(err, store.ErrNotFound) {
		// burn comparable time to avoid username probing via latency
		_, _ = pwd.Verify(dummyHash, password)
		return "", store.UserRow{}, ErrUnauthorized
	}
	if err != nil {
		return "", store.UserRow{}, fmt.Errorf("lookup user: %w", err)
	}
	if !row.Enabled {
		return "", store.UserRow{}, ErrUnauthorized
	}
	ok, err := pwd.Verify(row.PasswordHash, password)
	if err != nil || !ok {
		a.auditLogin(ctx, row.ID, ip, false)
		return "", store.UserRow{}, ErrUnauthorized
	}
	return a.issueSession(ctx, row, userAgent, ip)
}

// dummyHash equalizes verify cost on unknown usernames: a genuine argon2id
// hash computed once at startup for a fixed input.
var dummyHash = func() string {
	h, _ := pwd.Hash("gochathub-timing-equalizer")
	return h
}()

func (a *AuthService) issueSession(ctx context.Context, row store.UserRow, userAgent string, ip *string) (string, store.UserRow, error) {
	token := id.NewToken()
	sess := &store.SessionRow{
		ID:        id.NewID(),
		UserID:    row.ID,
		TokenHash: id.HashToken(token),
		ExpiresAt: time.Now().Add(a.SessionTTL),
		UserAgent: userAgent,
		IPAddress: ip,
	}
	if err := a.App.Store.CreateSession(ctx, sess); err != nil {
		return "", store.UserRow{}, fmt.Errorf("create session: %w", err)
	}
	a.auditLogin(ctx, row.ID, ip, true)
	return token, row, nil
}

func (a *AuthService) auditLogin(ctx context.Context, userID string, ip *string, ok bool) {
	action := "auth.login_failed"
	if ok {
		action = "auth.login"
	}
	detail := `{"ok":` + fmt.Sprint(ok) + `}`
	if err := a.App.Store.Audit(ctx, userID, action, "session", "", []byte(detail), ip); err != nil {
		a.App.Log.WarnContext(ctx, "audit login failed", "err", err)
	}
}

// ResolveBearer authenticates by raw bearer token; unknown/invalid → ErrUnauthorized.
func (a *AuthService) ResolveBearer(ctx context.Context, rawToken string) (Principal, error) {
	sess, user, err := a.App.Store.SessionByTokenHash(ctx, id.HashToken(rawToken))
	switch {
	case errors.Is(err, store.ErrNotFound):
		// fall through to API token
	case err != nil:
		return Principal{}, fmt.Errorf("session lookup: %w", err)
	default:
		a.touch(ctx, sess.LastSeenAt, func() { _ = a.App.Store.TouchSession(ctx, sess.ID) }, user)
		return Principal{
			UserID:    user.ID,
			Username:  user.Username,
			Role:      user.Role,
			Kind:      "session",
			SessionID: sess.ID,
		}, nil
	}
	tok, user, err := a.App.Store.APITokenByHash(ctx, id.HashToken(rawToken))
	if errors.Is(err, store.ErrNotFound) {
		return Principal{}, ErrUnauthorized
	}
	if err != nil {
		return Principal{}, fmt.Errorf("token lookup: %w", err)
	}
	var lastUsed time.Time
	if tok.LastUsedAt != nil {
		lastUsed = *tok.LastUsedAt
	}
	a.touch(ctx, lastUsed, func() { _ = a.App.Store.TouchAPIToken(ctx, tok.ID) }, user)
	return Principal{UserID: user.ID, Username: user.Username, Role: user.Role, Kind: "api_token", TokenID: tok.ID}, nil
}

// ResolveSession authenticates by session token hash (cookie flow).
func (a *AuthService) ResolveSession(ctx context.Context, rawToken string) (Principal, error) {
	sess, user, err := a.App.Store.SessionByTokenHash(ctx, id.HashToken(rawToken))
	if errors.Is(err, store.ErrNotFound) {
		return Principal{}, ErrUnauthorized
	}
	if err != nil {
		return Principal{}, fmt.Errorf("session lookup: %w", err)
	}
	a.touch(ctx, sess.LastSeenAt, func() { _ = a.App.Store.TouchSession(ctx, sess.ID) }, user)
	return Principal{UserID: user.ID, Username: user.Username, Role: user.Role, Kind: "session", SessionID: sess.ID}, nil
}

// lastSeenInterval throttles the advisory liveness writes: one UPDATE per
// (user, session, token) per minute of activity, not one per request.
const lastSeenInterval = time.Minute

// touch writes when stale: now - last stamp exceeds the interval (zero last
// stamp = never written). The middleware's per-request touches went with it.
func (a *AuthService) touch(ctx context.Context, last time.Time, write func(), user store.UserRow) {
	now := time.Now()
	stale := now.Sub(last) > lastSeenInterval
	userStale := user.LastSeenAt == nil || now.Sub(*user.LastSeenAt) > lastSeenInterval
	if stale {
		write()
	}
	if userStale && user.ID != "" {
		_ = a.App.Store.TouchLastSeen(ctx, user.ID)
	}
}

// Logout revokes the caller's session.
func (a *AuthService) Logout(ctx context.Context, p Principal) error {
	if p.Kind != "session" {
		return ErrBadRequest
	}
	return a.App.Store.RevokeSession(ctx, p.SessionID)
}

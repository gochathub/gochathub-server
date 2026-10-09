package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/gochathub/gochathub-server/internal/id"
	"github.com/gochathub/gochathub-server/internal/model"
	"github.com/gochathub/gochathub-server/internal/pwd"
	"github.com/gochathub/gochathub-server/internal/store"
)

var usernameRe = regexp.MustCompile(`^[a-z0-9_.-]{1,64}$`) // matches users CHECK

// CreateUser provisions an account (CLI path only, ADR-014).
func (u *UserService) CreateUser(ctx context.Context, username, displayName, email, password, role string) (store.UserRow, error) {
	username = strings.ToLower(strings.TrimSpace(username))
	if !usernameRe.MatchString(username) {
		return store.UserRow{}, bad("username must match [a-z0-9_.-]{1,64}")
	}
	if displayName == "" {
		return store.UserRow{}, bad("display name required")
	}
	if email != "" && (strings.ToLower(email) != email || !strings.Contains(email, "@")) {
		return store.UserRow{}, bad("email must be lowercase and contain @")
	}
	switch role {
	case "user", "moderator", "admin":
	default:
		return store.UserRow{}, bad("role must be user|moderator|admin")
	}
	hash, err := pwd.Hash(password)
	if err != nil {
		return store.UserRow{}, err
	}
	row := &store.UserRow{
		ID:           id.NewID(),
		Username:     username,
		DisplayName:  displayName,
		Email:        optString(email),
		PasswordHash: hash,
		Role:         role,
		Enabled:      true,
		Preferences:  []byte(`{}`),
	}
	if err := u.App.Store.InsertUser(ctx, row); err != nil {
		if store.IsUnique(err) {
			return store.UserRow{}, ErrConflict
		}
		return store.UserRow{}, fmt.Errorf("create user: %w", err)
	}
	return *row, u.App.Store.Audit(ctx, "", "user.create", "user", row.ID, []byte(`{}`), nil)
}

// optString converts "" to nil.
func optString(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// SetPassword resets a user's password (CLI admin path) and kills sessions.
func (u *UserService) SetPassword(ctx context.Context, username, password string) error {
	row, err := u.App.Store.UserByUsername(ctx, username)
	if err != nil {
		return ErrNotFound
	}
	hash, err := pwd.Hash(password)
	if err != nil {
		return err
	}
	if err := u.App.Store.UpdateUserPassword(ctx, row.ID, hash); err != nil {
		return fmt.Errorf("set password: %w", err)
	}
	if err := u.App.Store.RevokeUserSessions(ctx, row.ID); err != nil {
		return fmt.Errorf("revoke sessions: %w", err)
	}
	return u.App.Store.Audit(ctx, "", "user.passwd", "user", row.ID, []byte(`{}`), nil)
}

// SetEnabled toggles account state (CLI admin path).
func (u *UserService) SetEnabled(ctx context.Context, username string, enabled bool) error {
	row, err := u.App.Store.UserByUsername(ctx, username)
	if err != nil {
		return ErrNotFound
	}
	if err := u.App.Store.SetUserEnabled(ctx, row.ID, enabled); err != nil {
		return fmt.Errorf("set enabled: %w", err)
	}
	if !enabled {
		_ = u.App.Store.RevokeUserSessions(ctx, row.ID)
	}
	action := "user.disable"
	if enabled {
		action = "user.enable"
	}
	return u.App.Store.Audit(ctx, "", action, "user", row.ID, []byte(`{}`), nil)
}

// DeleteUser removes the account (CLI admin path).
func (u *UserService) DeleteUser(ctx context.Context, username string) error {
	row, err := u.App.Store.UserByUsername(ctx, username)
	if err != nil {
		return ErrNotFound
	}
	if err := u.App.Store.RevokeUserSessions(ctx, row.ID); err != nil {
		return fmt.Errorf("revoke sessions: %w", err)
	}
	if err := u.App.Store.DeleteUser(ctx, row.ID); err != nil {
		return fmt.Errorf("delete user: %w", err)
	}
	return u.App.Store.Audit(ctx, "", "user.delete", "user", row.ID, []byte(`{}`), nil)
}

// ChangeMyPassword verifies the current password, then replaces it and
// revokes all other sessions (CLI SetPassword semantic, self-service).
func (u *UserService) ChangeMyPassword(ctx context.Context, p Principal, oldPassword, newPassword string) error {
	row, err := u.App.Store.UserByID(ctx, p.UserID)
	if err != nil {
		return ErrNotFound
	}
	if ok, err := pwd.Verify(row.PasswordHash, oldPassword); err != nil || !ok {
		return ErrUnauthorized
	}
	if len(newPassword) < 8 {
		return bad("new password must be at least 8 characters")
	}
	hash, err := pwd.Hash(newPassword)
	if err != nil {
		return err
	}
	if err := u.App.Store.UpdateUserPassword(ctx, row.ID, hash); err != nil {
		return fmt.Errorf("change password: %w", err)
	}
	if err := u.App.Store.RevokeUserSessions(ctx, row.ID); err != nil {
		return fmt.Errorf("revoke sessions: %w", err)
	}
	// ponytail: revoking all sessions includes the caller's — one clean
	// relogin after a password change, same as the CLI path.
	return u.App.Store.Audit(ctx, p.UserID, "user.password_change", "user", row.ID, []byte(`{}`), nil)
}

// ListUsers returns all users for CLI listing.
func (u *UserService) ListUsers(ctx context.Context) ([]store.UserRow, error) {
	return u.App.Store.ListUsers(ctx)
}

// CurrentUser renders the caller's own profile (self render: email + own
// last-seen, prefs bypassed).
func (u *UserService) CurrentUser(ctx context.Context, p Principal) (model.User, error) {
	row, err := u.App.Store.UserByID(ctx, p.UserID)
	if err != nil {
		return model.User{}, ErrNotFound
	}
	return u.SelfUser(ctx, row)
}

// GetUser resolves another user's public profile under the same-visibility
// rule (users you share a room, contact, or pending invite with — no
// enumeration of strangers); otherwise ErrNotFound.
func (u *UserService) GetUser(ctx context.Context, p Principal, userID string) (model.User, error) {
	row, err := u.App.Store.UserByID(ctx, userID)
	if err != nil {
		return model.User{}, ErrNotFound
	}
	if row.ID != p.UserID {
		visible, err := u.App.Store.UserVisibleTo(ctx, row.ID, p.UserID)
		if err != nil {
			return model.User{}, fmt.Errorf("visibility check: %w", err)
		}
		if !visible {
			return model.User{}, ErrNotFound
		}
	}
	return u.PublicUser(ctx, row)
}

// UpdateMe applies PATCH /users/me: display name, email, timezone (IANA,
// validated here), avatar (owned ready attachment).
func (u *UserService) UpdateMe(ctx context.Context, p Principal, in model.UpdateUserInput) (model.User, error) {
	row, err := u.App.Store.UserByID(ctx, p.UserID)
	if err != nil {
		return model.User{}, ErrNotFound
	}
	patch := store.UserPatch{}
	if in.DisplayName != nil {
		d := strings.TrimSpace(*in.DisplayName)
		if d == "" || len(d) > 128 {
			return model.User{}, bad("display name must be 1-128 chars")
		}
		patch.DisplayName = &d
	}
	if in.Email != nil {
		if *in.Email == "" {
			patch.Email, patch.EmailSet = nil, true // clear
		} else {
			e := strings.ToLower(strings.TrimSpace(*in.Email))
			if !strings.Contains(e, "@") {
				return model.User{}, bad("email must contain @")
			}
			patch.Email, patch.EmailSet = &e, true
		}
	}
	if in.Timezone != nil {
		if *in.Timezone == "" {
			patch.Timezone, patch.TimezoneSet = nil, true
		} else {
			if _, err := time.LoadLocation(*in.Timezone); err != nil {
				return model.User{}, bad("timezone %q is not a valid IANA zone name", *in.Timezone)
			}
			patch.Timezone, patch.TimezoneSet = in.Timezone, true
		}
	}
	if in.AvatarAttachID != nil {
		if *in.AvatarAttachID == "" {
			patch.AvatarAttachmentID, patch.AvatarSet = nil, true
		} else {
			if err := u.checkAttachReadyOwned(ctx, *in.AvatarAttachID, p.UserID); err != nil {
				return model.User{}, err
			}
			patch.AvatarAttachmentID, patch.AvatarSet = in.AvatarAttachID, true
		}
	}
	if patch.DisplayName == nil && !patch.EmailSet && !patch.AvatarSet && !patch.TimezoneSet {
		return model.User{}, bad("no fields to update")
	}
	updated, err := u.App.Store.UpdateUserSelf(ctx, row.ID, patch)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return model.User{}, ErrNotFound
		}
		return model.User{}, fmt.Errorf("update self: %w", err)
	}
	return u.SelfUser(ctx, updated)
}

// GetPreferences returns the caller's resolved privacy prefs.
func (u *UserService) GetPreferences(ctx context.Context, p Principal) (model.Preferences, error) {
	row, err := u.App.Store.UserByID(ctx, p.UserID)
	if err != nil {
		return model.Preferences{}, ErrNotFound
	}
	return decodePrefs(row.Preferences), nil
}

// PatchPreferences merges the patch into stored prefs.
func (u *UserService) PatchPreferences(ctx context.Context, p Principal, patch model.PreferencesPatch) (model.Preferences, error) {
	if w := patch.SpellcheckWords; w != nil {
		if len(*w) > 1000 {
			return model.Preferences{}, bad("spellcheck_words: at most 1000 words")
		}
		for _, word := range *w {
			if word == "" || len(word) > 64 {
				return model.Preferences{}, bad("spellcheck_words: each word must be 1-64 bytes")
			}
		}
	}
	if patch.PrimaryColor != nil {
		v := strings.ToLower(strings.TrimSpace(*patch.PrimaryColor))
		if v != "" && !PrimaryColorSet[v] {
			return model.Preferences{}, bad("primary_color: must be one of the supported swatches")
		}
	}
	row, err := u.App.Store.UserByID(ctx, p.UserID)
	if err != nil {
		return model.Preferences{}, ErrNotFound
	}
	merged := MergePreferences(row.Preferences, patch)
	raw, err := json.Marshal(merged)
	if err != nil {
		return model.Preferences{}, err
	}
	if err := u.App.Store.SetUserPreferences(ctx, p.UserID, raw); err != nil {
		return model.Preferences{}, fmt.Errorf("set preferences: %w", err)
	}
	// The stored "" reset stays in the DB; the response reports the effective value.
	return decodePrefs(raw), nil
}

// Search finds enabled users by username/display-name substring.
func (u *UserService) Search(ctx context.Context, q string, limit int) ([]model.User, error) {
	q = strings.TrimSpace(q)
	if len(q) < 2 {
		return nil, bad("query must be at least 2 characters")
	}
	if limit < 1 || limit > 50 {
		limit = 20
	}
	rows, err := u.App.Store.SearchUsers(ctx, q, limit)
	if err != nil {
		return nil, fmt.Errorf("search users: %w", err)
	}
	out := make([]model.User, 0, len(rows))
	for _, r := range rows {
		m, err := u.PublicUser(ctx, r)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, nil
}

// CreateAPIToken mints a token for a user (CLI path); the raw value exists
// only for the duration of the call before it is displayed once.
func (u *UserService) CreateAPIToken(ctx context.Context, username, name string) (string, error) {
	row, err := u.App.Store.UserByUsername(ctx, username)
	if err != nil {
		return "", ErrNotFound
	}
	raw, _, err := u.issueAPIToken(ctx, row.ID, "", name)
	return raw, err
}

func (u *UserService) issueAPIToken(ctx context.Context, userID, actorID, name string) (string, store.APITokenRow, error) {
	raw := id.NewToken()
	t := store.APITokenRow{ID: id.NewID(), UserID: userID, Name: name, TokenHash: id.HashToken(raw)}
	if err := u.App.Store.CreateAPIToken(ctx, &t); err != nil {
		return "", t, fmt.Errorf("create token: %w", err)
	}
	_ = u.App.Store.Audit(ctx, actorID, "token.create", "api_token", t.ID, []byte(`{}`), nil)
	return raw, t, nil
}

// MintAPIToken issues a token for the signed-in user (web profile → mobile QR).
// Sessions only: a token that could mint tokens would make a leak self-renewing.
// ponytail: no expiry (decided); add expires_at here if that changes.
func (u *UserService) MintAPIToken(ctx context.Context, p Principal, name string) (string, store.APITokenRow, error) {
	if p.Kind != "session" {
		return "", store.APITokenRow{}, ErrForbidden
	}
	name = strings.TrimSpace(name)
	if name == "" {
		name = "mobile"
	}
	if len(name) > 100 {
		return "", store.APITokenRow{}, bad("name too long")
	}
	return u.issueAPIToken(ctx, p.UserID, p.UserID, name)
}

// ListAPITokens returns the caller's live tokens (metadata only, never the secret).
func (u *UserService) ListAPITokens(ctx context.Context, p Principal) ([]store.APITokenRow, error) {
	all, err := u.App.Store.ListAPITokens(ctx, p.UserID)
	if err != nil {
		return nil, err
	}
	live := all[:0]
	for _, t := range all {
		if t.RevokedAt == nil {
			live = append(live, t)
		}
	}
	return live, nil
}

// RevokeOwnAPIToken revokes one of the caller's tokens.
func (u *UserService) RevokeOwnAPIToken(ctx context.Context, p Principal, tokenID string) error {
	err := u.App.Store.RevokeAPIToken(ctx, p.UserID, tokenID)
	if errors.Is(err, store.ErrNotFound) {
		return ErrNotFound
	}
	return err
}

// RevokeAPIToken revokes a token by id (CLI path).
func (u *UserService) RevokeAPIToken(ctx context.Context, tokenID string) error {
	return u.App.Store.RevokeAPITokenByID(ctx, tokenID)
}

// --- notification modes (docs/REQUIREMENTS.md: all/mentions/directs/never) ---

// SetNotificationMode sets the default (roomID nil) or room-specific mode.
func (u *UserService) SetNotificationMode(ctx context.Context, p Principal, roomID *string, mode string) error {
	switch mode {
	case "all", "mentions", "directs", "never":
	default:
		return bad("mode must be all|mentions|directs|never")
	}
	return u.App.Store.SetNotifMode(ctx, p.UserID, roomID, mode)
}

// NotificationModes lists the caller's explicit modes (default row "").
func (u *UserService) NotificationModes(ctx context.Context, p Principal) ([]store.NotifModeRow, error) {
	return u.App.Store.ListNotifModes(ctx, p.UserID)
}

// checkAttachReadyOwned guards avatar associations.
func (u *UserService) checkAttachReadyOwned(ctx context.Context, attachID, uploader string) error {
	a, err := u.App.Store.AttachmentByID(ctx, attachID)
	if errors.Is(err, store.ErrNotFound) {
		return bad("attachment %s not found", attachID)
	}
	if err != nil {
		return err
	}
	if a.Status != "ready" {
		return bad("attachment %s is not ready", attachID)
	}
	if a.UploaderID != uploader {
		return ErrForbidden
	}
	return nil
}

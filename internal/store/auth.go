package store

import (
	"context"
	"time"
)

// SessionRow is the sessions table minus the user join.
type SessionRow struct {
	ID         string
	UserID     string
	TokenHash  []byte
	CreatedAt  time.Time
	ExpiresAt  time.Time
	RevokedAt  *time.Time
	LastSeenAt time.Time
	UserAgent  string
	IPAddress  *string
}

// CreateSession stores a hashed session token.
func (s *Store) CreateSession(ctx context.Context, sess *SessionRow) error {
	row := s.Q.QueryRow(ctx, `
		INSERT INTO sessions (id, user_id, token_hash, expires_at, user_agent, ip_address)
		VALUES ($1, $2, $3, $4, $5, $6)
		RETURNING created_at, last_seen_at`,
		sess.ID, sess.UserID, sess.TokenHash, sess.ExpiresAt, sess.UserAgent, sess.IPAddress)
	return row.Scan(&sess.CreatedAt, &sess.LastSeenAt)
}

// SessionByTokenHash returns the live session and its user; revoked and
// expired sessions are filtered out with revoked_at = null in the predicate.
func (s *Store) SessionByTokenHash(ctx context.Context, hash []byte) (SessionRow, UserRow, error) {
	const q = `SELECT
			s.id, s.user_id, s.token_hash, s.created_at, s.expires_at, s.revoked_at,
			s.last_seen_at, s.user_agent, s.ip_address::text,
			` + userCols + `
		FROM sessions s JOIN users u ON u.id = s.user_id
		WHERE s.token_hash = $1 AND s.revoked_at IS NULL AND s.expires_at > now()`
	var sess SessionRow
	var user UserRow
	err := s.Q.QueryRow(ctx, q, hash).Scan(
		&sess.ID, &sess.UserID, &sess.TokenHash, &sess.CreatedAt, &sess.ExpiresAt, &sess.RevokedAt,
		&sess.LastSeenAt, &sess.UserAgent, &sess.IPAddress,
		&user.ID, &user.Username, &user.DisplayName, &user.Email, &user.PasswordHash, &user.Role,
		&user.Enabled, &user.AvatarAttachmentID, &user.Timezone, &user.Preferences, &user.CreatedAt, &user.UpdatedAt, &user.LastSeenAt,
	)
	return sess, user, err
}

// TouchSession extends last_seen_at.
func (s *Store) TouchSession(ctx context.Context, id string) error {
	_, err := s.Q.Exec(ctx, `UPDATE sessions SET last_seen_at = now() WHERE id = $1`, id)
	return err
}

func (s *Store) RevokeSession(ctx context.Context, id string) error {
	_, err := s.Q.Exec(ctx, `UPDATE sessions SET revoked_at = now() WHERE id = $1 AND revoked_at IS NULL`, id)
	return err
}

func (s *Store) RevokeUserSessions(ctx context.Context, userID string) error {
	_, err := s.Q.Exec(ctx, `UPDATE sessions SET revoked_at = now() WHERE user_id = $1 AND revoked_at IS NULL`, userID)
	return err
}

// APITokenRow is the api_tokens shape.
type APITokenRow struct {
	ID         string
	UserID     string
	Name       string
	TokenHash  []byte
	CreatedAt  time.Time
	ExpiresAt  *time.Time
	RevokedAt  *time.Time
	LastUsedAt *time.Time
}

func (s *Store) CreateAPIToken(ctx context.Context, t *APITokenRow) error {
	row := s.Q.QueryRow(ctx, `
		INSERT INTO api_tokens (id, user_id, name, token_hash, expires_at)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING created_at`,
		t.ID, t.UserID, t.Name, t.TokenHash, t.ExpiresAt)
	return row.Scan(&t.CreatedAt)
}

func (s *Store) APITokenByHash(ctx context.Context, hash []byte) (APITokenRow, UserRow, error) {
	const q = `SELECT
			t.id, t.user_id, t.name, t.token_hash, t.created_at, t.expires_at, t.revoked_at, t.last_used_at,
			` + userCols + `
		FROM api_tokens t JOIN users u ON u.id = t.user_id
		WHERE t.token_hash = $1
		  AND t.revoked_at IS NULL
		  AND (t.expires_at IS NULL OR t.expires_at > now())
		  AND u.enabled`
	var t APITokenRow
	var user UserRow
	err := s.Q.QueryRow(ctx, q, hash).Scan(
		&t.ID, &t.UserID, &t.Name, &t.TokenHash, &t.CreatedAt, &t.ExpiresAt, &t.RevokedAt, &t.LastUsedAt,
		&user.ID, &user.Username, &user.DisplayName, &user.Email, &user.PasswordHash, &user.Role,
		&user.Enabled, &user.AvatarAttachmentID, &user.Timezone, &user.Preferences, &user.CreatedAt, &user.UpdatedAt, &user.LastSeenAt,
	)
	return t, user, err
}

func (s *Store) TouchAPIToken(ctx context.Context, id string) error {
	_, err := s.Q.Exec(ctx, `UPDATE api_tokens SET last_used_at = now() WHERE id = $1`, id)
	return err
}

func (s *Store) ListAPITokens(ctx context.Context, userID string) ([]APITokenRow, error) {
	rows, err := s.Q.Query(ctx, `
		SELECT id, user_id, name, token_hash, created_at, expires_at, revoked_at, last_used_at
		FROM api_tokens WHERE user_id = $1 ORDER BY created_at`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []APITokenRow
	for rows.Next() {
		var t APITokenRow
		if err := rows.Scan(&t.ID, &t.UserID, &t.Name, &t.TokenHash, &t.CreatedAt, &t.ExpiresAt, &t.RevokedAt, &t.LastUsedAt); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// RevokeAPIToken revokes only the caller's own token; ErrNotFound when it is
// someone else's, unknown, or already revoked.
func (s *Store) RevokeAPIToken(ctx context.Context, userID, tokenID string) error {
	tag, err := s.Q.Exec(ctx, `UPDATE api_tokens SET revoked_at = now() WHERE id = $1 AND user_id = $2 AND revoked_at IS NULL`, tokenID, userID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// RevokeAPITokenByID revokes by token id (CLI path).
func (s *Store) RevokeAPITokenByID(ctx context.Context, tokenID string) error {
	tag, err := s.Q.Exec(ctx, `UPDATE api_tokens SET revoked_at = now() WHERE id = $1 AND revoked_at IS NULL`, tokenID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

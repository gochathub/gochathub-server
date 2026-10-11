package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// SignupInviteRow is the signup_invites shape (token hash never leaves the store).
type SignupInviteRow struct {
	ID         string
	Email      string
	InvitedBy  *string
	CreatedAt  time.Time
	LastSentAt time.Time
	ExpiresAt  time.Time
	UsedAt     *time.Time
	RevokedAt  *time.Time
}

const signupInviteCols = `id, email, invited_by::text, created_at, last_sent_at, expires_at, used_at, revoked_at`

const openInvite = `used_at IS NULL AND revoked_at IS NULL`

func scanSignupInvite(row pgx.Row) (SignupInviteRow, error) {
	var r SignupInviteRow
	err := row.Scan(&r.ID, &r.Email, &r.InvitedBy, &r.CreatedAt, &r.LastSentAt, &r.ExpiresAt, &r.UsedAt, &r.RevokedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return r, ErrNotFound
	}
	return r, err
}

// UpsertSignupInvite creates the invite, or re-keys the email's open one
// (resend: new token, fresh expiry). r.ID is used for a new row only.
func (s *Store) UpsertSignupInvite(ctx context.Context, r SignupInviteRow, tokenHash []byte) (SignupInviteRow, error) {
	return scanSignupInvite(s.Q.QueryRow(ctx, `
		INSERT INTO signup_invites (id, email, token_hash, invited_by, expires_at)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (email) WHERE `+openInvite+`
		DO UPDATE SET token_hash = EXCLUDED.token_hash, expires_at = EXCLUDED.expires_at,
			invited_by = EXCLUDED.invited_by, last_sent_at = now()
		RETURNING `+signupInviteCols,
		r.ID, r.Email, tokenHash, r.InvitedBy, r.ExpiresAt))
}

func (s *Store) SignupInviteByID(ctx context.Context, id string) (SignupInviteRow, error) {
	return scanSignupInvite(s.Q.QueryRow(ctx, `SELECT `+signupInviteCols+` FROM signup_invites WHERE id = $1`, id))
}

func (s *Store) ListSignupInvites(ctx context.Context) ([]SignupInviteRow, error) {
	rows, err := s.Q.Query(ctx, `SELECT `+signupInviteCols+` FROM signup_invites ORDER BY created_at DESC LIMIT 200`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SignupInviteRow
	for rows.Next() {
		r, err := scanSignupInvite(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// RevokeSignupInvite revokes an open invite; ErrNotFound when none is open.
func (s *Store) RevokeSignupInvite(ctx context.Context, id string) error {
	tag, err := s.Q.Exec(ctx, `UPDATE signup_invites SET revoked_at = now() WHERE id = $1 AND `+openInvite, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// RevokeSignupInviteByEmail closes any open invite for the email (no-op if none).
func (s *Store) RevokeSignupInviteByEmail(ctx context.Context, email string) error {
	_, err := s.Q.Exec(ctx, `UPDATE signup_invites SET revoked_at = now() WHERE email = $1 AND `+openInvite, email)
	return err
}

// LookupSignupInvite returns the invited email for a live token.
func (s *Store) LookupSignupInvite(ctx context.Context, tokenHash []byte) (string, error) {
	var email string
	err := s.Q.QueryRow(ctx, `SELECT email FROM signup_invites
		WHERE token_hash = $1 AND `+openInvite+` AND expires_at > now()`, tokenHash).Scan(&email)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	return email, err
}

// ConsumeSignupInvite marks a live token used and returns (id, email). The row
// lock makes concurrent consumers race to exactly one winner.
func (s *Store) ConsumeSignupInvite(ctx context.Context, tokenHash []byte) (id, email string, err error) {
	err = s.Q.QueryRow(ctx, `UPDATE signup_invites SET used_at = now()
		WHERE token_hash = $1 AND `+openInvite+` AND expires_at > now()
		RETURNING id, email`, tokenHash).Scan(&id, &email)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", ErrNotFound
	}
	return id, email, err
}

// EmailTaken reports whether a user already owns the (lowercase) email.
func (s *Store) EmailTaken(ctx context.Context, email string) (bool, error) {
	var taken bool
	err := s.Q.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM users WHERE email = $1)`, email).Scan(&taken)
	return taken, err
}

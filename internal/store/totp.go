package store

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
)

// TOTPRow is the user_totp shape.
type TOTPRow struct {
	Secret   string
	Enabled  bool
	LastStep int64
}

// TOTPByUser returns ErrNotFound when the user never started enrollment.
func (s *Store) TOTPByUser(ctx context.Context, userID string) (TOTPRow, error) {
	var r TOTPRow
	err := s.Q.QueryRow(ctx, `SELECT secret, enabled, last_step FROM user_totp WHERE user_id = $1`, userID).
		Scan(&r.Secret, &r.Enabled, &r.LastStep)
	if err == pgx.ErrNoRows {
		return r, ErrNotFound
	}
	return r, err
}

// SetPendingTOTP stores a not-yet-enabled secret; an enabled row is never
// overwritten (returns false).
func (s *Store) SetPendingTOTP(ctx context.Context, userID, secret string) (bool, error) {
	tag, err := s.Q.Exec(ctx, `
		INSERT INTO user_totp (user_id, secret) VALUES ($1, $2)
		ON CONFLICT (user_id) DO UPDATE SET secret = EXCLUDED.secret, last_step = 0, created_at = now()
		WHERE user_totp.enabled = false`, userID, secret)
	return tag.RowsAffected() == 1, err
}

// ConsumeTOTPStep atomically records step; false when step was already used
// (or is older than the last accepted one) — replay protection.
func (s *Store) ConsumeTOTPStep(ctx context.Context, userID string, step int64) (bool, error) {
	tag, err := s.Q.Exec(ctx, `UPDATE user_totp SET last_step = $2 WHERE user_id = $1 AND last_step < $2`, userID, step)
	return tag.RowsAffected() == 1, err
}

func (s *Store) EnableTOTP(ctx context.Context, userID string) error {
	_, err := s.Q.Exec(ctx, `UPDATE user_totp SET enabled = true WHERE user_id = $1`, userID)
	return err
}

// DeleteTOTP removes the factor and its backup codes (disable and CLI reset).
func (s *Store) DeleteTOTP(ctx context.Context, userID string) error {
	_, err := s.Q.Exec(ctx, `WITH c AS (DELETE FROM user_backup_codes WHERE user_id = $1)
		DELETE FROM user_totp WHERE user_id = $1`, userID)
	return err
}

// ReplaceBackupCodes swaps the user's codes for the given hashes atomically.
func (s *Store) ReplaceBackupCodes(ctx context.Context, userID string, hashes []string) error {
	_, err := s.Q.Exec(ctx, `WITH d AS (DELETE FROM user_backup_codes WHERE user_id = $1)
		INSERT INTO user_backup_codes (user_id, code_hash) SELECT $1, unnest($2::text[])`, userID, hashes)
	return err
}

// UseBackupCode burns a code; false when unknown or already used.
func (s *Store) UseBackupCode(ctx context.Context, userID, hash string) (bool, error) {
	tag, err := s.Q.Exec(ctx, `UPDATE user_backup_codes SET used_at = now()
		WHERE user_id = $1 AND code_hash = $2 AND used_at IS NULL`, userID, hash)
	return tag.RowsAffected() == 1, err
}

func (s *Store) CreateLoginChallenge(ctx context.Context, tokenHash []byte, userID string, expires time.Time) error {
	_, err := s.Q.Exec(ctx, `INSERT INTO login_challenges (token_hash, user_id, expires_at) VALUES ($1, $2, $3)`,
		tokenHash, userID, expires)
	return err
}

// TryLoginChallenge spends one attempt and returns the owner; ErrNotFound
// when the challenge is unknown, expired or out of attempts.
func (s *Store) TryLoginChallenge(ctx context.Context, tokenHash []byte, maxAttempts int) (string, error) {
	var uid string
	err := s.Q.QueryRow(ctx, `UPDATE login_challenges SET attempts = attempts + 1
		WHERE token_hash = $1 AND expires_at > now() AND attempts < $2
		RETURNING user_id::text`, tokenHash, maxAttempts).Scan(&uid)
	if err == pgx.ErrNoRows {
		return "", ErrNotFound
	}
	return uid, err
}

func (s *Store) DeleteLoginChallenge(ctx context.Context, tokenHash []byte) error {
	_, err := s.Q.Exec(ctx, `DELETE FROM login_challenges WHERE token_hash = $1`, tokenHash)
	return err
}

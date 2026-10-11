package store

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
)

// PasskeyRow is user_passkeys; Credential is the go-webauthn Credential JSON.
type PasskeyRow struct {
	ID           string
	UserID       string
	CredentialID []byte
	Credential   []byte
	Name         string
	CreatedAt    time.Time
	LastUsedAt   *time.Time
}

const passkeyCols = `id, user_id, credential_id, credential, name, created_at, last_used_at`

func scanPasskey(row pgx.Row) (PasskeyRow, error) {
	var p PasskeyRow
	err := row.Scan(&p.ID, &p.UserID, &p.CredentialID, &p.Credential, &p.Name, &p.CreatedAt, &p.LastUsedAt)
	if err == pgx.ErrNoRows {
		err = ErrNotFound
	}
	return p, err
}

func (s *Store) CreatePasskey(ctx context.Context, p *PasskeyRow) error {
	return s.Q.QueryRow(ctx, `
		INSERT INTO user_passkeys (id, user_id, credential_id, credential, name)
		VALUES ($1, $2, $3, $4, $5) RETURNING created_at`,
		p.ID, p.UserID, p.CredentialID, p.Credential, p.Name).Scan(&p.CreatedAt)
}

func (s *Store) PasskeysByUser(ctx context.Context, userID string) ([]PasskeyRow, error) {
	rows, err := s.Q.Query(ctx, `SELECT `+passkeyCols+` FROM user_passkeys WHERE user_id = $1 ORDER BY created_at`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PasskeyRow
	for rows.Next() {
		p, err := scanPasskey(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *Store) PasskeyByCredentialID(ctx context.Context, credID []byte) (PasskeyRow, error) {
	return scanPasskey(s.Q.QueryRow(ctx, `SELECT `+passkeyCols+` FROM user_passkeys WHERE credential_id = $1`, credID))
}

// UpdatePasskeyCredential stores the post-login credential (sign count, flags).
func (s *Store) UpdatePasskeyCredential(ctx context.Context, id string, credential []byte) error {
	_, err := s.Q.Exec(ctx, `UPDATE user_passkeys SET credential = $2, last_used_at = now() WHERE id = $1`, id, credential)
	return err
}

// RenamePasskey is owner-scoped: ErrNotFound for someone else's or unknown id.
func (s *Store) RenamePasskey(ctx context.Context, userID, id, name string) (PasskeyRow, error) {
	return scanPasskey(s.Q.QueryRow(ctx, `UPDATE user_passkeys SET name = $3 WHERE id = $1 AND user_id = $2 RETURNING `+passkeyCols,
		id, userID, name))
}

// DeletePasskey is owner-scoped: ErrNotFound for someone else's or unknown id.
func (s *Store) DeletePasskey(ctx context.Context, userID, id string) error {
	tag, err := s.Q.Exec(ctx, `DELETE FROM user_passkeys WHERE id = $1 AND user_id = $2`, id, userID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteUserPasskeys drops every passkey of a user (CLI) and reports how many.
func (s *Store) DeleteUserPasskeys(ctx context.Context, userID string) (int64, error) {
	tag, err := s.Q.Exec(ctx, `DELETE FROM user_passkeys WHERE user_id = $1`, userID)
	return tag.RowsAffected(), err
}

// CreateCeremony stores WebAuthn session state; expired rows are swept here.
func (s *Store) CreateCeremony(ctx context.Context, tokenHash []byte, kind string, userID *string, session []byte, expires time.Time) error {
	if _, err := s.Q.Exec(ctx, `DELETE FROM webauthn_ceremonies WHERE expires_at < now()`); err != nil {
		return err
	}
	_, err := s.Q.Exec(ctx, `INSERT INTO webauthn_ceremonies (token_hash, kind, user_id, session_data, expires_at)
		VALUES ($1, $2, $3, $4, $5)`, tokenHash, kind, userID, session, expires)
	return err
}

// TakeCeremony consumes a ceremony exactly once; ErrNotFound when unknown,
// expired, already used, or of another kind.
func (s *Store) TakeCeremony(ctx context.Context, tokenHash []byte, kind string) (userID *string, session []byte, err error) {
	err = s.Q.QueryRow(ctx, `DELETE FROM webauthn_ceremonies
		WHERE token_hash = $1 AND kind = $2 AND expires_at > now()
		RETURNING user_id, session_data`, tokenHash, kind).Scan(&userID, &session)
	if err == pgx.ErrNoRows {
		err = ErrNotFound
	}
	return
}

func (s *Store) CreateHandoff(ctx context.Context, codeHash []byte, userID, challenge string, expires time.Time) error {
	if _, err := s.Q.Exec(ctx, `DELETE FROM mobile_handoffs WHERE expires_at < now()`); err != nil {
		return err
	}
	_, err := s.Q.Exec(ctx, `INSERT INTO mobile_handoffs (code_hash, user_id, code_challenge, expires_at)
		VALUES ($1, $2, $3, $4)`, codeHash, userID, challenge, expires)
	return err
}

// TakeHandoff consumes a handoff code exactly once.
func (s *Store) TakeHandoff(ctx context.Context, codeHash []byte) (userID, challenge string, err error) {
	err = s.Q.QueryRow(ctx, `DELETE FROM mobile_handoffs WHERE code_hash = $1 AND expires_at > now()
		RETURNING user_id, code_challenge`, codeHash).Scan(&userID, &challenge)
	if err == pgx.ErrNoRows {
		err = ErrNotFound
	}
	return
}

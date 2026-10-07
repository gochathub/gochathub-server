package store

import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// UserRow is the users table shape.
type UserRow struct {
	ID                 string
	Username           string
	DisplayName        string
	Email              *string
	PasswordHash       string
	Role               string
	Enabled            bool
	AvatarAttachmentID *string
	Timezone           *string // IANA name; validity checked on write
	Preferences        []byte  // raw jsonb
	CreatedAt          time.Time
	UpdatedAt          time.Time
	LastSeenAt         *time.Time
}

const userCols = `u.id, u.username, u.display_name, u.email, u.password_hash, u.role::text,
	u.enabled, u.avatar_attachment_id::text, u.timezone, u.preferences, u.created_at, u.updated_at, u.last_seen_at`

func scanUser(row pgx.Row) (UserRow, error) {
	var u UserRow
	err := row.Scan(&u.ID, &u.Username, &u.DisplayName, &u.Email, &u.PasswordHash, &u.Role,
		&u.Enabled, &u.AvatarAttachmentID, &u.Timezone, &u.Preferences, &u.CreatedAt, &u.UpdatedAt, &u.LastSeenAt)
	return u, err
}

// InsertUser creates an account. Username normalization is the caller's job.
func (s *Store) InsertUser(ctx context.Context, u *UserRow) error {
	err := s.Q.QueryRow(ctx, `
		INSERT INTO users (id, username, display_name, email, password_hash, role, preferences, timezone)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		RETURNING created_at, updated_at`,
		u.ID, u.Username, u.DisplayName, u.Email, u.PasswordHash, u.Role, string(u.Preferences), u.Timezone,
	).Scan(&u.CreatedAt, &u.UpdatedAt)
	return err
}

// UsersByIDs is the batched author lookup for message pages.
func (s *Store) UsersByIDs(ctx context.Context, ids []string) ([]UserRow, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	rows, err := s.Q.Query(ctx, `SELECT `+userCols+` FROM users u WHERE u.id::text = ANY($1)`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []UserRow
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

func (s *Store) UserByID(ctx context.Context, id string) (UserRow, error) {
	return scanUser(s.Q.QueryRow(ctx, `SELECT `+userCols+` FROM users u WHERE u.id = $1`, id))
}

func (s *Store) UserByUsername(ctx context.Context, username string) (UserRow, error) {
	return scanUser(s.Q.QueryRow(ctx, `SELECT `+userCols+` FROM users u WHERE u.username = $1`, strings.ToLower(username)))
}

// SearchUsers finds enabled users by username or display-name substring.
func (s *Store) SearchUsers(ctx context.Context, q string, limit int) ([]UserRow, error) {
	rows, err := s.Q.Query(ctx, `
		SELECT `+userCols+` FROM users u
		WHERE u.enabled AND (u.username LIKE $1 ESCAPE '\' OR u.display_name LIKE $1 ESCAPE '\')
		ORDER BY u.username LIMIT $2`,
		"%"+escapeLike(q)+"%", limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []UserRow
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// UserPatch carries PATCH /users/me fields. Pointer fields: nil means "keep"
// except Set booleans mark explicit null-carrying fields.
type UserPatch struct {
	DisplayName        *string // set when non-nil
	Email              *string // set when non-nil (nil value + Set=true clears)
	EmailSet           bool
	Timezone           *string // IANA name; nil value + Set=true clears
	TimezoneSet        bool
	AvatarAttachmentID *string
	AvatarSet          bool
}

// UpdateUserSelf applies the patch; SQL is built field by field so "keep"
// and "clear" stay distinguishable.
func (s *Store) UpdateUserSelf(ctx context.Context, id string, p UserPatch) (UserRow, error) {
	var sets []string
	args := []any{id}
	add := func(col string, v any) {
		sets = append(sets, col+" = $"+strconv.Itoa(len(args)+1))
		args = append(args, v)
	}
	if p.DisplayName != nil {
		add("display_name", *p.DisplayName)
	}
	if p.EmailSet {
		add("email", p.Email)
	}
	if p.AvatarSet {
		add("avatar_attachment_id", p.AvatarAttachmentID)
	}
	if p.TimezoneSet {
		add("timezone", p.Timezone)
	}
	sets = append(sets, "updated_at = now()")
	// alias u keeps the shared userCols RETURNING list valid
	q := `UPDATE users AS u SET ` + strings.Join(sets, ", ") + ` WHERE u.id = $1 RETURNING ` + userCols
	return scanUser(s.Q.QueryRow(ctx, q, args...))
}

// TouchLastSeen updates last_seen_at; visibility gating happens at read.
func (s *Store) TouchLastSeen(ctx context.Context, id string) error {
	_, err := s.Q.Exec(ctx, `UPDATE users SET last_seen_at = now() WHERE id = $1`, id)
	return err
}

// UpdateUserPassword replaces the stored password hash.
func (s *Store) UpdateUserPassword(ctx context.Context, id, hash string) error {
	_, err := s.Q.Exec(ctx, `
		UPDATE users SET password_hash = $2, updated_at = now() WHERE id = $1`, id, hash)
	return err
}

func (s *Store) SetUserEnabled(ctx context.Context, id string, enabled bool) error {
	_, err := s.Q.Exec(ctx, `
		UPDATE users SET enabled = $2, updated_at = now() WHERE id = $1`, id, enabled)
	return err
}

// DeleteUser hard-deletes the account row; dependent rows cascade.
func (s *Store) DeleteUser(ctx context.Context, id string) error {
	_, err := s.Q.Exec(ctx, `DELETE FROM users WHERE id = $1`, id)
	return err
}

func (s *Store) ListUsers(ctx context.Context) ([]UserRow, error) {
	rows, err := s.Q.Query(ctx, `SELECT `+userCols+` FROM users u ORDER BY u.username`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []UserRow
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// SetUserPreferences stores merged prefs JSON.
func (s *Store) SetUserPreferences(ctx context.Context, id string, prefsJSON []byte) error {
	_, err := s.Q.Exec(ctx, `
		UPDATE users SET preferences = $2::jsonb, updated_at = now() WHERE id = $1`, id, string(prefsJSON))
	return err
}

// UserVisibleTo reports whether two users share a room (`GET /users/{id}`
// visibility rule — no enumeration of strangers).
func (s *Store) UserVisibleTo(ctx context.Context, targetID, callerID string) (bool, error) {
	var ok bool
	err := s.Q.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1
			FROM room_members a JOIN room_members b ON a.room_id = b.room_id
			WHERE a.user_id = $1 AND b.user_id = $2
		) OR EXISTS (
			SELECT 1 FROM contacts WHERE (owner_id = $1 AND contact_user_id = $2)
			   OR (owner_id = $2 AND contact_user_id = $1)
		) OR EXISTS (SELECT 1 FROM room_invites ri
			WHERE ri.status = 'pending' AND ri.room_id IN (
				SELECT room_id FROM room_members WHERE user_id = $2)
			AND ri.invitee_id = $1)`,
		targetID, callerID).Scan(&ok)
	return ok, err
}

func escapeLike(s string) string {
	return strings.NewReplacer("\\", "\\\\", "%", "\\%", "_", "\\_").Replace(s)
}

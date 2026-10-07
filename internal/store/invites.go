package store

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
)

// InviteRow is the room_invites shape plus display fields.
type InviteRow struct {
	ID          string
	RoomID      string
	RoomName    *string
	RoomType    string
	InviterID   string
	InviterName string
	InviteeID   string
	InviteeName string
	Status      string
	ExpiresAt   *time.Time
	CreatedAt   time.Time
	AcceptedAt  *time.Time
}

func (s *Store) CreateInvite(ctx context.Context, i *InviteRow) error {
	// upsert against the partial open-invite index: re-inviting refreshes
	// the pending row (new inviter/expiry) instead of deadlocking the slot
	// behind an invite that already expired but cannot be listed/accepted
	return s.Q.QueryRow(ctx, `
		INSERT INTO room_invites (id, room_id, inviter_id, invitee_id, expires_at)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (room_id, invitee_id) WHERE status = 'pending'
		DO UPDATE SET inviter_id = EXCLUDED.inviter_id, expires_at = EXCLUDED.expires_at
		RETURNING id, created_at`,
		i.ID, i.RoomID, i.InviterID, i.InviteeID, i.ExpiresAt).Scan(&i.ID, &i.CreatedAt)
	// on conflict the pending row keeps its original id: the caller (and the
	// notification) must address the live invite, not the discarded new id
}

// inviteQuery selects the display-joined invite shape; WHERE/ORDER clauses
// are appended per lookup (same sync discipline as userCols).
const inviteQuery = `SELECT i.id, i.room_id, r.name, r.type::text,
	i.inviter_id, inv.display_name,
	i.invitee_id, e.display_name,
	i.status::text, i.expires_at, i.created_at, i.accepted_at
FROM room_invites i
JOIN rooms r ON r.id = i.room_id
JOIN users inv ON inv.id = i.inviter_id
JOIN users e ON e.id = i.invitee_id
`

// InviteForUser returns the invite only when the given user is invitee or
// inviter — non-participants get ErrNotFound.
func (s *Store) InviteForUser(ctx context.Context, inviteID, userID string) (InviteRow, error) {
	return scanInvite(s.Q.QueryRow(ctx, inviteQuery+
		`WHERE i.id = $1 AND (i.invitee_id = $2 OR i.inviter_id = $2)`,
		inviteID, userID))
}

func scanInvite(row pgx.Row) (InviteRow, error) {
	var i InviteRow
	err := row.Scan(&i.ID, &i.RoomID, &i.RoomName, &i.RoomType,
		&i.InviterID, &i.InviterName, &i.InviteeID, &i.InviteeName,
		&i.Status, &i.ExpiresAt, &i.CreatedAt, &i.AcceptedAt)
	return i, err
}

// UpdateInviteStatus moves a pending invite to its new status. Status values
// are controlled constants from the service layer. Returns ErrNotFound when
// the invite is absent or already closed.
func (s *Store) UpdateInviteStatus(ctx context.Context, inviteID string, status string) error {
	tag, err := s.Q.Exec(ctx, `
		UPDATE room_invites
		SET status = $2::invite_status,
		    accepted_at = CASE WHEN $2 = 'accepted' THEN now() ELSE accepted_at END
		WHERE id = $1 AND status = 'pending'`,
		inviteID, status)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) InvitesForUser(ctx context.Context, userID string) ([]InviteRow, error) {
	rows, err := s.Q.Query(ctx, inviteQuery+
		`WHERE i.invitee_id = $1 AND i.status = 'pending'
		AND (i.expires_at IS NULL OR i.expires_at > now())
		ORDER BY i.created_at DESC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []InviteRow
	for rows.Next() {
		i, err := scanInvite(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, i)
	}
	return out, rows.Err()
}

func (s *Store) InvitesForRoom(ctx context.Context, roomID string) ([]InviteRow, error) {
	rows, err := s.Q.Query(ctx, inviteQuery+
		`WHERE i.room_id = $1 AND i.status = 'pending'
		ORDER BY i.created_at DESC`, roomID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []InviteRow
	for rows.Next() {
		i, err := scanInvite(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, i)
	}
	return out, rows.Err()
}

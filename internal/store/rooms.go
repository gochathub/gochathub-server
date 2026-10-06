package store

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
)

// RoomRow is the rooms table shape.
type RoomRow struct {
	ID                 string
	Type               string
	Name               *string
	Description        string
	AvatarAttachmentID *string
	CreatedBy          string
	PinnedMessageID    *string
	ArchivedAt         *time.Time
	CreatedAt          time.Time
	UpdatedAt          time.Time
	// Archived is the caller's member-level conversation archive flag; only
	// room queries that join room_members carry it.
	Archived bool
}

const roomCols = `r.id, r.type::text, r.name, r.description, r.avatar_attachment_id::text,
	r.created_by, r.pinned_message_id::text, r.archived_at, r.created_at, r.updated_at`

func scanRoom(row pgx.Row) (RoomRow, error) {
	var r RoomRow
	err := row.Scan(&r.ID, &r.Type, &r.Name, &r.Description, &r.AvatarAttachmentID,
		&r.CreatedBy, &r.PinnedMessageID, &r.ArchivedAt, &r.CreatedAt, &r.UpdatedAt)
	return r, err
}

// CreateRoom inserts the room and the creator as admin member.
func (s *Store) CreateRoom(ctx context.Context, r *RoomRow, creatorAdmin bool) error {
	err := s.WithTx(ctx, func(tx *Store) error {
		if err := tx.Q.QueryRow(ctx, `
			INSERT INTO rooms (id, type, name, description, avatar_attachment_id, created_by)
			VALUES ($1, $2, $3, $4, $5, $6)
			RETURNING created_at, updated_at`,
			r.ID, r.Type, r.Name, r.Description, r.AvatarAttachmentID, r.CreatedBy,
		).Scan(&r.CreatedAt, &r.UpdatedAt); err != nil {
			return err
		}
		if creatorAdmin {
			role := "admin"
			_, err := tx.Q.Exec(ctx, `
				INSERT INTO room_members (room_id, user_id, role) VALUES ($1, $2, $3)`,
				r.ID, r.CreatedBy, role)
			if err != nil {
				return err
			}
		}
		return nil
	})
	return err
}

// RoomByID returns any room regardless of membership (join-eligible checks
// only; private content authorization stays with RoomForUser).
func (s *Store) RoomByID(ctx context.Context, roomID string) (RoomRow, error) {
	return scanRoom(s.Q.QueryRow(ctx, `SELECT `+roomCols+` FROM rooms r WHERE r.id = $1`, roomID))
}

// ListAllRooms returns every room (CLI administration view).
func (s *Store) ListAllRooms(ctx context.Context) ([]RoomRow, error) {
	rows, err := s.Q.Query(ctx, `SELECT `+roomCols+` FROM rooms r ORDER BY r.created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RoomRow
	for rows.Next() {
		r, err := scanRoom(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// RoomForUser returns the room only when the user is a member. Rooms are
// private content; non-members get ErrNotFound (no existence leak for
// private rooms).
func (s *Store) RoomForUser(ctx context.Context, roomID, userID string) (RoomRow, string, error) {
	var r RoomRow
	var role string
	const q = `SELECT r.id, r.type::text, r.name, r.description, r.avatar_attachment_id::text,
		r.created_by, r.pinned_message_id::text, r.archived_at, r.created_at, r.updated_at,
		m.role::text, m.archived
		FROM rooms r JOIN room_members m ON m.room_id = r.id AND m.user_id = $2
		WHERE r.id = $1`
	err := s.Q.QueryRow(ctx, q, roomID, userID).Scan(
		&r.ID, &r.Type, &r.Name, &r.Description, &r.AvatarAttachmentID,
		&r.CreatedBy, &r.PinnedMessageID, &r.ArchivedAt, &r.CreatedAt, &r.UpdatedAt, &role, &r.Archived)
	return r, role, err
}

// RoomSummaryRow is a joined room list row with unread counts.
type RoomSummaryRow struct {
	RoomRow
	MyRole      string
	UnreadCount int64
	LastReadID  *string
	MemberCount int64
}

// ListRoomsForUser returns the caller's rooms (archived included; the client
// filters). Unread counts come from receipts not yet read.
func (s *Store) ListRoomsForUser(ctx context.Context, userID string) ([]RoomSummaryRow, error) {
	rows, err := s.Q.Query(ctx, `
		SELECT r.id, r.type::text, r.name, r.description, r.avatar_attachment_id::text,
			r.created_by, r.pinned_message_id::text, r.archived_at, r.created_at, r.updated_at,
			m.role::text, m.archived,
			COALESCE(rc.unread, 0),
			m.last_read_message_id::text,
			mc.member_count
		FROM room_members m
		JOIN rooms r ON r.id = m.room_id
		LEFT JOIN LATERAL (
			SELECT count(*) AS unread
			FROM message_receipts mr
			JOIN messages ms ON ms.id = mr.message_id AND ms.room_id = m.room_id AND ms.deleted_at IS NULL
			WHERE mr.user_id = m.user_id AND mr.read_at IS NULL
		) rc ON true
		LEFT JOIN LATERAL (
			SELECT count(*) AS member_count FROM room_members x WHERE x.room_id = r.id
		) mc ON true
		WHERE m.user_id = $1
		ORDER BY r.updated_at DESC`,
		userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RoomSummaryRow
	for rows.Next() {
		var rr RoomSummaryRow
		if err := rows.Scan(&rr.ID, &rr.Type, &rr.Name, &rr.Description, &rr.AvatarAttachmentID,
			&rr.CreatedBy, &rr.PinnedMessageID, &rr.ArchivedAt, &rr.CreatedAt, &rr.UpdatedAt,
			&rr.MyRole, &rr.Archived, &rr.UnreadCount, &rr.LastReadID, &rr.MemberCount); err != nil {
			return nil, err
		}
		out = append(out, rr)
	}
	return out, rows.Err()
}

// UpdateRoom patches name/description/avatar. nils keep.
func (s *Store) UpdateRoom(ctx context.Context, roomID string, name *string, description *string, avatar *string) (RoomRow, error) {
	return scanRoom(s.Q.QueryRow(ctx, `
		UPDATE rooms AS r SET
			name = COALESCE($2, name),
			description = COALESCE($3, description),
			avatar_attachment_id = $4,
			updated_at = now()
		WHERE r.id = $1
		RETURNING `+roomCols,
		roomID, name, description, avatar))
}

// SetPinnedMessage pins (messageID non-nil) or unpins (nil).
func (s *Store) SetPinnedMessage(ctx context.Context, roomID string, messageID *string) error {
	_, err := s.Q.Exec(ctx, `
		UPDATE rooms SET pinned_message_id = $2, updated_at = now() WHERE id = $1`,
		roomID, messageID)
	return err
}

func (s *Store) ArchiveRoom(ctx context.Context, roomID string) error {
	_, err := s.Q.Exec(ctx, `UPDATE rooms SET archived_at = now(), updated_at = now() WHERE id = $1 AND archived_at IS NULL`, roomID)
	return err
}

type MemberRow struct {
	RoomID   string
	UserID   string
	Role     string
	JoinedAt time.Time
	// user fields for convenience
	Username    string
	DisplayName string
	AvatarAtt   *string
	LastSeen    *time.Time
}

func (s *Store) RoomMembers(ctx context.Context, roomID string) ([]MemberRow, error) {
	rows, err := s.Q.Query(ctx, `
		SELECT m.room_id, m.user_id, m.role::text, m.joined_at,
			u.username, u.display_name, u.avatar_attachment_id::text, u.last_seen_at
		FROM room_members m JOIN users u ON u.id = m.user_id
		WHERE m.room_id = $1 ORDER BY m.role, u.username`, roomID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MemberRow
	for rows.Next() {
		var m MemberRow
		if err := rows.Scan(&m.RoomID, &m.UserID, &m.Role, &m.JoinedAt,
			&m.Username, &m.DisplayName, &m.AvatarAtt, &m.LastSeen); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// MemberRole returns ("", ErrNotFound) when not a member.
func (s *Store) MemberRole(ctx context.Context, roomID, userID string) (string, error) {
	var role string
	err := s.Q.QueryRow(ctx,
		`SELECT role::text FROM room_members WHERE room_id = $1 AND user_id = $2`,
		roomID, userID).Scan(&role)
	return role, err
}

func (s *Store) AddMember(ctx context.Context, roomID, userID, role string) error {
	_, err := s.Q.Exec(ctx,
		`INSERT INTO room_members (room_id, user_id, role) VALUES ($1, $2, $3)
		 ON CONFLICT (room_id, user_id) DO UPDATE SET role = $3`,
		roomID, userID, role)
	return err
}

func (s *Store) RemoveMember(ctx context.Context, roomID, userID string) error {
	_, err := s.Q.Exec(ctx, `DELETE FROM room_members WHERE room_id = $1 AND user_id = $2`, roomID, userID)
	return err
}

// SetReadCursor advances the room read cursor when it moves forward.
func (s *Store) SetReadCursor(ctx context.Context, roomID, userID, messageID string) error {
	_, err := s.Q.Exec(ctx, `
		UPDATE room_members SET last_read_message_id = $3
		WHERE room_id = $1 AND user_id = $2
		  AND (last_read_message_id IS NULL
		       OR last_read_message_id <> $3)`,
		roomID, userID, messageID)
	return err
}

// GetDirectRoomBetween returns the existing direct room shared by both
// users, or ErrNotFound. Ensures one DM per peer pair.
func (s *Store) GetDirectRoomBetween(ctx context.Context, userA, userB string) (string, error) {
	var id string
	err := s.Q.QueryRow(ctx, `
		SELECT r.id FROM rooms r
		JOIN room_members a ON a.room_id = r.id AND a.user_id = $1
		JOIN room_members b ON b.room_id = r.id AND b.user_id = $2
		WHERE r.type = 'direct' AND r.archived_at IS NULL
		LIMIT 1`, userA, userB).Scan(&id)
	if err != nil {
		return "", ErrNotFound
	}
	return id, nil
}

func (s *Store) SetMemberArchived(ctx context.Context, roomID, userID string, archived bool) error {
	_, err := s.Q.Exec(ctx, `UPDATE room_members SET archived = $3 WHERE room_id = $1 AND user_id = $2`, roomID, userID, archived)
	return err
}

// RoomMemberIDs returns member ids plus the room type for push fan-out.
func (s *Store) RoomMemberIDs(ctx context.Context, roomID string) ([]string, string, error) {
	rows, err := s.Q.Query(ctx, `
		SELECT rm.user_id::text, r.type::text FROM room_members rm JOIN rooms r ON r.id = rm.room_id
		WHERE rm.room_id = $1`, roomID)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()
	var ids []string
	var roomType string
	for rows.Next() {
		var id, t string
		if err := rows.Scan(&id, &t); err != nil {
			return nil, "", err
		}
		ids = append(ids, id)
		roomType = t
	}
	return ids, roomType, rows.Err()
}

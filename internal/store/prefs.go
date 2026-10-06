package store

import (
	"context"
)

// Notification preference rows: room-specific override plus one default row
// (room_id IS NULL), mode enum 'all'|'mentions'|'directs'|'never'.
func (s *Store) SetNotifMode(ctx context.Context, userID string, roomID *string, mode string) error {
	if roomID == nil {
		_, err := s.Q.Exec(ctx, `
			INSERT INTO notification_preferences (user_id, room_id, mode)
			VALUES ($1, NULL, $2::notif_mode)
			ON CONFLICT (user_id) WHERE room_id IS NULL
			DO UPDATE SET mode = $2::notif_mode`,
			userID, mode)
		return err
	}
	_, err := s.Q.Exec(ctx, `
		INSERT INTO notification_preferences (user_id, room_id, mode)
		VALUES ($1, $2::uuid, $3::notif_mode)
		ON CONFLICT (user_id, room_id) DO UPDATE SET mode = $3::notif_mode`,
		userID, *roomID, mode)
	return err
}

// NotifMode returns the room-specific mode when a row exists for that room,
// else the user's default row. Absent row returns ErrNotFound; the service
// applies its own fallback.
func (s *Store) NotifMode(ctx context.Context, userID string, roomID *string) (string, error) {
	var mode string
	err := s.Q.QueryRow(ctx, `
		SELECT mode::text FROM notification_preferences
		WHERE user_id = $1 AND room_id IS NOT DISTINCT FROM $2::uuid`,
		userID, roomID).Scan(&mode)
	return mode, err
}

// MemberNotifModeWithDefault resolves the effective mode for a room:
// room-specific row when present, else the user's default row.
func (s *Store) MemberNotifModeWithDefault(ctx context.Context, userID, roomID string) (string, error) {
	mode, err := s.NotifMode(ctx, userID, &roomID)
	if err == nil {
		return mode, nil
	}
	if err == ErrNotFound {
		return s.NotifMode(ctx, userID, nil)
	}
	return "", err
}

func (s *Store) ListNotifModes(ctx context.Context, userID string) ([]NotifModeRow, error) {
	rows, err := s.Q.Query(ctx, `
		SELECT COALESCE(room_id::text, ''), mode::text
		FROM notification_preferences WHERE user_id = $1`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []NotifModeRow
	for rows.Next() {
		var r NotifModeRow
		if err := rows.Scan(&r.RoomID, &r.Mode); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

type NotifModeRow struct {
	RoomID string `json:"room_id"` // "" = default
	Mode   string `json:"mode"`
}

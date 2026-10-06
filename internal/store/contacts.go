package store

import (
	"context"
	"time"
)

// ContactRow is the contacts shape with the contact user's display fields.
type ContactRow struct {
	OwnerID     string
	ContactID   string
	CreatedAt   time.Time
	Username    string
	DisplayName string
	AvatarAtt   *string
	LastSeen    *time.Time
}

func (s *Store) AddContact(ctx context.Context, ownerID, contactUserID string) (ContactRow, error) {
	var c ContactRow
	err := s.Q.QueryRow(ctx, `
		INSERT INTO contacts (owner_id, contact_user_id) VALUES ($1, $2)
		RETURNING created_at`, ownerID, contactUserID).Scan(&c.CreatedAt)
	c.OwnerID, c.ContactID = ownerID, contactUserID
	return c, err
}

func (s *Store) RemoveContact(ctx context.Context, ownerID, contactUserID string) error {
	_, err := s.Q.Exec(ctx, `DELETE FROM contacts WHERE owner_id = $1 AND contact_user_id = $2`, ownerID, contactUserID)
	return err
}

func (s *Store) IsContact(ctx context.Context, ownerID, contactUserID string) (bool, error) {
	var ok bool
	err := s.Q.QueryRow(ctx, `SELECT EXISTS(
		SELECT 1 FROM contacts WHERE owner_id = $1 AND contact_user_id = $2)`,
		ownerID, contactUserID).Scan(&ok)
	return ok, err
}

func (s *Store) ListContacts(ctx context.Context, ownerID string) ([]ContactRow, error) {
	rows, err := s.Q.Query(ctx, `
		SELECT c.owner_id::text, c.contact_user_id::text, c.created_at,
			u.username, u.display_name, u.avatar_attachment_id::text, u.last_seen_at
		FROM contacts c JOIN users u ON u.id = c.contact_user_id AND u.enabled
		WHERE c.owner_id = $1 ORDER BY u.username`, ownerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ContactRow
	for rows.Next() {
		var c ContactRow
		if err := rows.Scan(&c.OwnerID, &c.ContactID, &c.CreatedAt,
			&c.Username, &c.DisplayName, &c.AvatarAtt, &c.LastSeen); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

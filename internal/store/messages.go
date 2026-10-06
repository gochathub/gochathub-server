package store

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
)

// MessageRow is the messages table shape.
type MessageRow struct {
	ID               string
	RoomID           string
	AuthorID         string
	ReplyToMessageID *string
	Body             string
	Format           string
	CreatedAt        time.Time
	EditedAt         *time.Time
	DeletedAt        *time.Time
}

const messageCols = `m.id, m.room_id, m.author_id, m.reply_to_message_id::text,
	m.body, m.format::text, m.created_at, m.edited_at, m.deleted_at`

func scanMessage(row pgx.Row) (MessageRow, error) {
	var m MessageRow
	err := row.Scan(&m.ID, &m.RoomID, &m.AuthorID, &m.ReplyToMessageID,
		&m.Body, &m.Format, &m.CreatedAt, &m.EditedAt, &m.DeletedAt)
	return m, err
}

func (s *Store) InsertMessage(ctx context.Context, m *MessageRow) error {
	return s.Q.QueryRow(ctx, `
		INSERT INTO messages (id, room_id, author_id, reply_to_message_id, body, format)
		VALUES ($1, $2, $3, $4, $5, $6)
		RETURNING created_at`,
		m.ID, m.RoomID, m.AuthorID, m.ReplyToMessageID, m.Body, m.Format,
	).Scan(&m.CreatedAt)
}

// ReceiptRow mirrors message_receipts.
type ReceiptRow struct {
	MessageID   string
	UserID      string
	DeliveredAt *time.Time
	ReadAt      *time.Time
}

// SeedReceipts creates a receipts row for every listed member; the author's
// row is stamped read at creation. Returns member ids for the WS fan-out.
func (s *Store) SeedReceipts(ctx context.Context, messageID, roomID, authorID string) ([]string, error) {
	rows, err := s.Q.Query(ctx, `
		WITH rec AS (
			INSERT INTO message_receipts (message_id, user_id, delivered_at, read_at)
			SELECT $1, m.user_id, now(),
				CASE WHEN m.user_id = $3 THEN now() ELSE NULL END
			FROM room_members m WHERE m.room_id = $2
			RETURNING user_id::text
		) SELECT user_id FROM rec`, messageID, roomID, authorID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// MarkDelivered stamps delivered_at once.
func (s *Store) MarkDelivered(ctx context.Context, messageID, userID string) error {
	_, err := s.Q.Exec(ctx, `
		UPDATE message_receipts SET delivered_at = COALESCE(delivered_at, now())
		WHERE message_id = $1 AND user_id = $2`, messageID, userID)
	return err
}

// MarkRead stamps read_at (implies delivered) and advances the room cursor.
func (s *Store) MarkRead(ctx context.Context, messageID, userID string) error {
	return s.WithTx(ctx, func(tx *Store) error {
		if _, err := tx.Q.Exec(ctx, `
			UPDATE message_receipts
			SET delivered_at = COALESCE(delivered_at, now()), read_at = now()
			WHERE message_id = $1 AND user_id = $2`, messageID, userID); err != nil {
			return err
		}
		var roomID string
		if err := tx.Q.QueryRow(ctx, `SELECT room_id FROM messages WHERE id = $1`, messageID).Scan(&roomID); err != nil {
			return err
		}
		return tx.SetReadCursor(ctx, roomID, userID, messageID)
	})
}

// ReceiptsForUser returns the caller's receipts for a message, or absent.
func (s *Store) ReceiptForUser(ctx context.Context, messageID, userID string) (ReceiptRow, error) {
	var r ReceiptRow
	err := s.Q.QueryRow(ctx, `
		SELECT message_id::text, user_id::text, delivered_at, read_at
		FROM message_receipts WHERE message_id = $1 AND user_id = $2`,
		messageID, userID).Scan(&r.MessageID, &r.UserID, &r.DeliveredAt, &r.ReadAt)
	return r, err
}

// ReceiptAggregate computes sender-visible receipts for a message (ADR-009):
// delivered/read timestamps exist only after every current non-author member
// (who participates in receipts per their readReceipts preference, ADR-013)
// has the corresponding receipt stamped.
func (s *Store) ReceiptAggregate(ctx context.Context, messageID string) (deliveredAt, readAt *time.Time, err error) {
	err = s.Q.QueryRow(ctx, `
		WITH params AS (SELECT author_id, room_id FROM messages WHERE id = $1),
		rows_ AS (
			SELECT mr.delivered_at, mr.read_at
			FROM message_receipts mr
			JOIN room_members rm ON rm.room_id = (SELECT room_id FROM params)
				AND rm.user_id = mr.user_id
			-- receipt participation: stored true, or absent (default allow)
			JOIN users pu ON pu.id = mr.user_id
				AND (pu.preferences->'read_receipts' IS NULL
				     OR (pu.preferences->>'read_receipts')::boolean IS TRUE)
			WHERE mr.message_id = $1 AND mr.user_id <> (SELECT author_id FROM params)
		),
		expected AS (
			SELECT count(*) AS c FROM room_members rm
			JOIN users pu ON pu.id = rm.user_id
				AND (pu.preferences->'read_receipts' IS NULL
				     OR (pu.preferences->>'read_receipts')::boolean IS TRUE)
			WHERE rm.room_id = (SELECT room_id FROM params)
			  AND rm.user_id <> (SELECT author_id FROM params)
		)
		SELECT
			CASE WHEN (SELECT c FROM expected) = 0
			      OR (SELECT count(*) FROM rows_) < (SELECT c FROM expected)
			      OR EXISTS (SELECT 1 FROM rows_ WHERE delivered_at IS NULL)
			     THEN NULL ELSE (SELECT min(delivered_at) FROM rows_) END::timestamptz,
			CASE WHEN (SELECT c FROM expected) = 0
			      OR (SELECT count(*) FROM rows_) < (SELECT c FROM expected)
			      OR EXISTS (SELECT 1 FROM rows_ WHERE read_at IS NULL)
			     THEN NULL ELSE (SELECT min(read_at) FROM rows_) END::timestamptz`,
		messageID).Scan(&deliveredAt, &readAt)
	return deliveredAt, readAt, err
}

// StampReceiptsRead stamps everything up to a time as read (cursor catch-up).
func (s *Store) StampReceiptsRead(ctx context.Context, roomID, userID string, readTo time.Time) error {
	_, err := s.Q.Exec(ctx, `
		UPDATE message_receipts mr
		SET delivered_at = COALESCE(mr.delivered_at, now()), read_at = now()
		FROM messages m
		WHERE mr.user_id = $2 AND mr.read_at IS NULL
		  AND m.id = mr.message_id AND m.room_id = $1 AND m.created_at <= $3`,
		roomID, userID, readTo)
	return err
}

func (s *Store) MessageByID(ctx context.Context, id string) (MessageRow, error) {
	return scanMessage(s.Q.QueryRow(ctx, `SELECT `+messageCols+` FROM messages m WHERE m.id = $1`, id))
}

// ListMessagesPage paginates newest-first by (created_at, id). before nil
// means start from the newest message.
func (s *Store) ListMessagesPage(ctx context.Context, roomID string, before *Cursor, limit int) ([]MessageRow, error) {
	const q = `SELECT ` + messageCols + `
		FROM messages m
		WHERE m.room_id = $1
		  AND ($2::timestamptz IS NULL OR row(m.created_at, m.id) < ($2::timestamptz, $3::uuid))
		ORDER BY m.created_at DESC, m.id DESC
		LIMIT $4`
	var args []any
	if before == nil {
		args = []any{roomID, nil, nil, limit}
	} else {
		args = []any{roomID, &before.At, &before.ID, limit}
	}
	rows, err := s.Q.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MessageRow
	for rows.Next() {
		m, err := scanMessage(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// Cursor is a pagination key: (created_at, id).
type Cursor struct {
	At time.Time
	ID string
}

// EditMessage updates the body and records a revision with the given id.
func (s *Store) EditMessage(ctx context.Context, messageID, editorID, revisionID, body, format string) (MessageRow, error) {
	return scanMessage(s.Q.QueryRow(ctx, `
		WITH upd AS (
			UPDATE messages SET body = $2, format = $3, edited_at = now()
			WHERE id = $1 AND deleted_at IS NULL
			RETURNING id, room_id, author_id, reply_to_message_id::text,
				body, format::text, created_at, edited_at, deleted_at
		), rev AS (
			INSERT INTO message_revisions (id, message_id, editor_id, body, format)
			VALUES ($4, $1, $5, $2, $3)
		) SELECT * FROM upd`,
		messageID, body, format, revisionID, editorID))
}

// SoftDeleteMessage tombstones; bodies stay out of API responses.
func (s *Store) SoftDeleteMessage(ctx context.Context, messageID string) (MessageRow, error) {
	return scanMessage(s.Q.QueryRow(ctx, `
		UPDATE messages AS m SET deleted_at = now()
		WHERE m.id = $1 AND m.deleted_at IS NULL
		RETURNING `+messageCols, messageID))
}

// --- reactions ---

func (s *Store) AddReaction(ctx context.Context, messageID, userID, emoji string) error {
	_, err := s.Q.Exec(ctx, `
		INSERT INTO message_reactions (message_id, user_id, emoji)
		VALUES ($1, $2, $3)
		ON CONFLICT (message_id, user_id, emoji) DO NOTHING`,
		messageID, userID, emoji)
	return err
}

func (s *Store) RemoveReaction(ctx context.Context, messageID, userID, emoji string) (bool, error) {
	tag, err := s.Q.Exec(ctx, `
		DELETE FROM message_reactions WHERE message_id = $1 AND user_id = $2 AND emoji = $3`,
		messageID, userID, emoji)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// ReactionCounts aggregates emoji counts for a set of messages.
type ReactionCount struct {
	MessageID string
	Emoji     string
	Count     int64
}

func (s *Store) ReactionCountsFor(ctx context.Context, messageIDs []string) ([]ReactionCount, error) {
	if len(messageIDs) == 0 {
		return nil, nil
	}
	rows, err := s.Q.Query(ctx, `
		SELECT message_id::text, emoji, count(*)
		FROM message_reactions WHERE message_id = ANY($1)
		GROUP BY message_id, emoji ORDER BY message_id, emoji`, messageIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ReactionCount
	for rows.Next() {
		var rc ReactionCount
		if err := rows.Scan(&rc.MessageID, &rc.Emoji, &rc.Count); err != nil {
			return nil, err
		}
		out = append(out, rc)
	}
	return out, rows.Err()
}

// --- mentions ---

func (s *Store) AddMentions(ctx context.Context, messageID string, userIDs []string) error {
	if len(userIDs) == 0 {
		return nil
	}
	_, err := s.Q.Exec(ctx, `
		INSERT INTO message_mentions (message_id, mentioned_user_id)
		SELECT $1, x FROM unnest($2::uuid[]) AS t(x)
		ON CONFLICT DO NOTHING`, messageID, userIDs)
	return err
}

func (s *Store) MentionsForMessages(ctx context.Context, messageIDs []string) (map[string][]string, error) {
	if len(messageIDs) == 0 {
		return nil, nil
	}
	rows, err := s.Q.Query(ctx, `
		SELECT message_id::text, mentioned_user_id::text
		FROM message_mentions WHERE message_id = ANY($1)`, messageIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]string{}
	for rows.Next() {
		var m, u string
		if err := rows.Scan(&m, &u); err != nil {
			return nil, err
		}
		out[m] = append(out[m], u)
	}
	return out, rows.Err()
}

// MessageUserIDsForUsers is a lookup used when parsing @mentions.
func (s *Store) UserIDsByUsernames(ctx context.Context, usernames []string) (map[string]string, error) {
	if len(usernames) == 0 {
		return map[string]string{}, nil
	}
	rows, err := s.Q.Query(ctx, `
		SELECT username, id::text FROM users WHERE username = ANY($1)`, usernames)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var u, i string
		if err := rows.Scan(&u, &i); err != nil {
			return nil, err
		}
		out[u] = i
	}
	return out, rows.Err()
}

// --- attachments links ---

func (s *Store) AttachMessagesTo(ctx context.Context, messageID string, attachmentIDs []string) error {
	if len(attachmentIDs) == 0 {
		return nil
	}
	_, err := s.Q.Exec(ctx, `
		INSERT INTO message_attachments (message_id, attachment_id, sort_order)
		SELECT $1, a, y FROM unnest($2::uuid[]) WITH ORDINALITY AS t(a, y)
		ON CONFLICT DO NOTHING`, messageID, attachmentIDs)
	return err
}

// MessageAttachmentRow joins the link with attachment fields.
type MessageAttachmentRow struct {
	MessageID string
	AttachmentRow
}

func (s *Store) AttachmentsForMessages(ctx context.Context, messageIDs []string) ([]MessageAttachmentRow, error) {
	if len(messageIDs) == 0 {
		return nil, nil
	}
	rows, err := s.Q.Query(ctx, `
		SELECT ma.message_id::text, a.id::text, a.storage_key, a.filename, a.mime_type,
			a.size_bytes, a.sha256, a.width, a.height, a.status::text, a.created_at, a.deleted_at
		FROM message_attachments ma
		JOIN attachments a ON a.id = ma.attachment_id AND a.deleted_at IS NULL
		WHERE ma.message_id = ANY($1)
		ORDER BY ma.message_id, ma.sort_order`, messageIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MessageAttachmentRow
	for rows.Next() {
		var m MessageAttachmentRow
		if err := rows.Scan(&m.MessageID, &m.AttachmentRow.ID, &m.AttachmentRow.StorageKey,
			&m.AttachmentRow.Filename, &m.AttachmentRow.MimeType, &m.AttachmentRow.SizeBytes,
			&m.AttachmentRow.SHA256, &m.AttachmentRow.Width, &m.AttachmentRow.Height,
			&m.AttachmentRow.Status, &m.AttachmentRow.CreatedAt, &m.AttachmentRow.DeletedAt); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

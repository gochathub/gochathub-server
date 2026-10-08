package store

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
)

// WebhookRow is the webhooks table shape.
type WebhookRow struct {
	ID           string
	Name         string
	BotUserID    string
	RoomID       string
	SecretHash   []byte
	AllowedCIDRs []string
	MaxSpamScore *float32
	Enabled      bool
	CreatedAt    time.Time
	RotatedAt    *time.Time
	LastUsedAt   *time.Time
}

const webhookCols = `w.id::text, w.name, w.bot_user_id::text, w.room_id::text, w.secret_hash,
	w.allowed_cidrs, w.max_spam_score, w.enabled, w.created_at, w.rotated_at, w.last_used_at`

func scanWebhook(row pgx.Row) (WebhookRow, error) {
	var w WebhookRow
	err := row.Scan(&w.ID, &w.Name, &w.BotUserID, &w.RoomID, &w.SecretHash,
		&w.AllowedCIDRs, &w.MaxSpamScore, &w.Enabled, &w.CreatedAt, &w.RotatedAt, &w.LastUsedAt)
	return w, err
}

func (s *Store) InsertWebhook(ctx context.Context, w *WebhookRow) error {
	return s.Q.QueryRow(ctx, `
		INSERT INTO webhooks (id, name, bot_user_id, room_id, secret_hash, allowed_cidrs, max_spam_score)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		RETURNING created_at`,
		w.ID, w.Name, w.BotUserID, w.RoomID, w.SecretHash, w.AllowedCIDRs, w.MaxSpamScore,
	).Scan(&w.CreatedAt)
}

func (s *Store) WebhookByID(ctx context.Context, id string) (WebhookRow, error) {
	return scanWebhook(s.Q.QueryRow(ctx, `SELECT `+webhookCols+` FROM webhooks w WHERE w.id = $1`, id))
}

func (s *Store) ListWebhooks(ctx context.Context) ([]WebhookRow, error) {
	rows, err := s.Q.Query(ctx, `SELECT `+webhookCols+` FROM webhooks w ORDER BY w.created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []WebhookRow
	for rows.Next() {
		w, err := scanWebhook(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// SetWebhookSecret replaces the secret hash (rotation); ErrNotFound if unknown.
func (s *Store) SetWebhookSecret(ctx context.Context, id string, hash []byte) error {
	tag, err := s.Q.Exec(ctx, `UPDATE webhooks SET secret_hash = $2, rotated_at = now() WHERE id = $1`, id, hash)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) SetWebhookEnabled(ctx context.Context, id string, enabled bool) error {
	tag, err := s.Q.Exec(ctx, `UPDATE webhooks SET enabled = $2 WHERE id = $1`, id, enabled)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) DeleteWebhook(ctx context.Context, id string) error {
	tag, err := s.Q.Exec(ctx, `DELETE FROM webhooks WHERE id = $1`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) TouchWebhook(ctx context.Context, id string) error {
	_, err := s.Q.Exec(ctx, `UPDATE webhooks SET last_used_at = now() WHERE id = $1`, id)
	return err
}

// WebhookDelivery returns the message already created for a source id, or
// ErrNotFound.
func (s *Store) WebhookDelivery(ctx context.Context, webhookID, sourceID string) (string, error) {
	var msgID string
	err := s.Q.QueryRow(ctx,
		`SELECT message_id::text FROM webhook_deliveries WHERE webhook_id = $1 AND source_id = $2`,
		webhookID, sourceID).Scan(&msgID)
	return msgID, err
}

// InsertWebhookDelivery records the source id; IsUnique(err) means a concurrent
// delivery of the same source message won.
func (s *Store) InsertWebhookDelivery(ctx context.Context, webhookID, sourceID, messageID string) error {
	_, err := s.Q.Exec(ctx,
		`INSERT INTO webhook_deliveries (webhook_id, source_id, message_id) VALUES ($1, $2, $3)`,
		webhookID, sourceID, messageID)
	return err
}

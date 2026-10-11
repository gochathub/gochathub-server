package store

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// SrcRocketChat marks rooms mirrored from a live Rocket.Chat workspace
// (docs/ROCKETCHAT-INTEGRATION.md). Native rooms carry the empty string.
const SrcRocketChat = "rocket_chat"

// RCLink is one user's delegated Rocket.Chat credentials (rocket_chat_links).
// PatEnc is AES-GCM ciphertext; only the bridge decrypts it.
type RCLink struct {
	UserID     string
	BaseURL    string // the user's own workspace origin
	RCUserID   string
	RCUsername string
	PatEnc     []byte
	Watermark  *time.Time
	CreatedAt  time.Time
}

// InsertRCLink creates or replaces the caller's link (deleted rows revive;
// a re-link restarts the watermark from scratch).
func (s *Store) InsertRCLink(ctx context.Context, l *RCLink) error {
	return s.Q.QueryRow(ctx, `
		INSERT INTO rocket_chat_links (user_id, base_url, rc_user_id, rc_username, pat_enc, watermark)
		VALUES ($1, $2, $3, $4, $5, NULL)
		ON CONFLICT (user_id) DO UPDATE SET
			base_url = EXCLUDED.base_url, rc_user_id = EXCLUDED.rc_user_id,
			rc_username = EXCLUDED.rc_username, pat_enc = EXCLUDED.pat_enc,
			watermark = NULL, deleted_at = NULL
		RETURNING created_at`,
		l.UserID, l.BaseURL, l.RCUserID, l.RCUsername, l.PatEnc).Scan(&l.CreatedAt)
}

// SetRCLinkWatermark advances the subscriptions delta cursor.
func (s *Store) SetRCLinkWatermark(ctx context.Context, userID string, wm time.Time) error {
	_, err := s.Q.Exec(ctx, `UPDATE rocket_chat_links SET watermark = $2 WHERE user_id = $1`, userID, wm)
	return err
}

// DeleteRCLink is unlink: the PAT ciphertext is destroyed and the user's
// mirrored rooms are archived (hidden from every list; history stays so a
// re-link brings the same rooms back).
func (s *Store) DeleteRCLink(ctx context.Context, userID string) error {
	return s.WithTx(ctx, func(tx *Store) error {
		if _, err := tx.Q.Exec(ctx, `DELETE FROM rocket_chat_links WHERE user_id = $1`, userID); err != nil {
			return err
		}
		_, err := tx.Q.Exec(ctx, `
			UPDATE rooms SET archived_at = now(), updated_at = now()
			WHERE created_by = $1 AND source = $2 AND archived_at IS NULL`, userID, SrcRocketChat)
		return err
	})
}

// linkSel is the full SELECT for a rocket_chat_links row under `alias`.
func linkSel(alias string) string {
	return `SELECT ` + alias + `.user_id, ` + alias + `.base_url, ` + alias + `.rc_user_id, ` +
		alias + `.rc_username, ` + alias + `.pat_enc, ` + alias + `.watermark, ` + alias + `.created_at
		FROM rocket_chat_links ` + alias
}

// RCLinkByUser returns the active link; ErrNotFound when none.
func (s *Store) RCLinkByUser(ctx context.Context, userID string) (RCLink, error) {
	row, err := scanLink(s.Q.QueryRow(ctx,
		linkSel("l")+` WHERE l.user_id = $1 AND l.deleted_at IS NULL`, userID))
	if err != nil {
		return RCLink{}, err
	}
	return row, nil
}

// ActiveRCLinks lists all live links (the poll loop's work list).
func (s *Store) ActiveRCLinks(ctx context.Context) ([]RCLink, error) {
	rows, err := s.Q.Query(ctx, linkSel("l")+` WHERE l.deleted_at IS NULL ORDER BY l.user_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RCLink
	for rows.Next() {
		l, err := scanLink(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

func scanLink(row pgx.Row) (RCLink, error) {
	var l RCLink
	err := row.Scan(&l.UserID, &l.BaseURL, &l.RCUserID, &l.RCUsername, &l.PatEnc, &l.Watermark, &l.CreatedAt)
	return l, err
}

// UpsertRCRoom ensures the owner's mirrored room exists; inserted reports
// whether this call created it (triggering backfill). Ids are per link owner
// (see rcbridge.rcID), so no other user's rows can conflict. An archived room
// (unlinked earlier) comes back as the workspace still advertises it. The
// stored history watermark is read back so delta pulls start where the last
// one ended.
func (s *Store) UpsertRCRoom(ctx context.Context, r *RoomRow, createdAt time.Time) (bool, error) {
	var inserted bool
	err := s.WithTx(ctx, func(tx *Store) error {
		return tx.Q.QueryRow(ctx, `
			INSERT INTO rooms (id, type, name, description, source, source_rid, created_by, created_at)
			VALUES ($1, $2, $3, '', $4, $5, $6, $7)
			ON CONFLICT (id) DO UPDATE SET
				type = $2, name = $3, archived_at = NULL, updated_at = now()
			RETURNING (xmax = 0) AS inserted, created_at, updated_at, source_synced_at`,
			r.ID, r.Type, r.Name, r.Source, r.SourceRID, r.CreatedBy, createdAt,
		).Scan(&inserted, &r.CreatedAt, &r.UpdatedAt, &r.SourceSyncedAt)
	})
	return inserted, err
}

// AddRCRoomMember joins a link owner to a mirrored room (DO NOTHING semantics
// come from AddMember already).
func (s *Store) AddRCRoomMember(ctx context.Context, roomID, userID string) error {
	return s.AddMember(ctx, roomID, userID, "member")
}

// ArchiveRCRoom hides one mirrored room when its subscription disappears
// remotely (removed from the room, or the room was deleted there).
func (s *Store) ArchiveRCRoom(ctx context.Context, roomID string) error {
	_, err := s.Q.Exec(ctx, `
		UPDATE rooms SET archived_at = now(), updated_at = now()
		WHERE id = $1 AND source = $2 AND archived_at IS NULL`, roomID, SrcRocketChat)
	return err
}

// SetRoomSourceSynced stamps the per-room history watermark.
func (s *Store) SetRoomSourceSynced(ctx context.Context, roomID string, synced time.Time) error {
	_, err := s.Q.Exec(ctx, `UPDATE rooms SET source_synced_at = $2 WHERE id = $1`, roomID, synced)
	return err
}

// UpsertStubUser ensures a remote author has a local, non-login shadow
// account owned by one link (the id derives from the link owner, so no stub is
// shared between users). Shadow accounts look like webhook bots
// (password_hash '!', never verifiable) and SearchUsers hides them. A remote
// name that collides with a local username gets an id-derived suffix.
func (s *Store) UpsertStubUser(ctx context.Context, userID, username string) error {
	if _, err := s.UserByID(ctx, userID); err == nil {
		return nil
	}
	for _, candidate := range []string{username, username + "-" + userID[:8]} {
		_, err := s.Q.Exec(ctx, `
			INSERT INTO users (id, username, display_name, password_hash, role, enabled, preferences)
			VALUES ($1, $2, $3, '!', 'user', true, '{}'::jsonb)`,
			userID, candidate, username)
		if err == nil {
			return nil
		}
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" && strings.Contains(pgErr.ConstraintName, "username") {
			continue
		}
		return err
	}
	return errors.New("stub username collision")
}

// InsertRCMessage stores one mirrored message idempotently; inserted is true
// only when the row actually landed (first sighting), in which case receipts
// were seeded.
func (s *Store) InsertRCMessage(ctx context.Context, m *MessageRow) (bool, error) {
	inserted := false
	err := s.WithTx(ctx, func(tx *Store) error {
		var landed string
		err := tx.Q.QueryRow(ctx, `
			INSERT INTO messages (id, room_id, author_id, reply_to_message_id, body, format, created_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7)
			ON CONFLICT (id) DO NOTHING
			RETURNING id::text`,
			m.ID, m.RoomID, m.AuthorID, m.ReplyToMessageID, m.Body, m.Format, m.CreatedAt).Scan(&landed)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil // already mirrored (sync race, repeat run, own echo)
		}
		if err != nil {
			return err
		}
		inserted = true
		_, err = tx.SeedReceipts(ctx, m.ID, m.RoomID, m.AuthorID)
		return err
	})
	return inserted, err
}

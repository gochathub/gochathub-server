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
	RCUserID   string
	RCUsername string
	PatEnc     []byte
	Watermark  *time.Time
	CreatedAt  time.Time
}

const linkCols = `l.user_id, l.rc_user_id, l.rc_username, l.pat_enc, l.watermark, l.created_at`

// InsertRCLink creates or replaces the caller's link (deleted rows revive;
// a re-link restarts the watermark from scratch).
func (s *Store) InsertRCLink(ctx context.Context, l *RCLink) error {
	return s.Q.QueryRow(ctx, `
		INSERT INTO rocket_chat_links (user_id, rc_user_id, rc_username, pat_enc, watermark)
		VALUES ($1, $2, $3, $4, NULL)
		ON CONFLICT (user_id) DO UPDATE SET
			rc_user_id = EXCLUDED.rc_user_id, rc_username = EXCLUDED.rc_username,
			pat_enc = EXCLUDED.pat_enc, watermark = NULL, deleted_at = NULL
		RETURNING created_at`,
		l.UserID, l.RCUserID, l.RCUsername, l.PatEnc).Scan(&l.CreatedAt)
}

// SetRCLinkUsername updates the denormalized display name from users.info.
func (s *Store) SetRCLinkUsername(ctx context.Context, userID, username string) error {
	_, err := s.Q.Exec(ctx, `UPDATE rocket_chat_links SET rc_username = $2 WHERE user_id = $1`, userID, username)
	return err
}

// SetRCLinkWatermark advances the subscriptions delta cursor.
func (s *Store) SetRCLinkWatermark(ctx context.Context, userID string, wm time.Time) error {
	_, err := s.Q.Exec(ctx, `UPDATE rocket_chat_links SET watermark = $2 WHERE user_id = $1`, userID, wm)
	return err
}

// DeleteRCLink is unlink: the PAT ciphertext is destroyed; mirrored rooms stay.
func (s *Store) DeleteRCLink(ctx context.Context, userID string) error {
	_, err := s.Q.Exec(ctx, `DELETE FROM rocket_chat_links WHERE user_id = $1`, userID)
	return err
}

// linkSel is the full SELECT for a rocket_chat_links row under `alias`.
func linkSel(alias string) string {
	return `SELECT ` + alias + `.user_id, ` + alias + `.rc_user_id, ` + alias + `.rc_username, ` +
		alias + `.pat_enc, ` + alias + `.watermark, ` + alias + `.created_at FROM rocket_chat_links ` + alias
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
	err := row.Scan(&l.UserID, &l.RCUserID, &l.RCUsername, &l.PatEnc, &l.Watermark, &l.CreatedAt)
	return l, err
}

// RCRoomByRID returns the locally mirrored room for a Rocket.Chat room id.
func (s *Store) RCRoomByRID(ctx context.Context, rid string) (RoomRow, error) {
	return scanRoom(s.Q.QueryRow(ctx,
		`SELECT `+roomCols+` FROM rooms r WHERE r.source_rid = $1 AND r.source = $2`, rid, SrcRocketChat))
}

// UpsertRCRoom ensures the mirrored room exists; inserted reports whether this
// call created it (triggering backfill). Room ids are deterministic from the
// RC rid (migrate.NSRoom), so an id conflict covers both "already bridged"
// and "already imported by rcmigrate" — the latter picks up its source marker
// here, which is what makes linking an imported workspace free. A previously
// archived local room comes back as RC still advertises it.
func (s *Store) UpsertRCRoom(ctx context.Context, r *RoomRow, createdAt time.Time) (bool, error) {
	var inserted bool
	err := s.WithTx(ctx, func(tx *Store) error {
		return tx.Q.QueryRow(ctx, `
			INSERT INTO rooms (id, type, name, description, source, source_rid, created_by, created_at)
			VALUES ($1, $2, $3, '', $4, $5, $6, $7)
			ON CONFLICT (id) DO UPDATE SET
				type = $2, name = $3, source = $4, source_rid = $5, archived_at = NULL, updated_at = now()
			RETURNING (xmax = 0) AS inserted, created_at, updated_at`,
			r.ID, r.Type, r.Name, r.Source, r.SourceRID, r.CreatedBy, createdAt,
		).Scan(&inserted, &r.CreatedAt, &r.UpdatedAt)
	})
	return inserted, err
}

// AddRCRoomMember joins a link owner to a mirrored room (DO NOTHING semantics
// come from AddMember already).
func (s *Store) AddRCRoomMember(ctx context.Context, roomID, userID string) error {
	return s.AddMember(ctx, roomID, userID, "member")
}

// ArchiveRCRoom tombstones locally when the RC subscription disappears
// (removed from the RC room, or the room was deleted there).
func (s *Store) ArchiveRCRoom(ctx context.Context, rid string) error {
	_, err := s.Q.Exec(ctx, `
		UPDATE rooms SET archived_at = now(), updated_at = now()
		WHERE source_rid = $1 AND source = $2 AND archived_at IS NULL`, rid, SrcRocketChat)
	return err
}

// SetRoomSourceSynced stamps the per-room history watermark.
func (s *Store) SetRoomSourceSynced(ctx context.Context, roomID string, synced time.Time) error {
	_, err := s.Q.Exec(ctx, `UPDATE rooms SET source_synced_at = $2 WHERE id = $1`, roomID, synced)
	return err
}

// UpsertStubUser ensures an RC author has a local, non-login shadow account
// (same pattern as webhook bots: "!" is not a verifiable hash). RC usernames
// can collide with local ones (different server); retry with an -rc suffix.
// An rcMigrate-imported real user (same nsUser id, real hash) satisfies the
// lookup and is used as-is — the two paths share one identity.
func (s *Store) UpsertStubUser(ctx context.Context, userID, username string) (string, error) {
	if existing, err := s.UserByID(ctx, userID); err == nil {
		return existing.Username, nil // already present: stub or imported real account
	}
	for attempt := 0; attempt < 3; attempt++ {
		candidate := username
		if attempt > 0 {
			candidate = username + strings.Repeat("-rc", attempt)
		}
		_, err := s.Q.Exec(ctx, `
			INSERT INTO users (id, username, display_name, password_hash, role, enabled, preferences)
			VALUES ($1, $2, $2, '!', 'user', true, '{}'::jsonb)`,
			userID, candidate)
		if err == nil {
			return candidate, nil
		}
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" && strings.Contains(pgErr.ConstraintName, "username") {
			continue // local account owns this name: suffix and retry
		}
		return "", err
	}
	return "", errors.New("stub username: no free -rc suffix")
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

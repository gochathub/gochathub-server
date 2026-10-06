package store

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
)

// AttachmentRow is the attachments table shape.
type AttachmentRow struct {
	ID         string
	UploaderID string
	StorageKey string
	Filename   string
	MimeType   string
	SizeBytes  int64
	SHA256     string
	Width      *int
	Height     *int
	Status     string
	CreatedAt  time.Time
	DeletedAt  *time.Time
}

func (s *Store) InsertAttachment(ctx context.Context, a *AttachmentRow) error {
	return s.Q.QueryRow(ctx, `
		INSERT INTO attachments (id, uploader_id, storage_key, filename, mime_type, size_bytes, sha256)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		RETURNING created_at`,
		a.ID, a.UploaderID, a.StorageKey, a.Filename, a.MimeType, a.SizeBytes, a.SHA256,
	).Scan(&a.CreatedAt)
}

func scanAttachment(row pgx.Row) (AttachmentRow, error) {
	var a AttachmentRow
	err := row.Scan(&a.ID, &a.UploaderID, &a.StorageKey, &a.Filename, &a.MimeType,
		&a.SizeBytes, &a.SHA256, &a.Width, &a.Height, &a.Status, &a.CreatedAt, &a.DeletedAt)
	return a, err
}

const attachmentCols = `a.id::text, a.uploader_id::text, a.storage_key, a.filename, a.mime_type,
	a.size_bytes, a.sha256, a.width, a.height, a.status::text, a.created_at, a.deleted_at`

func (s *Store) AttachmentByID(ctx context.Context, id string) (AttachmentRow, error) {
	return scanAttachment(s.Q.QueryRow(ctx, `SELECT `+attachmentCols+` FROM attachments a WHERE a.id = $1 AND a.deleted_at IS NULL`, id))
}

func (s *Store) AttachmentsByIDs(ctx context.Context, ids []string) ([]AttachmentRow, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	rows, err := s.Q.Query(ctx, `SELECT `+attachmentCols+`
		FROM attachments a WHERE a.id::text = ANY($1) AND a.deleted_at IS NULL`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AttachmentRow
	for rows.Next() {
		a, err := scanAttachment(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// CompleteAttachment stamps size/checksum after the object store upload.
func (s *Store) CompleteAttachment(ctx context.Context, id string, size int64, sha256 string) (AttachmentRow, error) {
	return scanAttachment(s.Q.QueryRow(ctx, `
		UPDATE attachments AS a SET size_bytes = $2, sha256 = $3, status = 'ready'
		WHERE a.id = $1 AND a.deleted_at IS NULL
		RETURNING `+attachmentCols, id, size, sha256))
}

// DeleteAttachment soft-deletes; object deletion is the storage janitor's job.
func (s *Store) DeleteAttachment(ctx context.Context, id, uploaderID string) error {
	_, err := s.Q.Exec(ctx, `
		UPDATE attachments SET deleted_at = now()
		WHERE id = $1 AND uploader_id = $2 AND deleted_at IS NULL`, id, uploaderID)
	return err
}

// AttachmentVisibleTo reports read authorization via a message link in a room
// whose caller is a member (uploader/admin handled in the service).
func (s *Store) AttachmentVisibleTo(ctx context.Context, attachID, userID string) (bool, error) {
	var ok bool
	err := s.Q.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1
			FROM message_attachments ma
			JOIN messages m  ON m.id = ma.message_id
			JOIN room_members rm ON rm.room_id = m.room_id AND rm.user_id = $2
			WHERE ma.attachment_id = $1
		)`, attachID, userID).Scan(&ok)
	return ok, err
}

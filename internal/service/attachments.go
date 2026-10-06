package service

import (
	"context"
	"fmt"
	"path"
	"regexp"
	"strings"

	"github.com/gochathub/gochathub-server/internal/id"
	"github.com/gochathub/gochathub-server/internal/model"
	"github.com/gochathub/gochathub-server/internal/store"
)

var sha256Re = regexp.MustCompile(`^[a-f0-9]{64}$`)

// AttachmentService manages the upload-session flow:
// create (pending) → client PUTs to presigned URL → complete (verify).
type AttachmentService struct{ App *App }

// CreateUploadInput is POST /attachments.
type CreateUploadInput struct {
	Filename string `json:"filename"`
	MimeType string `json:"mime_type"`
	Size     int64  `json:"size_bytes"`
	SHA256   string `json:"sha256"`
}

var mimeRe = regexp.MustCompile(`^[a-z0-9][a-z0-9!#$&^_.+-]{0,126}/[a-z0-9][a-z0-9!#$&^_.+-]{0,126}$`)

// CreateUpload issues a pending attachment row and a presigned PUT URL.
func (as *AttachmentService) CreateUpload(ctx context.Context, p Principal, in CreateUploadInput) (model.Attachment, string, error) {
	if !as.App.AllowUploads || as.App.Storage == nil {
		return model.Attachment{}, "", ErrForbidden
	}
	in.Filename = sanitizeFilename(in.Filename)
	if in.Filename == "" {
		return model.Attachment{}, "", bad("filename required")
	}
	if in.Size <= 0 || in.Size > as.App.MaxUploadBytes {
		return model.Attachment{}, "", bad("size must be between 1 and %d bytes", as.App.MaxUploadBytes)
	}
	in.MimeType = strings.ToLower(strings.TrimSpace(in.MimeType))
	if in.MimeType == "" {
		in.MimeType = "application/octet-stream"
	}
	if !mimeRe.MatchString(in.MimeType) {
		return model.Attachment{}, "", bad("mime_type must be a plain type/subtype")
	}
	if in.SHA256 != "" && !sha256Re.MatchString(strings.ToLower(in.SHA256)) {
		return model.Attachment{}, "", bad("sha256 must be 64 hex characters")
	}
	a := &store.AttachmentRow{
		ID:         id.NewID(),
		UploaderID: p.UserID,
		Filename:   in.Filename,
		MimeType:   in.MimeType,
		SizeBytes:  in.Size,
		SHA256:     strings.ToLower(in.SHA256),
		Status:     "pending",
	}
	// storage key is fully server-generated (path traversal defense)
	a.StorageKey = "att/" + a.ID + "/" + in.Filename
	if err := as.App.Store.InsertAttachment(ctx, a); err != nil {
		return model.Attachment{}, "", fmt.Errorf("create attachment: %w", err)
	}
	url, err := as.App.Storage.PresignPut(ctx, a.StorageKey, a.MimeType, a.SizeBytes)
	if err != nil {
		return model.Attachment{}, "", fmt.Errorf("presign put: %w", err)
	}
	return attachmentToModel(*a, ""), url, nil
}

// Complete verifies the object landed (size/sha256 match the upload session)
// and marks the attachment ready.
func (as *AttachmentService) Complete(ctx context.Context, p Principal, attachID string) (model.Attachment, error) {
	a, err := as.App.Store.AttachmentByID(ctx, attachID)
	if err != nil {
		return model.Attachment{}, ErrNotFound
	}
	if a.UploaderID != p.UserID {
		return model.Attachment{}, ErrForbidden
	}
	if a.Status != "pending" {
		return model.Attachment{}, ErrConflict
	}
	size, exists, gotSHA, err := as.App.Storage.Stat(ctx, a.StorageKey)
	if err != nil {
		return model.Attachment{}, fmt.Errorf("stat object: %w", err)
	}
	if !exists {
		return model.Attachment{}, bad("object not uploaded yet")
	}
	if a.SizeBytes == 0 {
		a.SizeBytes = size // trust the object when the session lacked size
	}
	if size != a.SizeBytes {
		return model.Attachment{}, bad("uploaded size %d does not match session size %d", size, a.SizeBytes)
	}
	if a.SHA256 != "" && gotSHA != "" && gotSHA != a.SHA256 {
		return model.Attachment{}, bad("sha256 mismatch")
	}
	ready, err := as.App.Store.CompleteAttachment(ctx, attachID, size, firstNonEmpty(a.SHA256, gotSHA))
	if err != nil {
		return model.Attachment{}, fmt.Errorf("complete attachment: %w", err)
	}
	return attachmentToModel(ready, ""), nil
}

// Get returns metadata + presigned download URL after authorization:
// uploader, admins, room members through message links, avatar consumers.
func (as *AttachmentService) Get(ctx context.Context, p Principal, attachID string) (model.Attachment, error) {
	a, err := as.App.Store.AttachmentByID(ctx, attachID)
	if err != nil {
		return model.Attachment{}, ErrNotFound
	}
	if err := as.authorizeRead(ctx, p, a); err != nil {
		return model.Attachment{}, err
	}
	url, err := as.App.Storage.PresignGet(ctx, a.StorageKey)
	if err != nil {
		return model.Attachment{}, fmt.Errorf("presign get: %w", err)
	}
	return attachmentToModel(a, url), nil
}

// authorizeRead: uploader always; otherwise any message-linked room membership.
func (as *AttachmentService) authorizeRead(ctx context.Context, p Principal, a store.AttachmentRow) error {
	if a.UploaderID == p.UserID || p.IsAdmin() {
		return nil
	}
	ok, err := as.App.Store.AttachmentVisibleTo(ctx, a.ID, p.UserID)
	if err != nil {
		return err
	}
	if !ok {
		return ErrForbidden
	}
	return nil
}

// Delete removes access (soft) and best-effort deletes the object.
func (as *AttachmentService) Delete(ctx context.Context, p Principal, attachID string) error {
	a, err := as.App.Store.AttachmentByID(ctx, attachID)
	if err != nil {
		return ErrNotFound
	}
	if a.UploaderID != p.UserID && !p.IsAdmin() {
		return ErrForbidden
	}
	if err := as.App.Store.DeleteAttachment(ctx, attachID, a.UploaderID); err != nil {
		return fmt.Errorf("delete attachment: %w", err)
	}
	if a.UploaderID == p.UserID || p.IsAdmin() {
		_ = as.App.Storage.Delete(ctx, a.StorageKey)
	}
	_ = as.App.Store.Audit(ctx, p.UserID, "attachment.delete", "attachment", attachID, []byte(`{}`), nil)
	return nil
}

// sanitizeFilename strips directory components and control characters
// (docs: path traversal) — name is metadata, storage key is server-generated.
func sanitizeFilename(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return ""
	}
	name = path.Base(name)
	if name == "." || name == "/" || name == ".." {
		return ""
	}
	var b strings.Builder
	for _, r := range name {
		switch {
		case r < 32 || r == 0x7f:
			continue // drop control chars
		case r == '/' || r == '\\':
			b.WriteRune('-')
		default:
			b.WriteRune(r)
		}
	}
	out := b.String()
	if out == "" || out == "." || out == ".." {
		return ""
	}
	if len(out) > 255 {
		out = out[:255]
	}
	return out
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

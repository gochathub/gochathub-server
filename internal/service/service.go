// Package service holds the business layer shared by HTTP handlers, the WebSocket
// hub, and the admin CLI (docs/CLI.md: no duplicated logic).
package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	"github.com/gochathub/gochathub-server/internal/id"
	"github.com/gochathub/gochathub-server/internal/model"
	"github.com/gochathub/gochathub-server/internal/store"
)

// Typed service errors the transport layer maps to HTTP codes.
var (
	ErrNotFound     = store.ErrNotFound
	ErrUnauthorized = errors.New("unauthorized")
	ErrForbidden    = errors.New("forbidden")
	ErrConflict     = errors.New("conflict")
	ErrBadRequest   = errors.New("bad request")
)

// ValidationError renders 400 with a reason.
type ValidationError struct{ Reason string }

func (e *ValidationError) Error() string { return "validation: " + e.Reason }

func bad(format string, args ...any) error {
	return &ValidationError{Reason: fmt.Sprintf(format, args...)}
}

// App wires the business layer together. One instance per process.
type App struct {
	Store *store.Store
	Log   *slog.Logger

	Storage        Storage // attachment object store; Disabled when uploads off
	AllowUploads   bool
	MaxUploadBytes int64
	BaseOrigin     string // expected Origin for browser requests ("" = any, dev)

	Sender      PushSender
	Notify      Notifier
	Users       UserService
	Auth        *AuthService
	Rooms       *RoomService
	Messages    *MessageService
	Contacts    *ContactService
	Invites     *InviteService
	Attachments *AttachmentService
	Devices     *DeviceService
	Webhooks    *WebhookService
}

// PushSender is the push boundary. Push is a delivery optimization; never
// authoritative (docs/DECISIONS.md ADR-003).
type PushSender interface {
	// NotifyMessage fans out for a chat message event; implementers decide
	// which members get a ping by policy.
	NotifyMessage(ctx context.Context, roomID, messageID, authorID string, mentionedUserIDs []string)
	// PingValidation delivers the registration validation token (docs/UNIFIEDPUSH.md §3.3).
	PingValidation(ctx context.Context, endpointRow store.PushEndpointRow, tokenB64 string) error
}

// Storage abstracts S3-compatible object storage with presigned flows.
type Storage interface {
	PresignPut(ctx context.Context, key, mimeType string, size int64) (string, error)
	PresignGet(ctx context.Context, key string) (string, error)
	// PutObject writes server-side (webhook attachment ingest).
	PutObject(ctx context.Context, key, mimeType string, size int64, r io.Reader) error
	Stat(ctx context.Context, key string) (size int64, exists bool, sha256Hex string, err error)
	Delete(ctx context.Context, key string) error
}

// UserService groups user-profile helpers used by HTTP and CLI.
type UserService struct{ App *App }

// --- user rendering (ADR-011 avatars, ADR-013 prefs, ADR-018 null) ---

// PublicUser renders a row for other users: no email, prefs-gated last seen.
func (u *UserService) PublicUser(ctx context.Context, row store.UserRow) (model.User, error) {
	return u.render(ctx, row, false)
}

// SelfUser renders the caller's own profile (email included, prefs bypassed).
func (u *UserService) SelfUser(ctx context.Context, row store.UserRow) (model.User, error) {
	return u.render(ctx, row, true)
}

func (u *UserService) render(ctx context.Context, row store.UserRow, self bool) (model.User, error) {
	prefs := decodePrefs(row.Preferences)
	email := ""
	if self {
		email = valueOf(row.Email)
	}
	var twoFactor *bool
	if self {
		t, err := u.App.Store.TOTPByUser(ctx, row.ID)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return model.User{}, fmt.Errorf("totp status: %w", err)
		}
		on := err == nil && t.Enabled
		twoFactor = &on
	}
	return model.User{
		ID:               row.ID,
		Username:         row.Username,
		DisplayName:      row.DisplayName,
		Role:             row.Role,
		Email:            email,
		Timezone:         valueOf(row.Timezone),
		AvatarURL:        u.avatarURL(ctx, row),
		LastSeenAt:       lastSeenVisible(row.LastSeenAt, prefs, self),
		TwoFactorEnabled: twoFactor,
	}, nil
}

// avatarURL: uploaded attachment first, then Gravatar by email (ADR-017),
// else "" (clients monogram).
func (u *UserService) avatarURL(ctx context.Context, row store.UserRow) string {
	if row.AvatarAttachmentID != nil && *row.AvatarAttachmentID != "" {
		att, err := u.App.Store.AttachmentByID(ctx, *row.AvatarAttachmentID)
		if err == nil && att.Status == "ready" {
			if url, err := u.App.Storage.PresignGet(ctx, att.StorageKey); err == nil {
				return url
			}
			u.App.Log.DebugContext(ctx, "avatar presign failed", "attachment", att.ID, "err", err)
		}
	}
	if email := valueOf(row.Email); email != "" {
		sum := sha256.Sum256([]byte(strings.ToLower(strings.TrimSpace(email))))
		return "https://gravatar.com/avatar/" + hex.EncodeToString(sum[:]) + "?s=256&d=404"
	}
	return ""
}

// lastSeenVisible returns nil when hidden (ADR-018), the timestamp otherwise.
func lastSeenVisible(t *time.Time, prefs model.Preferences, self bool) *time.Time {
	if t == nil {
		return nil
	}
	if self || prefs.LastSeenVisible {
		return t
	}
	return nil
}

// Accent swatches stored server-side; api/openapi.yaml enumerates the same
// set (docs/PRIMARY_COLOR_SETTINGS.md in the client repo). "" or a missing
// key resolves to the default at decode. Clients derive shades from the hex.
const PrimaryColorDefault = "#4f46e5"

var PrimaryColorSwatches = []string{
	"#4f46e5", "#7c3aed", "#9333ea", "#db2777", "#dc2626", "#c2410c",
	"#b45309", "#4d7c0f", "#15803d", "#0f766e", "#0e7490", "#0369a1",
	"#2563eb", "#475569", "#27313a",
}

var PrimaryColorSet = map[string]bool{}

func init() {
	for _, c := range PrimaryColorSwatches {
		PrimaryColorSet[c] = true
	}
}

// decodePrefs tolerates malformed stored JSONB. Missing keys default to
// functionality-on / privacy-off: last_seen and read receipts hidden until
// opted in; group invites and private messages allowed until opted out
// (ADR-013).
func decodePrefs(raw []byte) model.Preferences {
	defaults := model.Preferences{AllowGroupInvites: true, AllowPrivateMessages: true, SpellcheckWords: []string{}, PrimaryColor: PrimaryColorDefault}
	p := defaults
	if len(raw) == 0 || json.Unmarshal(raw, &p) != nil {
		return defaults
	}
	if p.SpellcheckWords == nil {
		p.SpellcheckWords = []string{}
	}
	if p.PrimaryColor == "" {
		p.PrimaryColor = PrimaryColorDefault
	}
	return p
}

func valueOf(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// MergePreferences applies a patch to stored prefs (JSONB merge, ADR-013).
func MergePreferences(stored []byte, patch model.PreferencesPatch) model.Preferences {
	p := decodePrefs(stored)
	if patch.LastSeenVisible != nil {
		p.LastSeenVisible = *patch.LastSeenVisible
	}
	if patch.ReadReceipts != nil {
		p.ReadReceipts = *patch.ReadReceipts
	}
	if patch.AllowGroupInvites != nil {
		p.AllowGroupInvites = *patch.AllowGroupInvites
	}
	if patch.AllowPrivateMessages != nil {
		p.AllowPrivateMessages = *patch.AllowPrivateMessages
	}
	if patch.SpellcheckEnabled != nil {
		p.SpellcheckEnabled = *patch.SpellcheckEnabled
	}
	if patch.SpellcheckWords != nil {
		p.SpellcheckWords = *patch.SpellcheckWords
	}
	if patch.PrimaryColor != nil {
		p.PrimaryColor = strings.ToLower(strings.TrimSpace(*patch.PrimaryColor)) // "" = reset
	}
	return p
}

// NewEnvelope builds a WS event envelope with a fresh event id.
func NewEnvelope(ctx context.Context, typ, roomID string, data map[string]any) model.WSEnvelope {
	return model.WSEnvelope{
		Type:      typ,
		ID:        id.NewID(),
		Timestamp: time.Now().UTC(),
		RoomID:    roomID,
		Data:      data,
	}
}

// SenderPublicKey returns the VAPID public key when the sender exposes one.
func (a *App) SenderPublicKey() string {
	if pc, ok := a.Sender.(interface{ PublicKey() string }); ok {
		return pc.PublicKey()
	}
	return ""
}

// avatarURLRoom resolves only uploaded avatars for rooms (no Gravatar).
func (u *UserService) avatarURLRoom(ctx context.Context, attachID *string) string {
	if attachID == nil || *attachID == "" {
		return ""
	}
	att, err := u.App.Store.AttachmentByID(ctx, *attachID)
	if err != nil || att.Status != "ready" {
		return ""
	}
	url, err := u.App.Storage.PresignGet(ctx, att.StorageKey)
	if err != nil {
		u.App.Log.DebugContext(ctx, "room avatar presign failed", "attachment", att.ID, "err", err)
		return ""
	}
	return url
}

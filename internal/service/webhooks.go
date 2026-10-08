package service

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/netip"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/gochathub/gochathub-server/internal/id"
	"github.com/gochathub/gochathub-server/internal/markdown"
	"github.com/gochathub/gochathub-server/internal/store"
)

// WebhookService owns inbound webhooks (docs/WEBHOOKS.md): CLI management and
// the ingest path behind POST /hooks/{id}/{secret}. Each webhook posts as its
// own bot user into one fixed room (ADR-020).
type WebhookService struct{ App *App }

// maxHookAttachments mirrors the per-message cap in MessageService.Create.
const maxHookAttachments = 10

// WebhookCreateInput: exactly one of RoomID / Username.
type WebhookCreateInput struct {
	Name     string
	RoomID   string // post into this existing room
	Username string // post into a bot<->user DM with this user
	// Self: the operator asserts Username is their own account, so the DM
	// privacy gate is skipped. Without it the target must allow private
	// messages (the CLI has no chat identity to verify against).
	Self    bool
	CIDRs   []string // optional source allowlist; empty = any
	MaxSpam *float32 // optional X-Spam-Score ceiling; nil = gate off
}

// WebhookCreated carries the one-time secret.
type WebhookCreated struct {
	ID, Secret, RoomID, BotUsername string
}

// Create provisions the bot user, resolves the target room, and stores the hook.
func (ws *WebhookService) Create(ctx context.Context, in WebhookCreateInput) (WebhookCreated, error) {
	name := strings.TrimSpace(in.Name)
	if name == "" || len(name) > 100 {
		return WebhookCreated{}, bad("name required (max 100 bytes)")
	}
	if (in.RoomID == "") == (in.Username == "") {
		return WebhookCreated{}, bad("exactly one of room or user is required")
	}
	cidrs := append([]string{}, in.CIDRs...) // non-nil: column is NOT NULL
	for _, c := range cidrs {
		if _, err := netip.ParsePrefix(c); err != nil {
			return WebhookCreated{}, bad("bad cidr %q", c)
		}
	}
	hookID, secret := id.NewID(), id.NewToken()
	bot := &store.UserRow{
		ID:          id.NewID(),
		Username:    "bot-" + strings.ReplaceAll(hookID, "-", ""),
		DisplayName: name,
		Role:        "bot",
		// not a valid hash format: pwd.Verify errors, so the bot can never log in
		PasswordHash: "!",
		Enabled:      true,
		Preferences:  []byte(`{}`),
	}
	hook := &store.WebhookRow{
		ID: hookID, Name: name, BotUserID: bot.ID,
		SecretHash: id.HashToken(secret), AllowedCIDRs: cidrs, MaxSpamScore: in.MaxSpam,
	}
	err := ws.App.Store.WithTx(ctx, func(tx *store.Store) error {
		if err := tx.InsertUser(ctx, bot); err != nil {
			return fmt.Errorf("create bot: %w", err)
		}
		roomID, err := ws.targetRoom(ctx, tx, in, bot.ID)
		if err != nil {
			return err
		}
		hook.RoomID = roomID
		if err := tx.InsertWebhook(ctx, hook); err != nil {
			return fmt.Errorf("create webhook: %w", err)
		}
		return tx.Audit(ctx, "", "webhook.create", "webhook", hookID,
			[]byte(`{"room_id":"`+roomID+`","bot_user_id":"`+bot.ID+`"}`), nil)
	})
	if err != nil {
		return WebhookCreated{}, err
	}
	return WebhookCreated{ID: hookID, Secret: secret, RoomID: hook.RoomID, BotUsername: bot.Username}, nil
}

// targetRoom returns the room the bot will post into, joining or creating it.
func (ws *WebhookService) targetRoom(ctx context.Context, tx *store.Store, in WebhookCreateInput, botID string) (string, error) {
	if in.RoomID != "" {
		room, err := tx.RoomByID(ctx, in.RoomID)
		if errors.Is(err, store.ErrNotFound) {
			return "", bad("room not found")
		}
		if err != nil {
			return "", fmt.Errorf("lookup room: %w", err)
		}
		if room.Type == "direct" {
			return "", bad("direct rooms cannot host a webhook; use a user target")
		}
		if room.ArchivedAt != nil {
			return "", bad("room is archived")
		}
		if err := tx.AddMember(ctx, room.ID, botID, "member"); err != nil {
			return "", fmt.Errorf("join bot: %w", err)
		}
		return room.ID, nil
	}
	target, err := tx.UserByUsername(ctx, in.Username)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return "", fmt.Errorf("lookup user: %w", err)
	}
	if err != nil || !target.Enabled || target.Role == "bot" {
		return "", bad("user not found or not eligible")
	}
	// a fresh bot is nobody's contact, so the gate reduces to the pref
	if !in.Self && !decodePrefs(target.Preferences).AllowPrivateMessages {
		return "", ErrForbidden
	}
	room := &store.RoomRow{ID: id.NewID(), Type: "direct", CreatedBy: botID}
	if err := tx.CreateRoom(ctx, room, true); err != nil {
		return "", fmt.Errorf("create dm: %w", err)
	}
	if err := tx.AddMember(ctx, room.ID, target.ID, "member"); err != nil {
		return "", fmt.Errorf("add dm member: %w", err)
	}
	return room.ID, nil
}

func (ws *WebhookService) List(ctx context.Context) ([]store.WebhookRow, error) {
	return ws.App.Store.ListWebhooks(ctx)
}

// Rotate issues a new secret; the old one stops working immediately.
func (ws *WebhookService) Rotate(ctx context.Context, hookID string) (string, error) {
	if _, err := uuid.Parse(hookID); err != nil {
		return "", ErrNotFound
	}
	secret := id.NewToken()
	if err := ws.App.Store.SetWebhookSecret(ctx, hookID, id.HashToken(secret)); err != nil {
		return "", err
	}
	return secret, ws.App.Store.Audit(ctx, "", "webhook.rotate", "webhook", hookID, []byte(`{}`), nil)
}

func (ws *WebhookService) SetEnabled(ctx context.Context, hookID string, enabled bool) error {
	if _, err := uuid.Parse(hookID); err != nil {
		return ErrNotFound
	}
	if err := ws.App.Store.SetWebhookEnabled(ctx, hookID, enabled); err != nil {
		return err
	}
	return ws.App.Store.Audit(ctx, "", "webhook.set_enabled", "webhook", hookID,
		[]byte(`{"enabled":`+strconv.FormatBool(enabled)+`}`), nil)
}

// Delete removes the hook and disables its bot. The bot row, its messages and
// its room membership stay (messages.author_id is ON DELETE RESTRICT).
func (ws *WebhookService) Delete(ctx context.Context, hookID string) error {
	if _, err := uuid.Parse(hookID); err != nil {
		return ErrNotFound
	}
	return ws.App.Store.WithTx(ctx, func(tx *store.Store) error {
		hook, err := tx.WebhookByID(ctx, hookID)
		if err != nil {
			return err
		}
		if err := tx.DeleteWebhook(ctx, hookID); err != nil {
			return err
		}
		if err := tx.SetUserEnabled(ctx, hook.BotUserID, false); err != nil {
			return err
		}
		return tx.Audit(ctx, "", "webhook.delete", "webhook", hookID, []byte(`{}`), nil)
	})
}

// Authenticate resolves id+secret; every failure is ErrForbidden so callers
// cannot tell unknown, disabled, wrong-secret and wrong-source apart.
func (ws *WebhookService) Authenticate(ctx context.Context, hookID, secret, ip string) (store.WebhookRow, error) {
	if _, err := uuid.Parse(hookID); err != nil {
		return store.WebhookRow{}, ErrForbidden
	}
	hook, err := ws.App.Store.WebhookByID(ctx, hookID)
	if errors.Is(err, store.ErrNotFound) {
		return store.WebhookRow{}, ErrForbidden
	}
	if err != nil {
		return store.WebhookRow{}, fmt.Errorf("webhook lookup: %w", err)
	}
	if subtle.ConstantTimeCompare(hook.SecretHash, id.HashToken(secret)) != 1 ||
		!hook.Enabled || !ipAllowed(hook.AllowedCIDRs, ip) {
		return store.WebhookRow{}, ErrForbidden
	}
	return hook, nil
}

func ipAllowed(cidrs []string, ip string) bool {
	if len(cidrs) == 0 {
		return true
	}
	addr, err := netip.ParseAddr(ip)
	if err != nil {
		return false
	}
	addr = addr.Unmap()
	for _, c := range cidrs {
		if p, err := netip.ParsePrefix(c); err == nil && p.Contains(addr) {
			return true
		}
	}
	return false
}

// InboundEmail is the Postmark inbound JSON subset. The Cloudflare Email
// Worker posts the same shape. HtmlBody is deliberately absent: never stored.
type InboundEmail struct {
	MessageID         string              `json:"MessageID"`
	Subject           string              `json:"Subject"`
	TextBody          string              `json:"TextBody"`
	StrippedTextReply string              `json:"StrippedTextReply"`
	FromFull          InboundAddress      `json:"FromFull"`
	Headers           []InboundHeader     `json:"Headers"`
	Attachments       []InboundAttachment `json:"Attachments"`
}

type InboundAddress struct {
	Email string `json:"Email"`
	Name  string `json:"Name"`
}

type InboundHeader struct {
	Name  string `json:"Name"`
	Value string `json:"Value"`
}

type InboundAttachment struct {
	Name        string `json:"Name"`
	Content     string `json:"Content"` // standard base64
	ContentType string `json:"ContentType"`
}

// IngestResult reports what happened; exactly one of the three outcomes.
type IngestResult struct {
	MessageID string
	Duplicate bool
	Dropped   bool // spam gate
}

// Ingest turns one inbound email into one message in the hook's room.
func (ws *WebhookService) Ingest(ctx context.Context, hook store.WebhookRow, in InboundEmail) (IngestResult, error) {
	if len(in.MessageID) > 512 {
		return IngestResult{}, bad("MessageID too long")
	}
	if hook.MaxSpamScore != nil {
		if score, ok := spamScore(in.Headers); ok && float32(score) > *hook.MaxSpamScore {
			ws.App.Log.InfoContext(ctx, "webhook spam dropped", "webhook", hook.ID, "score", score)
			return IngestResult{Dropped: true}, nil // 200: a retry would not change the score
		}
	}
	if in.MessageID != "" {
		if existing, err := ws.App.Store.WebhookDelivery(ctx, hook.ID, in.MessageID); err == nil {
			return IngestResult{MessageID: existing, Duplicate: true}, nil
		} else if !errors.Is(err, store.ErrNotFound) {
			return IngestResult{}, fmt.Errorf("delivery lookup: %w", err)
		}
	}
	bot, err := ws.App.Store.UserByID(ctx, hook.BotUserID)
	if err != nil || !bot.Enabled {
		return IngestResult{}, ErrForbidden
	}
	p := Principal{UserID: bot.ID, Username: bot.Username, Role: bot.Role, Kind: "webhook"}

	attIDs, notes := ws.storeAttachments(ctx, p, in.Attachments)
	msg, err := ws.App.Messages.Create(ctx, p, hook.RoomID, MessageInput{
		Body: renderEmail(in, notes), AttachmentIDs: attIDs, SkipMentions: true,
	})
	if err != nil {
		for _, a := range attIDs {
			_ = ws.App.Store.DeleteAttachment(ctx, a, p.UserID)
		}
		return IngestResult{}, err
	}
	if in.MessageID != "" {
		// ponytail: check-then-insert is at-least-once (a crash between create
		// and insert re-posts on retry; never loses mail). A concurrent
		// duplicate loses on the primary key and tombstones its own message.
		if err := ws.App.Store.InsertWebhookDelivery(ctx, hook.ID, in.MessageID, msg.ID); err != nil {
			if !store.IsUnique(err) {
				ws.App.Log.WarnContext(ctx, "webhook delivery not recorded", "webhook", hook.ID, "err", err)
			} else {
				_, _ = ws.App.Store.SoftDeleteMessage(ctx, msg.ID)
				existing, _ := ws.App.Store.WebhookDelivery(ctx, hook.ID, in.MessageID)
				return IngestResult{MessageID: existing, Duplicate: true}, nil
			}
		}
	}
	if err := ws.App.Store.TouchWebhook(ctx, hook.ID); err != nil {
		ws.App.Log.WarnContext(ctx, "webhook touch", "webhook", hook.ID, "err", err)
	}
	return IngestResult{MessageID: msg.ID}, nil
}

func spamScore(h []InboundHeader) (float64, bool) {
	for _, x := range h {
		if strings.EqualFold(x.Name, "X-Spam-Score") {
			f, err := strconv.ParseFloat(strings.TrimSpace(x.Value), 64)
			return f, err == nil
		}
	}
	return 0, false
}

// storeAttachments ingests what it can; a bad attachment becomes a body note
// so the mail itself is never lost (ADR-022).
func (ws *WebhookService) storeAttachments(ctx context.Context, p Principal, atts []InboundAttachment) (ids, notes []string) {
	for i, a := range atts {
		name := sanitizeFilename(a.Name)
		if name == "" {
			name = "attachment"
		}
		if i >= maxHookAttachments {
			notes = append(notes, "skipped attachment "+inlineCode(name, 100)+": more than 10 attachments")
			continue
		}
		attID, reason := ws.storeAttachment(ctx, p, name, a)
		if reason != "" {
			notes = append(notes, "skipped attachment "+inlineCode(name, 100)+": "+reason)
			continue
		}
		ids = append(ids, attID)
	}
	return ids, notes
}

// storeAttachment reuses the upload pipeline as the bot: CreateUpload
// (sanitizing, mime and size rules), server-side PutObject, Complete (verify).
// Returns a short reason on failure.
func (ws *WebhookService) storeAttachment(ctx context.Context, p Principal, name string, a InboundAttachment) (string, string) {
	if int64(base64.StdEncoding.DecodedLen(len(a.Content))) > ws.App.MaxUploadBytes {
		return "", "too large"
	}
	data, err := base64.StdEncoding.DecodeString(a.Content)
	if err != nil || len(data) == 0 {
		return "", "unreadable"
	}
	sum := sha256.Sum256(data)
	m, _, err := ws.App.Attachments.CreateUpload(ctx, p, CreateUploadInput{
		Filename: name, MimeType: a.ContentType, Size: int64(len(data)), SHA256: hex.EncodeToString(sum[:]),
	})
	var ve *ValidationError
	switch {
	case errors.Is(err, ErrForbidden):
		return "", "uploads disabled"
	case errors.As(err, &ve):
		return "", "rejected"
	case err != nil:
		ws.App.Log.WarnContext(ctx, "webhook attachment", "err", err)
		return "", "storage error"
	}
	fail := func(err error) (string, string) {
		ws.App.Log.WarnContext(ctx, "webhook attachment", "err", err)
		_ = ws.App.Store.DeleteAttachment(ctx, m.ID, p.UserID)
		return "", "storage error"
	}
	row, err := ws.App.Store.AttachmentByID(ctx, m.ID)
	if err != nil {
		return fail(err)
	}
	if err := ws.App.Storage.PutObject(ctx, row.StorageKey, row.MimeType, row.SizeBytes, bytes.NewReader(data)); err != nil {
		return fail(err)
	}
	if _, err := ws.App.Attachments.Complete(ctx, p, m.ID); err != nil {
		return fail(err)
	}
	return m.ID, ""
}

// renderEmail builds a message body that always passes markdown.Validate:
// untrusted header fields go in inline code, the text in a fence, and
// backticks in either are replaced so nothing can close the code early and
// expose raw HTML-like text (e.g. "<a@b.c>") to the validator.
func renderEmail(in InboundEmail, notes []string) string {
	from := strings.TrimSpace(in.FromFull.Email)
	if n := strings.TrimSpace(in.FromFull.Name); n != "" {
		from = n + " <" + from + ">"
	}
	if from == "" {
		from = "(unknown sender)"
	}
	head := "**From:** " + inlineCode(from, 200)
	if s := strings.TrimSpace(in.Subject); s != "" {
		head += "\n\n**Subject:** " + inlineCode(s, 200)
	}
	tail := ""
	if len(notes) > 0 {
		tail = "\n\n- " + strings.Join(notes, "\n- ")
	}
	raw := in.StrippedTextReply
	if strings.TrimSpace(raw) == "" {
		raw = in.TextBody
	}
	text := bodyText(raw)
	if text == "" {
		return head + "\n\n(no text content)" + tail
	}
	budget := markdown.MaxMessageBytes - len(head) - len(tail) - 32 // fence + marker overhead
	if budget < 0 {
		budget = 0
	}
	return head + "\n\n```\n" + truncateBytes(text, budget) + "\n```" + tail
}

// inlineCode wraps s in a single-backtick span after neutralizing characters
// that could end the span or break the line.
func inlineCode(s string, max int) string {
	s = strings.TrimSpace(strings.Map(func(r rune) rune {
		switch {
		case r == '`':
			return 'ˋ'
		case r < 32 || r == 0x7f:
			return ' '
		}
		return r
	}, strings.ToValidUTF8(s, "?")))
	if r := []rune(s); len(r) > max {
		s = string(r[:max]) + "…"
	}
	if s == "" {
		s = "(none)"
	}
	return "`" + s + "`"
}

// bodyText keeps newlines and tabs, drops other control characters and
// replaces backticks (ˋ U+02CB) so the text cannot terminate its fence.
func bodyText(s string) string {
	return strings.TrimSpace(strings.Map(func(r rune) rune {
		switch {
		case r == '`':
			return 'ˋ'
		case r == '\n' || r == '\t':
			return r
		case r < 32 || r == 0x7f:
			return -1
		}
		return r
	}, strings.ToValidUTF8(s, "?")))
}

func truncateBytes(s string, max int) string {
	if len(s) <= max {
		return s
	}
	s = s[:max]
	for !utf8.ValidString(s) {
		s = s[:len(s)-1] // drop a split trailing rune
	}
	return s + "\n[truncated]"
}

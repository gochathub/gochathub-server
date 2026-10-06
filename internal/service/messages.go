package service

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/gochathub/gochathub-server/internal/id"
	"github.com/gochathub/gochathub-server/internal/markdown"
	"github.com/gochathub/gochathub-server/internal/model"
	"github.com/gochathub/gochathub-server/internal/store"
)

// MessageService owns message lifecycle: create with receipts seed, listing
// with hydration, edit/delete/revision, reactions, mentions.
type MessageService struct{ App *App }

// MessageInput is POST /rooms/{id}/messages.
type MessageInput struct {
	Body          string   `json:"body"`
	Format        string   `json:"format"`
	ReplyToID     string   `json:"reply_to_message_id"`
	AttachmentIDs []string `json:"attachment_ids"`
}

// Create validates, persists, seeds receipts, notifies WS, and requests push.
func (ms *MessageService) Create(ctx context.Context, p Principal, roomID string, in MessageInput) (model.Message, error) {
	room, _, err := ms.App.Store.RoomForUser(ctx, roomID, p.UserID)
	if err != nil {
		return model.Message{}, ErrNotFound
	}
	if room.ArchivedAt != nil {
		return model.Message{}, ErrConflict
	}
	if in.Format == "" {
		in.Format = "markdown"
	}
	if in.Format != "markdown" {
		return model.Message{}, bad("only markdown messages are supported")
	}
	if err := markdown.Validate(in.Body); err != nil {
		return model.Message{}, bad("message invalid: %s", err.Error())
	}
	if in.ReplyToID != "" {
		ref, err := ms.App.Store.MessageByID(ctx, in.ReplyToID)
		if err != nil || ref.RoomID != roomID {
			return model.Message{}, bad("reply target not in this room")
		}
	}
	if len(in.AttachmentIDs) > 10 {
		return model.Message{}, bad("at most 10 attachments per message")
	}
	for _, a := range in.AttachmentIDs {
		if err := ms.App.Users.checkAttachReadyOwned(ctx, a, p.UserID); err != nil {
			return model.Message{}, err
		}
	}

	m := &store.MessageRow{
		ID:       id.NewID(),
		RoomID:   roomID,
		AuthorID: p.UserID,
		Body:     in.Body,
		Format:   in.Format,
	}
	if in.ReplyToID != "" {
		m.ReplyToMessageID = &in.ReplyToID
	}
	mentionIDs, _ := mentionTargets(ctx, ms.App.Store, in.Body)
	err = ms.App.Store.WithTx(ctx, func(tx *store.Store) error {
		if err := tx.InsertMessage(ctx, m); err != nil {
			return err
		}
		if _, err := tx.SeedReceipts(ctx, m.ID, roomID, p.UserID); err != nil {
			return fmt.Errorf("seed receipts: %w", err)
		}
		if err := tx.AddMentions(ctx, m.ID, mentionIDs); err != nil {
			return fmt.Errorf("mentions: %w", err)
		}
		if err := tx.AttachMessagesTo(ctx, m.ID, in.AttachmentIDs); err != nil {
			return fmt.Errorf("link attachments: %w", err)
		}
		return nil
	})
	if err != nil {
		return model.Message{}, err
	}
	msg, err := ms.hydrate(ctx, *m, p.UserID)
	if err != nil {
		return model.Message{}, fmt.Errorf("hydrate: %w", err)
	}
	ms.App.Notify.ToRoom(roomID, NewEnvelope(ctx, "message.created", roomID, map[string]any{
		"message": msg,
	}))
	if ms.App.Sender != nil {
		ms.App.Sender.NotifyMessage(ctx, roomID, m.ID, p.UserID, mentionIDs)
	}
	return msg, nil
}

// List hydrates a cursor page; caller must be a member.
func (ms *MessageService) List(ctx context.Context, p Principal, roomID, before string, limit int) (model.MessagePage, error) {
	if _, _, err := ms.App.Store.RoomForUser(ctx, roomID, p.UserID); err != nil {
		return model.MessagePage{}, ErrNotFound
	}
	if limit < 1 || limit > 100 {
		limit = 50
	}
	var cur *store.Cursor
	if before != "" {
		c, err := decodeCursor(before)
		if err != nil {
			return model.MessagePage{}, bad("bad cursor")
		}
		cur = &c
	}
	rows, err := ms.App.Store.ListMessagesPage(ctx, roomID, cur, limit)
	if err != nil {
		return model.MessagePage{}, fmt.Errorf("list messages: %w", err)
	}
	out := make([]model.Message, 0, len(rows))
	for _, r := range rows {
		m, err := ms.hydrate(ctx, r, p.UserID)
		if err != nil {
			return model.MessagePage{}, err
		}
		out = append(out, m)
	}
	page := model.MessagePage{Items: out}
	if len(rows) == limit {
		last := rows[len(rows)-1]
		page.NextCursor = encodeCursor(store.Cursor{At: last.CreatedAt, ID: last.ID})
	}
	return page, nil
}

// hydrate assembles the model.Message: author, receipts (caller's, gated),
// attachments URLs, reactions, mentions.
func (ms *MessageService) hydrate(ctx context.Context, row store.MessageRow, callerID string) (model.Message, error) {
	msg := model.Message{
		ID:               row.ID,
		RoomID:           row.RoomID,
		AuthorID:         row.AuthorID,
		Body:             row.Body,
		Format:           row.Format,
		ReplyToMessageID: row.ReplyToMessageID,
		CreatedAt:        row.CreatedAt,
		EditedAt:         row.EditedAt,
		DeletedAt:        row.DeletedAt,
	}
	if row.DeletedAt != nil {
		msg.Body = "" // tombstone: no content
	}
	author, err := ms.App.Store.UserByID(ctx, row.AuthorID)
	if err != nil {
		return model.Message{}, fmt.Errorf("author lookup: %w", err)
	}
	pd, err := ms.App.Users.PublicUser(ctx, author)
	if err != nil {
		return model.Message{}, err
	}
	msg.Author = &pd
	// receipts visible per ADR-009 rule: senders see aggregate state for
	// their own messages; recipients see their own receipt only.
	if row.AuthorID == callerID {
		delivered, read, aggErr := ms.App.Store.ReceiptAggregate(ctx, row.ID)
		if aggErr == nil && (delivered != nil || read != nil) {
			msg.Receipts = &model.Receipts{DeliveredAt: delivered, ReadAt: read}
		}
	} else {
		rec, recErr := ms.App.Store.ReceiptForUser(ctx, row.ID, callerID)
		if recErr == nil {
			msg.Receipts = &model.Receipts{DeliveredAt: rec.DeliveredAt, ReadAt: rec.ReadAt}
		}
	}
	atts, err := ms.App.Store.AttachmentsForMessages(ctx, []string{row.ID})
	if err != nil {
		return model.Message{}, fmt.Errorf("attachments: %w", err)
	}
	if len(atts) > 0 {
		msg.Attachments = make([]model.Attachment, 0, len(atts))
		for _, a := range atts {
			var url string
			if ms.App.Storage != nil {
				u2, urlErr := ms.App.Storage.PresignGet(ctx, a.AttachmentRow.StorageKey)
				if urlErr != nil {
					return model.Message{}, fmt.Errorf("presign: %w", urlErr)
				}
				url = u2
			}
			msg.Attachments = append(msg.Attachments, attachmentToModel(a.AttachmentRow, url))
		}
	}
	rcts, err := ms.App.Store.ReactionCountsFor(ctx, []string{row.ID})
	if err != nil {
		return model.Message{}, fmt.Errorf("reactions: %w", err)
	}
	if len(rcts) > 0 {
		msg.Reactions = make([]model.Reaction, 0, len(rcts))
		for _, rc := range rcts {
			msg.Reactions = append(msg.Reactions, model.Reaction{Emoji: rc.Emoji, Count: rc.Count})
		}
	}
	return msg, nil
}

// mentionTargets maps @names to user ids (normalized lowercase).
func mentionTargets(ctx context.Context, st *store.Store, body string) ([]string, []string) {
	names := markdown.ParseMentions(body)
	if len(names) == 0 {
		return nil, names
	}
	lowered := make([]string, 0, len(names))
	for _, n := range names {
		lowered = append(lowered, strings.ToLower(n))
	}
	m, err := st.UserIDsByUsernames(ctx, lowered)
	if err != nil {
		return nil, names
	}
	var ids []string
	for _, n := range lowered {
		if uid, ok := m[n]; ok {
			ids = append(ids, uid)
		}
	}
	return ids, names
}

// attachmentToModel strips server-only fields.
func attachmentToModel(a store.AttachmentRow, url string) model.Attachment {
	return model.Attachment{
		ID: a.ID, Filename: a.Filename, MimeType: a.MimeType, SizeBytes: a.SizeBytes,
		URL: url, Width: a.Width, Height: a.Height, Status: "",
	}
}

// cursor codecs: base64url("v1<unixnano>:<id>").
const cursorPrefix = "v1:"

func encodeCursor(c store.Cursor) string {
	raw := fmt.Sprintf("%s%d:%s", cursorPrefix, c.At.UnixNano(), c.ID)
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

func decodeCursor(s string) (store.Cursor, error) {
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return store.Cursor{}, err
	}
	str := string(raw)
	rest, ok := strings.CutPrefix(str, cursorPrefix)
	if !ok {
		return store.Cursor{}, errors.New("bad cursor prefix")
	}
	nano, idPart, ok := strings.Cut(rest, ":")
	if !ok {
		return store.Cursor{}, errors.New("bad cursor body")
	}
	n, err := strconv.ParseInt(nano, 10, 64)
	if err != nil {
		return store.Cursor{}, err
	}
	_, uerr := uuid.Parse(idPart)
	if uerr != nil {
		return store.Cursor{}, uerr
	}
	return store.Cursor{At: time.Unix(0, n).UTC(), ID: idPart}, nil
}

// --- edit / delete ---

// MessageEditInput is PATCH /messages/{id}.
type MessageEditInput struct {
	Body   string `json:"body"`
	Format string `json:"format"`
}

func (ms *MessageService) Edit(ctx context.Context, p Principal, messageID string, in MessageEditInput) (model.Message, error) {
	row, err := ms.App.Store.MessageByID(ctx, messageID)
	if err != nil {
		return model.Message{}, ErrNotFound
	}
	if row.DeletedAt != nil {
		return model.Message{}, ErrConflict
	}
	if _, role, err := ms.App.Store.RoomForUser(ctx, row.RoomID, p.UserID); err != nil {
		return model.Message{}, ErrNotFound
	} else if row.AuthorID != p.UserID && role != "admin" && !p.IsAdmin() {
		return model.Message{}, ErrForbidden
	}
	if in.Format == "" {
		in.Format = "markdown"
	}
	if in.Format != "markdown" {
		return model.Message{}, bad("only markdown messages are supported")
	}
	if err := markdown.Validate(in.Body); err != nil {
		return model.Message{}, bad("message invalid: %s", err.Error())
	}
	updated, err := ms.App.Store.EditMessage(ctx, messageID, p.UserID, id.NewID(), in.Body, in.Format)
	if err != nil {
		return model.Message{}, fmt.Errorf("edit message: %w", err)
	}
	msg, err := ms.hydrate(ctx, updated, p.UserID)
	if err != nil {
		return model.Message{}, err
	}
	ms.App.Notify.ToRoom(updated.RoomID, NewEnvelope(ctx, "message.updated", updated.RoomID, map[string]any{
		"message": msg,
	}))
	return msg, nil
}

func (ms *MessageService) Delete(ctx context.Context, p Principal, messageID string) (model.Message, error) {
	row, err := ms.App.Store.MessageByID(ctx, messageID)
	if err != nil {
		return model.Message{}, ErrNotFound
	}
	if row.DeletedAt != nil {
		return model.Message{}, ErrConflict
	}
	if _, role, err := ms.App.Store.RoomForUser(ctx, row.RoomID, p.UserID); err != nil {
		return model.Message{}, ErrNotFound
	} else if row.AuthorID != p.UserID && role != "admin" && !p.IsAdmin() {
		return model.Message{}, ErrForbidden
	}
	tomb, err := ms.App.Store.SoftDeleteMessage(ctx, messageID)
	if err != nil {
		return model.Message{}, fmt.Errorf("delete message: %w", err)
	}
	msg, err := ms.hydrate(ctx, tomb, p.UserID)
	if err != nil {
		return model.Message{}, err
	}
	ms.App.Notify.ToRoom(row.RoomID, NewEnvelope(ctx, "message.deleted", row.RoomID, map[string]any{
		"message": msg,
	}))
	return msg, nil
}

// --- reactions ---

func (ms *MessageService) React(ctx context.Context, p Principal, messageID, emoji string) error {
	if !validEmoji(emoji) {
		return bad("emoji must be 1-16 code points")
	}
	row, err := ms.App.Store.MessageByID(ctx, messageID)
	if err != nil {
		return ErrNotFound
	}
	if _, _, err := ms.App.Store.RoomForUser(ctx, row.RoomID, p.UserID); err != nil {
		return ErrNotFound
	}
	if row.DeletedAt != nil {
		return ErrConflict
	}
	if err := ms.App.Store.AddReaction(ctx, messageID, p.UserID, emoji); err != nil {
		return fmt.Errorf("react: %w", err)
	}
	counts, _ := ms.App.Store.ReactionCountsFor(ctx, []string{messageID})
	count := int64(1)
	for _, rc := range counts {
		if rc.Emoji == emoji {
			count = rc.Count
		}
	}
	ms.App.Notify.ToRoom(row.RoomID, NewEnvelope(ctx, "message.reaction_added", row.RoomID, map[string]any{
		"message_id": messageID,
		"emoji":      emoji,
		"count":      count,
		"user_id":    p.UserID,
	}))
	return nil
}

func (ms *MessageService) Unreact(ctx context.Context, p Principal, messageID, emoji string) error {
	row, err := ms.App.Store.MessageByID(ctx, messageID)
	if err != nil {
		return ErrNotFound
	}
	if _, _, err := ms.App.Store.RoomForUser(ctx, row.RoomID, p.UserID); err != nil {
		return ErrNotFound
	}
	existed, err := ms.App.Store.RemoveReaction(ctx, messageID, p.UserID, emoji)
	if err != nil {
		return fmt.Errorf("unreact: %w", err)
	}
	if !existed {
		return ErrNotFound
	}
	counts, _ := ms.App.Store.ReactionCountsFor(ctx, []string{messageID})
	count := int64(0)
	for _, rc := range counts {
		if rc.Emoji == emoji {
			count = rc.Count
		}
	}
	ms.App.Notify.ToRoom(row.RoomID, NewEnvelope(ctx, "message.reaction_removed", row.RoomID, map[string]any{
		"message_id": messageID,
		"emoji":      emoji,
		"count":      count,
		"user_id":    p.UserID,
	}))
	return nil
}

// GetOne fetches a single message with membership authz.
func (ms *MessageService) GetOne(ctx context.Context, p Principal, messageID string) (model.Message, error) {
	row, err := ms.App.Store.MessageByID(ctx, messageID)
	if err != nil {
		return model.Message{}, ErrNotFound
	}
	if _, _, err := ms.App.Store.RoomForUser(ctx, row.RoomID, p.UserID); err != nil {
		return model.Message{}, ErrNotFound
	}
	return ms.hydrate(ctx, row, p.UserID)
}

// Reactions returns aggregated counts for a message (membership required).
func (ms *MessageService) Reactions(ctx context.Context, p Principal, messageID string) ([]model.Reaction, error) {
	row, err := ms.App.Store.MessageByID(ctx, messageID)
	if err != nil {
		return nil, ErrNotFound
	}
	if _, _, err := ms.App.Store.RoomForUser(ctx, row.RoomID, p.UserID); err != nil {
		return nil, ErrNotFound
	}
	counts, err := ms.App.Store.ReactionCountsFor(ctx, []string{messageID})
	if err != nil {
		return nil, fmt.Errorf("reactions: %w", err)
	}
	out := make([]model.Reaction, 0, len(counts))
	for _, c := range counts {
		out = append(out, model.Reaction{Emoji: c.Emoji, Count: c.Count})
	}
	return out, nil
}

// --- socket acks (delivered receipts) ---

// SocketAck records a delivered receipt from a WS ack frame and notifies the
// author (message.receipts_changed).
func (ms *MessageService) SocketAck(ctx context.Context, userID, messageID string) error {
	row, err := ms.App.Store.MessageByID(ctx, messageID)
	if err != nil {
		return ErrNotFound
	}
	if _, _, err := ms.App.Store.RoomForUser(ctx, row.RoomID, userID); err != nil {
		return ErrForbidden
	}
	if err := ms.App.Store.MarkDelivered(ctx, messageID, userID); err != nil {
		return fmt.Errorf("mark delivered: %w", err)
	}
	ms.App.Notify.ToUser(row.AuthorID, NewEnvelope(ctx, "message.receipts_changed", row.RoomID, map[string]any{
		"message_id": messageID,
		"user_id":    userID,
	}))
	return nil
}

// validEmoji keeps reactions bounded.
func validEmoji(s string) bool {
	if utf8.RuneCountInString(s) == 0 || utf8.RuneCountInString(s) > 16 {
		return false
	}
	return !strings.ContainsAny(s, " \t\n\r")
}

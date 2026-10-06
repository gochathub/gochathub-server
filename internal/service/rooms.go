package service

import (
	"context"
	"errors"
	"fmt"

	"github.com/gochathub/gochathub-server/internal/id"
	"github.com/gochathub/gochathub-server/internal/model"
	"github.com/gochathub/gochathub-server/internal/store"
)

// Notifier is the WS fan-out boundary (implemented by internal/ws.Hub).
type Notifier interface {
	ToUser(userID string, env model.WSEnvelope)
	ToRoom(roomID string, env model.WSEnvelope)
	// SubscribedToRoom reports live subscribers for push suppression
	// (docs/PUSH.md: no push to users already viewing the room).
	SubscribedToRoom(userID, roomID string) bool
	Connected(uid string) bool
}

// RoomService owns room, membership, invitation, pinning, and read-state logic.
type RoomService struct{ App *App }

// CreateInput is room creation across all types.
type CreateInput struct {
	Type           model.RoomType
	Name           string
	Description    string
	AvatarAttachID string   // optional
	Members        []string // direct/group_direct co-members (user ids)
}

func validRoomType(t model.RoomType) bool {
	switch t {
	case model.RoomPublic, model.RoomPrivate, model.RoomDirect, model.RoomGroupDir:
		return true
	}
	return false
}

// Create makes a room. Authorization rules:
//   - public/private: creator becomes admin member.
//   - group_direct: creator admin; exactly the listed members added now.
//   - direct: exactly one co-member; must be a contact unless the target
//     allows private messages (ADR prefs), and both users' prefs apply.
func (rs *RoomService) Create(ctx context.Context, p Principal, in CreateInput) (model.Room, error) {
	if !validRoomType(in.Type) {
		return model.Room{}, bad("unknown room type %q", in.Type)
	}
	room := &store.RoomRow{
		ID:          id.NewID(),
		Type:        string(in.Type),
		Description: in.Description,
		CreatedBy:   p.UserID,
	}
	var members []string
	switch in.Type {
	case model.RoomPublic, model.RoomPrivate:
		name := in.Name
		room.Name = &name
		if name == "" {
			return model.Room{}, bad("name required for %s rooms", in.Type)
		}
	case model.RoomDirect:
		if len(in.Members) != 1 {
			return model.Room{}, bad("direct rooms take exactly one other member")
		}
		other := in.Members[0]
		if other == p.UserID {
			return model.Room{}, bad("cannot start a direct room with yourself")
		}
		// one DM per peer pair: reuse the existing room instead of spawning
		// duplicates when the same conversation is started twice.
		if existing, err := rs.App.Store.GetDirectRoomBetween(ctx, p.UserID, other); err == nil {
			roomRow, role, err := rs.App.Store.RoomForUser(ctx, existing, p.UserID)
			if err == nil {
				return rs.toModel(ctx, roomRow, role, 0), nil
			}
		}
		target, err := rs.App.Store.UserByID(ctx, other)
		if err != nil {
			return model.Room{}, ErrNotFound
		}
		prefs := decodePrefs(target.Preferences)
		if !prefs.AllowPrivateMessages {
			isContact, err := rs.App.Store.IsContact(ctx, other, p.UserID)
			if err != nil || !isContact {
				return model.Room{}, ErrForbidden
			}
		}
		members = []string{other, p.UserID}
	case model.RoomGroupDir:
		members = append(members, p.UserID)
		for _, m := range in.Members {
			if m != p.UserID {
				members = append(members, m)
			}
		}
		if in.Name == "" {
			return model.Room{}, bad("name required for group rooms")
		}
		name := in.Name
		room.Name = &name
	}
	if err := rs.App.Store.CreateRoom(ctx, room, true); err != nil {
		return model.Room{}, fmt.Errorf("create room: %w", err)
	}
	for _, m := range members {
		if m == p.UserID {
			continue
		}
		if err := rs.App.Store.AddMember(ctx, room.ID, m, "member"); err != nil {
			return model.Room{}, fmt.Errorf("add member: %w", err)
		}
	}
	rr := *room
	return rs.toModel(ctx, rr, "admin", 0), nil
}

func (rs *RoomService) toModel(ctx context.Context, r store.RoomRow, role string, unread int64) model.Room {
	m := model.Room{
		ID:              r.ID,
		Type:            model.RoomType(r.Type),
		Name:            r.Name,
		Description:     r.Description,
		AvatarURL:       rs.App.Users.avatarURLRoom(ctx, r.AvatarAttachmentID),
		PinnedMessageID: r.PinnedMessageID,
		ArchivedAt:      r.ArchivedAt,
		Archived:        r.Archived,
		CreatedAt:       r.CreatedAt,
		UpdatedAt:       r.UpdatedAt,
		MyRole:          role,
		UnreadCount:     unread,
	}
	return m
}

// Get returns the room for the caller; membership required.
func (rs *RoomService) Get(ctx context.Context, p Principal, roomID string) (model.Room, error) {
	room, role, err := rs.App.Store.RoomForUser(ctx, roomID, p.UserID)
	if err != nil {
		return model.Room{}, ErrNotFound
	}
	return rs.toModel(ctx, room, role, 0), nil
}

// List returns the caller's rooms with unread counts.
func (rs *RoomService) List(ctx context.Context, p Principal) ([]model.Room, error) {
	rows, err := rs.App.Store.ListRoomsForUser(ctx, p.UserID)
	if err != nil {
		return nil, fmt.Errorf("list rooms: %w", err)
	}
	out := make([]model.Room, 0, len(rows))
	for _, r := range rows {
		out = append(out, rs.toModel(ctx, r.RoomRow, r.MyRole, r.UnreadCount))
	}
	return out, nil
}

// UpdateInput patches name/description/avatar (admins only).
type UpdateInput struct {
	Name           *string
	Description    *string
	AvatarAttachID *string // sets when non-nil; pointer nil keeps
	AvatarClear    bool
}

func (rs *RoomService) Update(ctx context.Context, p Principal, roomID string, in UpdateInput) (model.Room, error) {
	room, role, err := rs.App.Store.RoomForUser(ctx, roomID, p.UserID)
	if err != nil {
		return model.Room{}, ErrNotFound
	}
	if role != "admin" && !p.IsAdmin() {
		return model.Room{}, ErrForbidden
	}
	avatar := room.AvatarAttachmentID
	if in.AvatarClear {
		avatar = nil
	} else if in.AvatarAttachID != nil && *in.AvatarAttachID != "" {
		if err := rs.checkAttachReadyOwned(ctx, *in.AvatarAttachID, p.UserID); err != nil {
			return model.Room{}, err
		}
		avatar = in.AvatarAttachID
	}
	updated, err := rs.App.Store.UpdateRoom(ctx, roomID, in.Name, in.Description, avatar)
	if err != nil {
		return model.Room{}, fmt.Errorf("update room: %w", err)
	}
	return rs.toModel(ctx, updated, role, 0), nil
}

// Archive rooms server-side (admins only); member-level archive is a
// separate per-user flag.
func (rs *RoomService) Archive(ctx context.Context, p Principal, roomID string) error {
	_, role, err := rs.App.Store.RoomForUser(ctx, roomID, p.UserID)
	if err != nil {
		return ErrNotFound
	}
	if role != "admin" && !p.IsAdmin() {
		return ErrForbidden
	}
	if err := rs.App.Store.ArchiveRoom(ctx, roomID); err != nil {
		return fmt.Errorf("archive room: %w", err)
	}
	return rs.App.Store.Audit(ctx, p.UserID, "room.archive", "room", roomID, []byte(`{}`), nil)
}

// AddMember: self-join on public rooms; admin adds on all other types.
func (rs *RoomService) AddMember(ctx context.Context, p Principal, roomID, userID string) error {
	// fetch without the membership filter: public rooms are open to join
	room, err := rs.App.Store.RoomByID(ctx, roomID)
	if err != nil {
		return ErrNotFound
	}
	if room.Type != string(model.RoomPublic) {
		role, err := rs.App.Store.MemberRole(ctx, roomID, p.UserID)
		if err != nil {
			return ErrNotFound
		}
		if role != "admin" && !p.IsAdmin() {
			return ErrForbidden
		}
	}
	if room.ArchivedAt != nil {
		return ErrConflict
	}
	if err := rs.App.Store.AddMember(ctx, roomID, userID, "member"); err != nil {
		return fmt.Errorf("add member: %w", err)
	}
	rs.App.Notify.ToRoom(roomID, NewEnvelope(ctx, "room.member_added", roomID, map[string]any{
		"user_id": userID,
	}))
	return nil
}

// SetMemberArchive toggles the caller's per-user conversation archive
// (web client archive button).
func (rs *RoomService) SetMemberArchive(ctx context.Context, p Principal, roomID string, archived bool) error {
	if _, _, err := rs.App.Store.RoomForUser(ctx, roomID, p.UserID); err != nil {
		return ErrNotFound
	}
	return rs.App.Store.SetMemberArchived(ctx, roomID, p.UserID, archived)
}

// RemoveMember: self-leave or admin action.
func (rs *RoomService) RemoveMember(ctx context.Context, p Principal, roomID, userID string) error {
	_, role, err := rs.App.Store.RoomForUser(ctx, roomID, p.UserID)
	if err != nil {
		return ErrNotFound
	}
	if userID != p.UserID && role != "admin" && !p.IsAdmin() {
		return ErrForbidden
	}
	if err := rs.App.Store.RemoveMember(ctx, roomID, userID); err != nil {
		return fmt.Errorf("remove member: %w", err)
	}
	rs.App.Notify.ToRoom(roomID, NewEnvelope(ctx, "room.member_removed", roomID, map[string]any{
		"user_id": userID,
	}))
	return nil
}

// --- pinning ---

func (rs *RoomService) Pin(ctx context.Context, p Principal, roomID, messageID string) error {
	_, role, err := rs.App.Store.RoomForUser(ctx, roomID, p.UserID)
	if err != nil {
		return ErrNotFound
	}
	if role != "admin" && !p.IsAdmin() {
		return ErrForbidden
	}
	msg, err := rs.App.Store.MessageByID(ctx, messageID)
	if err != nil || msg.RoomID != roomID {
		return ErrNotFound
	}
	if err := rs.App.Store.SetPinnedMessage(ctx, roomID, &messageID); err != nil {
		return fmt.Errorf("pin: %w", err)
	}
	rs.App.Notify.ToRoom(roomID, NewEnvelope(ctx, "room.pinned_changed", roomID, map[string]any{
		"pinned_message_id": messageID,
	}))
	return nil
}

func (rs *RoomService) Unpin(ctx context.Context, p Principal, roomID string) error {
	_, role, err := rs.App.Store.RoomForUser(ctx, roomID, p.UserID)
	if err != nil {
		return ErrNotFound
	}
	if role != "admin" && !p.IsAdmin() {
		return ErrForbidden
	}
	if err := rs.App.Store.SetPinnedMessage(ctx, roomID, nil); err != nil {
		return fmt.Errorf("unpin: %w", err)
	}
	rs.App.Notify.ToRoom(roomID, NewEnvelope(ctx, "room.pinned_changed", roomID, map[string]any{
		"pinned_message_id": nil,
	}))
	return nil
}

// --- read state ---

// Read advances the cursor; receipts stamp read, WS event fires.
func (rs *RoomService) Read(ctx context.Context, p Principal, roomID, messageID string) error {
	if _, _, err := rs.App.Store.RoomForUser(ctx, roomID, p.UserID); err != nil {
		return ErrNotFound
	}
	msg, err := rs.App.Store.MessageByID(ctx, messageID)
	if err != nil || msg.RoomID != roomID {
		return ErrNotFound
	}
	if err := rs.App.Store.MarkRead(ctx, messageID, p.UserID); err != nil {
		return fmt.Errorf("mark read: %w", err)
	}
	// stamp any older unread rows so the count comes down completely
	if err := rs.App.Store.StampReceiptsRead(ctx, roomID, p.UserID, msg.CreatedAt); err != nil {
		return fmt.Errorf("stamp receipts: %w", err)
	}
	rs.App.Notify.ToRoom(roomID, NewEnvelope(ctx, "room.read_state_changed", roomID, map[string]any{
		"user_id":    p.UserID,
		"message_id": messageID,
	}))
	return nil
}

// --- members listing with preference gating (ADR-013) ---

// CheckSubscribable authorizes WS room subscriptions (membership required).
func (rs *RoomService) CheckSubscribable(ctx context.Context, p Principal, roomID string) error {
	if _, _, err := rs.App.Store.RoomForUser(ctx, roomID, p.UserID); err != nil {
		return ErrForbidden
	}
	return nil
}

func (rs *RoomService) Members(ctx context.Context, p Principal, roomID string) ([]model.User, error) {
	if _, _, err := rs.App.Store.RoomForUser(ctx, roomID, p.UserID); err != nil {
		return nil, ErrNotFound
	}
	rows, err := rs.App.Store.RoomMembers(ctx, roomID)
	if err != nil {
		return nil, fmt.Errorf("members: %w", err)
	}
	out := make([]model.User, 0, len(rows))
	for _, m := range rows {
		prof := store.UserRow{
			ID: m.UserID, Username: m.Username, DisplayName: m.DisplayName,
			AvatarAttachmentID: m.AvatarAtt, LastSeenAt: m.LastSeen,
		}
		u, err := rs.App.Users.PublicUser(ctx, prof)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, nil
}

func (rs *RoomService) checkAttachReadyOwned(ctx context.Context, attachID, uploader string) error {
	a, err := rs.App.Store.AttachmentByID(ctx, attachID)
	if errors.Is(err, store.ErrNotFound) {
		return bad("attachment %s not found", attachID)
	}
	if err != nil {
		return err
	}
	if a.Status != "ready" {
		return bad("attachment %s is not ready", attachID)
	}
	if a.UploaderID != uploader {
		return ErrForbidden
	}
	return nil
}

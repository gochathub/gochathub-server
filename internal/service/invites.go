package service

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/gochathub/gochathub-server/internal/id"
	"github.com/gochathub/gochathub-server/internal/model"
	"github.com/gochathub/gochathub-server/internal/store"
)

// InviteService handles room invitations (private/group_direct rooms).
type InviteService struct{ App *App }

func (is *InviteService) fillRoom(ctx context.Context, i store.InviteRow, p Principal) (model.Invite, error) {
	out := model.Invite{
		ID:         i.ID,
		Status:     model.InviteStatus(i.Status),
		ExpiresAt:  i.ExpiresAt,
		CreatedAt:  i.CreatedAt,
		AcceptedAt: i.AcceptedAt,
	}
	room, err := is.App.Store.RoomByID(ctx, i.RoomID)
	if err != nil {
		return model.Invite{}, ErrNotFound
	}
	out.Room = model.Room{
		ID:          room.ID,
		Type:        model.RoomType(room.Type),
		Name:        room.Name,
		Description: room.Description,
		AvatarURL:   is.App.Users.avatarURLRoom(ctx, room.AvatarAttachmentID),
		CreatedAt:   room.CreatedAt,
		UpdatedAt:   room.UpdatedAt,
	}
	return out, nil
}

// Create invites a user to a room: admin authz, respects the invitee's
// allow_group_invites preference, rejects existing membership/pending invite.
func (is *InviteService) Create(ctx context.Context, p Principal, roomID, inviteeID string, expiresAt *time.Time) (string, error) {
	room, role, err := is.App.Store.RoomForUser(ctx, roomID, p.UserID)
	if err != nil {
		return "", ErrNotFound
	}
	if room.Type == string(model.RoomPublic) {
		return "", bad("public rooms do not need invites")
	}
	if role != "admin" && !p.IsAdmin() {
		return "", ErrForbidden
	}
	if inviteeID == p.UserID {
		return "", bad("cannot invite yourself to your own room")
	}
	invitee, err := is.App.Store.UserByID(ctx, inviteeID)
	if err != nil {
		return "", ErrNotFound
	}
	if !invitee.Enabled {
		return "", ErrNotFound
	}
	if _, memberErr := is.App.Store.MemberRole(ctx, roomID, inviteeID); memberErr == nil {
		return "", ErrConflict
	}
	if !decodePrefs(invitee.Preferences).AllowGroupInvites {
		return "", ErrForbidden
	}
	inv := &store.InviteRow{
		ID:        id.NewID(),
		RoomID:    roomID,
		RoomName:  room.Name,
		RoomType:  room.Type,
		InviterID: p.UserID,
		InviteeID: inviteeID,
		Status:    "pending",
		ExpiresAt: expiresAt,
	}
	if err := is.App.Store.CreateInvite(ctx, inv); err != nil {
		if store.IsUnique(err) {
			return "", ErrConflict // open invite already exists
		}
		return "", fmt.Errorf("create invite: %w", err)
	}
	is.App.Notify.ToUser(inviteeID, NewEnvelope(ctx, "invite.created", roomID, map[string]any{
		"invite_id":  inv.ID,
		"room_name":  valueOf(room.Name),
		"inviter_id": p.UserID,
	}))
	_ = is.App.Store.Audit(ctx, p.UserID, "invite.create", "invite", inv.ID, []byte(`{"room_id":"`+roomID+`"}`), nil)
	return inv.ID, nil
}

// Accept adds the invitee and closes the invite. Invitee-only action.
func (is *InviteService) Accept(ctx context.Context, p Principal, inviteID string) error {
	i, err := is.App.Store.InviteForUser(ctx, inviteID, p.UserID)
	if errors.Is(err, store.ErrNotFound) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("load invite: %w", err)
	}
	if i.InviteeID != p.UserID {
		return ErrForbidden
	}
	if err := is.App.Store.UpdateInviteStatus(ctx, inviteID, "accepted"); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return ErrConflict // already closed or absent
		}
		return fmt.Errorf("accept invite: %w", err)
	}
	if err := is.App.Store.AddMember(ctx, i.RoomID, p.UserID, "member"); err != nil {
		return fmt.Errorf("join room: %w", err)
	}
	is.App.Notify.ToUser(i.InviterID, NewEnvelope(ctx, "invite.accepted", i.RoomID, map[string]any{
		"invite_id": inviteID,
		"user_id":   p.UserID,
	}))
	_ = is.App.Store.Audit(ctx, p.UserID, "invite.accept", "invite", inviteID, []byte(`{}`), nil)
	return nil
}

// Decline is invitee-only; Revoke is inviter/admin-only.
func (is *InviteService) Decline(ctx context.Context, p Principal, inviteID string) error {
	return is.close(ctx, p, inviteID, "declined", false)
}

func (is *InviteService) Revoke(ctx context.Context, p Principal, inviteID string) error {
	return is.close(ctx, p, inviteID, "revoked", true)
}

func (is *InviteService) close(ctx context.Context, p Principal, inviteID, status string, inviterSide bool) error {
	i, err := is.App.Store.InviteForUser(ctx, inviteID, p.UserID)
	if err != nil {
		return ErrNotFound
	}
	if inviterSide {
		roomOwner := i.InviterID == p.UserID
		role, _ := is.App.Store.MemberRole(ctx, i.RoomID, p.UserID)
		if !roomOwner && role != "admin" && !p.IsAdmin() {
			return ErrForbidden
		}
	} else if i.InviteeID != p.UserID {
		return ErrForbidden
	}
	if err := is.App.Store.UpdateInviteStatus(ctx, inviteID, status); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return ErrNotFound
		}
		return fmt.Errorf("close invite: %w", err)
	}
	is.App.Notify.ToUser(i.InviteeID, NewEnvelope(ctx, "invite.revoked", i.RoomID, map[string]any{
		"invite_id": inviteID,
	}))
	return nil
}

// ForMe lists open invites for the caller.
func (is *InviteService) ForMe(ctx context.Context, p Principal) ([]model.Invite, error) {
	rows, err := is.App.Store.InvitesForUser(ctx, p.UserID)
	if err != nil {
		return nil, fmt.Errorf("invites: %w", err)
	}
	out := make([]model.Invite, 0, len(rows))
	for _, r := range rows {
		m, err := is.fillRoom(ctx, r, p)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, nil
}

// ForRoom lists pending invites (admins).
func (is *InviteService) ForRoom(ctx context.Context, p Principal, roomID string) ([]model.Invite, error) {
	if _, role, err := is.App.Store.RoomForUser(ctx, roomID, p.UserID); err != nil {
		return nil, ErrNotFound
	} else if role != "admin" && !p.IsAdmin() {
		return nil, ErrForbidden
	}
	rows, err := is.App.Store.InvitesForRoom(ctx, roomID)
	if err != nil {
		return nil, fmt.Errorf("invites: %w", err)
	}
	out := make([]model.Invite, 0, len(rows))
	for _, r := range rows {
		m, err := is.fillRoom(ctx, r, p)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, nil
}

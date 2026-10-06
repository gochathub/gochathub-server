package service

import (
	"context"
	"fmt"

	"github.com/gochathub/gochathub-server/internal/model"
	"github.com/gochathub/gochathub-server/internal/store"
)

// ContactService manages one-way contact relationships (ADR-010).
type ContactService struct{ App *App }

// Add links user→contact (one-way). Self-add and duplicates are rejected.
func (cs *ContactService) Add(ctx context.Context, p Principal, contactUserID string) (model.Contact, error) {
	if contactUserID == p.UserID {
		return model.Contact{}, bad("cannot add yourself as a contact")
	}
	row, err := cs.App.Store.UserByID(ctx, contactUserID)
	if err != nil {
		return model.Contact{}, ErrNotFound
	}
	if !row.Enabled {
		return model.Contact{}, ErrNotFound
	}
	cr, err := cs.App.Store.AddContact(ctx, p.UserID, contactUserID)
	switch {
	case store.IsUnique(err):
		return model.Contact{}, ErrConflict
	case err != nil:
		return model.Contact{}, fmt.Errorf("add contact: %w", err)
	}
	pu, err := cs.App.Users.PublicUser(ctx, row)
	if err != nil {
		return model.Contact{}, err
	}
	out := model.Contact{ID: contactUserID, User: pu, CreatedAt: cr.CreatedAt}
	cs.App.Notify.ToUser(contactUserID, NewEnvelope(ctx, "contact.added", "", map[string]any{
		"contact": model.Contact{ID: p.UserID},
	}))
	return out, nil
}

// Remove unlinks.
func (cs *ContactService) Remove(ctx context.Context, p Principal, contactUserID string) error {
	if err := cs.App.Store.RemoveContact(ctx, p.UserID, contactUserID); err != nil {
		return fmt.Errorf("remove contact: %w", err)
	}
	cs.App.Notify.ToUser(contactUserID, NewEnvelope(ctx, "contact.removed", "", map[string]any{
		"contact_id": p.UserID,
	}))
	return nil
}

// List returns the caller's contacts.
func (cs *ContactService) List(ctx context.Context, p Principal) ([]model.Contact, error) {
	rows, err := cs.App.Store.ListContacts(ctx, p.UserID)
	if err != nil {
		return nil, fmt.Errorf("list contacts: %w", err)
	}
	out := make([]model.Contact, 0, len(rows))
	for _, r := range rows {
		prof := store.UserRow{
			ID: r.ContactID, Username: r.Username, DisplayName: r.DisplayName,
			AvatarAttachmentID: r.AvatarAtt, LastSeenAt: r.LastSeen,
		}
		pu, err := cs.App.Users.PublicUser(ctx, prof)
		if err != nil {
			return nil, err
		}
		out = append(out, model.Contact{ID: r.ContactID, User: pu, CreatedAt: r.CreatedAt})
	}
	return out, nil
}

package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/gochathub/gochathub-server/internal/id"
	"github.com/gochathub/gochathub-server/internal/model"
	"github.com/gochathub/gochathub-server/internal/store"
)

// ErrMailFailed: the invite row exists but the email could not be sent.
var ErrMailFailed = errors.New("mail send failed")

const signupInviteTTL = 7 * 24 * time.Hour

// Mail is a plain-text message.
type Mail struct{ To, Subject, Body string }

// Mailer is the outbound email boundary (SMTP in prod, a fake in tests).
type Mailer interface {
	Send(ctx context.Context, m Mail) error
}

// SignupInviteService manages email signup invites (ADR-026). Mail == nil
// disables the feature: every method returns ErrNotFound.
type SignupInviteService struct {
	App  *App
	Mail Mailer
}

// SignupInvite is the admin view; the token is never part of it.
type SignupInvite struct {
	ID         string     `json:"id"`
	Email      string     `json:"email"`
	Status     string     `json:"status"` // open | used | revoked | expired
	InvitedBy  *string    `json:"invited_by"`
	CreatedAt  time.Time  `json:"created_at"`
	LastSentAt time.Time  `json:"last_sent_at"`
	ExpiresAt  time.Time  `json:"expires_at"`
	UsedAt     *time.Time `json:"used_at"`
}

func inviteView(r store.SignupInviteRow) SignupInvite {
	status := "open"
	switch {
	case r.UsedAt != nil:
		status = "used"
	case r.RevokedAt != nil:
		status = "revoked"
	case !r.ExpiresAt.After(time.Now()):
		status = "expired"
	}
	return SignupInvite{ID: r.ID, Email: r.Email, Status: status, InvitedBy: r.InvitedBy,
		CreatedAt: r.CreatedAt, LastSentAt: r.LastSentAt, ExpiresAt: r.ExpiresAt, UsedAt: r.UsedAt}
}

func (s *SignupInviteService) admin(p Principal) error {
	if s.Mail == nil {
		return ErrNotFound
	}
	if !p.IsAdmin() {
		return ErrForbidden
	}
	return nil
}

// Create invites an email, or re-keys (resends) its open invite.
func (s *SignupInviteService) Create(ctx context.Context, p Principal, email string) (SignupInvite, error) {
	if err := s.admin(p); err != nil {
		return SignupInvite{}, err
	}
	email = strings.ToLower(strings.TrimSpace(email))
	if len(email) > 254 || strings.ContainsAny(email, " \r\n<>,;") || strings.Index(email, "@") < 1 {
		return SignupInvite{}, bad("email must be a valid address")
	}
	taken, err := s.App.Store.EmailTaken(ctx, email)
	if err != nil {
		return SignupInvite{}, fmt.Errorf("email lookup: %w", err)
	}
	if taken {
		return SignupInvite{}, ErrConflict
	}
	return s.send(ctx, p, store.SignupInviteRow{ID: id.NewID(), Email: email}, "signup_invite.create")
}

// Resend rotates the token of an open invite and mails it again.
func (s *SignupInviteService) Resend(ctx context.Context, p Principal, inviteID string) (SignupInvite, error) {
	if err := s.admin(p); err != nil {
		return SignupInvite{}, err
	}
	row, err := s.App.Store.SignupInviteByID(ctx, inviteID)
	if err != nil || row.UsedAt != nil || row.RevokedAt != nil {
		return SignupInvite{}, ErrNotFound
	}
	return s.send(ctx, p, row, "signup_invite.resend")
}

func (s *SignupInviteService) send(ctx context.Context, p Principal, row store.SignupInviteRow, action string) (SignupInvite, error) {
	token := id.NewToken()
	row.InvitedBy = &p.UserID
	row.ExpiresAt = time.Now().Add(signupInviteTTL)
	row, err := s.App.Store.UpsertSignupInvite(ctx, row, id.HashToken(token))
	if err != nil {
		return SignupInvite{}, fmt.Errorf("save invite: %w", err)
	}
	if err := s.App.Store.Audit(ctx, p.UserID, action, "signup_invite", row.ID, []byte(`{}`), nil); err != nil {
		return SignupInvite{}, err
	}
	inviter := p.Username
	if u, err := s.App.Store.UserByID(ctx, p.UserID); err == nil {
		inviter = u.DisplayName
	}
	// ponytail: synchronous send, bounded by the caller's context; queue it if admins ever bulk-invite
	mctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if err := s.Mail.Send(mctx, Mail{
		To:      row.Email,
		Subject: "You're invited to goChatHub",
		Body: fmt.Sprintf("%s invited you to goChatHub.\n\nCreate your account (link valid 7 days):\n%s/signup#token=%s\n\nIf you weren't expecting this, ignore this email.\n",
			inviter, strings.TrimRight(s.App.BaseOrigin, "/"), token),
	}); err != nil {
		s.App.Log.ErrorContext(ctx, "signup invite mail", "invite", row.ID, "err", err)
		return inviteView(row), ErrMailFailed
	}
	return inviteView(row), nil
}

func (s *SignupInviteService) List(ctx context.Context, p Principal) ([]SignupInvite, error) {
	if err := s.admin(p); err != nil {
		return nil, err
	}
	rows, err := s.App.Store.ListSignupInvites(ctx)
	if err != nil {
		return nil, fmt.Errorf("list signup invites: %w", err)
	}
	out := make([]SignupInvite, 0, len(rows))
	for _, r := range rows {
		out = append(out, inviteView(r))
	}
	return out, nil
}

func (s *SignupInviteService) Revoke(ctx context.Context, p Principal, inviteID string) error {
	if err := s.admin(p); err != nil {
		return err
	}
	if err := s.App.Store.RevokeSignupInvite(ctx, inviteID); err != nil {
		return err
	}
	return s.App.Store.Audit(ctx, p.UserID, "signup_invite.revoke", "signup_invite", inviteID, []byte(`{}`), nil)
}

// Lookup returns the invited email; every failure mode is ErrNotFound.
func (s *SignupInviteService) Lookup(ctx context.Context, token string) (string, error) {
	if s.Mail == nil {
		return "", ErrNotFound
	}
	return s.App.Store.LookupSignupInvite(ctx, id.HashToken(token))
}

// CompleteInput is the public signup form.
type CompleteInput struct {
	Token, Username, DisplayName, Password, Timezone, PrimaryColor string
}

// Complete consumes the token and creates the account in one transaction; any
// failure (bad username, weak password...) rolls back and leaves the token live.
func (s *SignupInviteService) Complete(ctx context.Context, in CompleteInput, userAgent string, ip *string) (string, store.UserRow, error) {
	if s.Mail == nil {
		return "", store.UserRow{}, ErrNotFound
	}
	if n := len(in.Password); n < 12 || n > 128 {
		return "", store.UserRow{}, bad("password must be 12-128 characters")
	}
	var tz *string
	if in.Timezone != "" {
		if _, err := time.LoadLocation(in.Timezone); err != nil {
			return "", store.UserRow{}, bad("timezone %q is not a valid IANA zone name", in.Timezone)
		}
		tz = &in.Timezone
	}
	color := strings.ToLower(strings.TrimSpace(in.PrimaryColor))
	if color != "" && !PrimaryColorSet[color] {
		return "", store.UserRow{}, bad("primary_color: must be one of the supported swatches")
	}
	prefs, err := json.Marshal(MergePreferences([]byte(`{}`), model.PreferencesPatch{PrimaryColor: &color}))
	if err != nil {
		return "", store.UserRow{}, err
	}
	var user store.UserRow
	err = s.App.Store.WithTx(ctx, func(tx *store.Store) error {
		inviteID, email, err := tx.ConsumeSignupInvite(ctx, id.HashToken(in.Token))
		if err != nil {
			return err
		}
		if user, err = s.App.Users.createUser(ctx, tx, in.Username, in.DisplayName, email, in.Password, "user", tz, prefs); err != nil {
			return err
		}
		return tx.Audit(ctx, user.ID, "signup_invite.complete", "signup_invite", inviteID, []byte(`{}`), ip)
	})
	if err != nil {
		return "", store.UserRow{}, err
	}
	return s.App.Auth.issueSession(ctx, user, userAgent, ip)
}

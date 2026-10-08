package httpapi

import (
	"errors"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/gochathub/gochathub-server/internal/model"
	"github.com/gochathub/gochathub-server/internal/service"
	"github.com/gochathub/gochathub-server/internal/store"
)

const version = "dev" // set via ldflags in the build

func (a *API) handleHealthz(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]string{"status": "ok"})
}

func (a *API) handleReadyz(w http.ResponseWriter, r *http.Request) {
	if err := a.svc.Store.Ping(r.Context()); err != nil {
		writeError(w, 503, "not_ready", "database unreachable")
		return
	}
	writeJSON(w, 200, map[string]string{"status": "ready"})
}

func (a *API) handleVersion(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]string{"version": version})
}

// --- auth ---

func (a *API) handleLogin(w http.ResponseWriter, r *http.Request) {
	// brute-force ceiling, independent of the general per-IP bucket
	if !a.loginRate.allow(visitorIP(r, a.trustProxy)) {
		w.Header().Set("Retry-After", "5")
		writeError(w, 429, "rate_limited", "too many login attempts")
		return
	}
	var in struct {
		Username string `json:"username"`
		Password string `json:"password"`
		// TokenRequest: non-browser clients (Android) take the session token
		// in the body instead of only a cookie (ADR-015).
		TokenRequest   bool   `json:"token_request"`
		TurnstileToken string `json:"turnstile_token"`
	}
	if err := decodeJSON(r, &in); err != nil {
		writeError(w, 400, "validation", "bad body")
		return
	}
	ip := ipForAudit(r, a.trustProxy)
	// before the password check, so bots spend Cloudflare's time, not argon2's
	if verify := a.svc.Auth.VerifyCaptcha; verify != nil {
		if err := verify(r.Context(), in.TurnstileToken, ip); err != nil {
			if !errors.Is(err, service.ErrCaptcha) {
				a.log.WarnContext(r.Context(), "turnstile verify", "err", err)
			}
			writeError(w, 403, "captcha_failed", "captcha verification failed")
			return
		}
	}
	token, user, err := a.svc.Auth.Login(r.Context(), in.Username, in.Password, r.UserAgent(), ip)
	var tf *service.TwoFactorRequired
	if errors.As(err, &tf) {
		writeJSON(w, 401, map[string]any{"error": map[string]string{
			"code": "two_factor_required", "message": "two-factor code required", "challenge": tf.Challenge,
		}})
		return
	}
	a.finishLogin(w, r, token, user, err, in.TokenRequest)
}

// handleLogin2FA redeems the challenge from a two_factor_required login.
func (a *API) handleLogin2FA(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Challenge    string `json:"challenge"`
		Code         string `json:"code"`
		TokenRequest bool   `json:"token_request"`
	}
	if err := decodeJSON(r, &in); err != nil {
		writeError(w, 400, "validation", "bad body")
		return
	}
	token, user, err := a.svc.Auth.Login2FA(r.Context(), in.Challenge, in.Code, r.UserAgent(), ipForAudit(r, a.trustProxy))
	a.finishLogin(w, r, token, user, err, in.TokenRequest)
}

func (a *API) finishLogin(w http.ResponseWriter, r *http.Request, token string, user store.UserRow, err error, tokenRequest bool) {
	if err != nil {
		a.mapError(w, err)
		return
	}
	self, err := a.svc.Users.SelfUser(r.Context(), user)
	if err != nil {
		a.mapError(w, err)
		return
	}
	http.SetCookie(w, a.sessionCookie(token, r))
	out := map[string]any{"user": self}
	if tokenRequest {
		out["token"] = token
	}
	writeJSON(w, 200, out)
}

// --- two-factor (TOTP) ---

func (a *API) handleTOTPSetup(w http.ResponseWriter, r *http.Request) {
	p, _ := principalFrom(r.Context())
	secret, url, err := a.svc.Auth.SetupTOTP(r.Context(), p)
	if err != nil {
		a.mapError(w, err)
		return
	}
	writeJSON(w, 200, map[string]string{"secret": secret, "otpauth_url": url})
}

type codeBody struct {
	Code     string `json:"code"`
	Password string `json:"password"`
}

func (a *API) handleTOTPEnable(w http.ResponseWriter, r *http.Request) {
	p, _ := principalFrom(r.Context())
	var in codeBody
	if err := decodeJSON(r, &in); err != nil {
		writeError(w, 400, "validation", "bad body")
		return
	}
	codes, err := a.svc.Auth.EnableTOTP(r.Context(), p, in.Code)
	if err != nil {
		a.mapError(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"backup_codes": codes})
}

func (a *API) handleTOTPBackupCodes(w http.ResponseWriter, r *http.Request) {
	p, _ := principalFrom(r.Context())
	var in codeBody
	if err := decodeJSON(r, &in); err != nil {
		writeError(w, 400, "validation", "bad body")
		return
	}
	codes, err := a.svc.Auth.RegenerateBackupCodes(r.Context(), p, in.Code)
	if err != nil {
		a.mapError(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"backup_codes": codes})
}

func (a *API) handleTOTPDisable(w http.ResponseWriter, r *http.Request) {
	p, _ := principalFrom(r.Context())
	var in codeBody
	if err := decodeJSON(r, &in); err != nil {
		writeError(w, 400, "validation", "bad body")
		return
	}
	if err := a.svc.Auth.DisableTOTP(r.Context(), p, in.Password, in.Code); err != nil {
		a.mapError(w, err)
		return
	}
	w.WriteHeader(204)
}

// --- personal API tokens (web profile mints, mobile signs in with it) ---

func (a *API) handleMintToken(w http.ResponseWriter, r *http.Request) {
	p, _ := principalFrom(r.Context())
	var in struct {
		Name string `json:"name"`
	}
	if err := decodeJSON(r, &in); err != nil {
		writeError(w, 400, "validation", "bad body")
		return
	}
	raw, t, err := a.svc.Users.MintAPIToken(r.Context(), p, in.Name)
	if err != nil {
		a.mapError(w, err)
		return
	}
	writeJSON(w, 201, map[string]any{"id": t.ID, "name": t.Name, "token": raw})
}

func (a *API) handleListTokens(w http.ResponseWriter, r *http.Request) {
	p, _ := principalFrom(r.Context())
	rows, err := a.svc.Users.ListAPITokens(r.Context(), p)
	if err != nil {
		a.mapError(w, err)
		return
	}
	out := make([]map[string]any, 0, len(rows))
	for _, t := range rows {
		out = append(out, map[string]any{"id": t.ID, "name": t.Name, "created_at": t.CreatedAt, "last_used_at": t.LastUsedAt})
	}
	writeJSON(w, 200, out)
}

func (a *API) handleRevokeToken(w http.ResponseWriter, r *http.Request) {
	p, _ := principalFrom(r.Context())
	if err := a.svc.Users.RevokeOwnAPIToken(r.Context(), p, r.PathValue("tokenId")); err != nil {
		a.mapError(w, err)
		return
	}
	w.WriteHeader(204)
}

// sessionCookie builds the ADR-015 cookie.
func (a *API) sessionCookie(token string, r *http.Request) *http.Cookie {
	c := &http.Cookie{
		Name:     a.cookieName,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		Secure:   a.cfg.CookieSecure,
		SameSite: http.SameSiteLaxMode,
	}
	return c
}

func (a *API) handleLogout(w http.ResponseWriter, r *http.Request) {
	p, _ := principalFrom(r.Context())
	if err := a.svc.Auth.Logout(r.Context(), p); err != nil {
		a.mapError(w, err)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: a.cookieName, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, Secure: a.cfg.CookieSecure, SameSite: http.SameSiteLaxMode})
	writeJSON(w, 200, map[string]string{"status": "logged_out"})
}

// --- users ---

func (a *API) handleMe(w http.ResponseWriter, r *http.Request) {
	p, _ := principalFrom(r.Context())
	self, err := a.svc.Users.CurrentUser(r.Context(), p)
	if err != nil {
		a.mapError(w, err)
		return
	}
	writeJSON(w, 200, self)
}

func (a *API) handleUpdateMe(w http.ResponseWriter, r *http.Request) {
	p, _ := principalFrom(r.Context())
	var in model.UpdateUserInput
	if err := decodeJSON(r, &in); err != nil {
		writeError(w, 400, "validation", "bad body")
		return
	}
	self, err := a.svc.Users.UpdateMe(r.Context(), p, in)
	if err != nil {
		a.mapError(w, err)
		return
	}
	writeJSON(w, 200, self)
}

func (a *API) handleGetPrefs(w http.ResponseWriter, r *http.Request) {
	p, _ := principalFrom(r.Context())
	prefs, err := a.svc.Users.GetPreferences(r.Context(), p)
	if err != nil {
		a.mapError(w, err)
		return
	}
	writeJSON(w, 200, prefs)
}

func (a *API) handlePatchPrefs(w http.ResponseWriter, r *http.Request) {
	p, _ := principalFrom(r.Context())
	var in model.PreferencesPatch
	if err := decodeJSON(r, &in); err != nil {
		writeError(w, 400, "validation", "bad body")
		return
	}
	prefs, err := a.svc.Users.PatchPreferences(r.Context(), p, in)
	if err != nil {
		a.mapError(w, err)
		return
	}
	writeJSON(w, 200, prefs)
}

// handleChangePassword applies PATCH /users/me/password: authenticated
// change (old + new); unknown-user session revocation mirrors the CLI path.
func (a *API) handlePatchPassword(w http.ResponseWriter, r *http.Request) {
	p, _ := principalFrom(r.Context())
	var in model.ChangePasswordRequest
	if err := decodeJSON(r, &in); err != nil {
		writeError(w, 400, "validation", "bad body")
		return
	}
	if err := a.svc.Users.ChangeMyPassword(r.Context(), p, in.OldPassword, in.NewPassword); err != nil {
		a.mapError(w, err)
		return
	}
	w.WriteHeader(204)
}

func (a *API) handleSearchUsers(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query().Get("q")
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	users, err := a.svc.Users.Search(r.Context(), q, limit)
	if err != nil {
		a.mapError(w, err)
		return
	}
	writeJSON(w, 200, users)
}

func (a *API) handleGetUser(w http.ResponseWriter, r *http.Request) {
	p, _ := principalFrom(r.Context())
	u, err := a.svc.Users.GetUser(r.Context(), p, r.PathValue("userId"))
	if err != nil {
		a.mapError(w, err)
		return
	}
	writeJSON(w, 200, u)
}

// --- contacts ---

func (a *API) handleListContacts(w http.ResponseWriter, r *http.Request) {
	p, _ := principalFrom(r.Context())
	contacts, err := a.svc.Contacts.List(r.Context(), p)
	if err != nil {
		a.mapError(w, err)
		return
	}
	writeJSON(w, 200, contacts)
}

func (a *API) handleAddContact(w http.ResponseWriter, r *http.Request) {
	p, _ := principalFrom(r.Context())
	var in struct {
		UserID string `json:"user_id"`
	}
	if err := decodeJSON(r, &in); err != nil || in.UserID == "" {
		writeError(w, 400, "validation", "user_id required")
		return
	}
	contact, err := a.svc.Contacts.Add(r.Context(), p, in.UserID)
	if err != nil {
		a.mapError(w, err)
		return
	}
	writeJSON(w, 201, contact)
}

func (a *API) handleRemoveContact(w http.ResponseWriter, r *http.Request) {
	p, _ := principalFrom(r.Context())
	if err := a.svc.Contacts.Remove(r.Context(), p, r.PathValue("contactId")); err != nil {
		a.mapError(w, err)
		return
	}
	w.WriteHeader(204)
}

// --- rooms ---

func (a *API) handleListRooms(w http.ResponseWriter, r *http.Request) {
	p, _ := principalFrom(r.Context())
	rooms, err := a.svc.Rooms.List(r.Context(), p)
	if err != nil {
		a.mapError(w, err)
		return
	}
	writeJSON(w, 200, rooms)
}

func (a *API) handleCreateRoom(w http.ResponseWriter, r *http.Request) {
	p, _ := principalFrom(r.Context())
	var in service.CreateInput
	if err := decodeJSON(r, &in); err != nil {
		writeError(w, 400, "validation", "bad body")
		return
	}
	room, err := a.svc.Rooms.Create(r.Context(), p, in)
	if err != nil {
		a.mapError(w, err)
		return
	}
	writeJSON(w, 201, room)
}

func (a *API) handleGetRoom(w http.ResponseWriter, r *http.Request) {
	p, _ := principalFrom(r.Context())
	room, err := a.svc.Rooms.Get(r.Context(), p, r.PathValue("roomId"))
	if err != nil {
		a.mapError(w, err)
		return
	}
	writeJSON(w, 200, room)
}

func (a *API) handleUpdateRoom(w http.ResponseWriter, r *http.Request) {
	p, _ := principalFrom(r.Context())
	var in struct {
		Name           *string `json:"name"`
		Description    *string `json:"description"`
		AvatarAttachID *string `json:"avatar_attachment_id"`
	}
	if err := decodeJSON(r, &in); err != nil {
		writeError(w, 400, "validation", "bad body")
		return
	}
	room, err := a.svc.Rooms.Update(r.Context(), p, r.PathValue("roomId"), service.UpdateInput{
		Name: in.Name, Description: in.Description, AvatarAttachID: in.AvatarAttachID,
	})
	if err != nil {
		a.mapError(w, err)
		return
	}
	writeJSON(w, 200, room)
}

func (a *API) handleArchiveRoom(w http.ResponseWriter, r *http.Request) {
	p, _ := principalFrom(r.Context())
	if err := a.svc.Rooms.Archive(r.Context(), p, r.PathValue("roomId")); err != nil {
		a.mapError(w, err)
		return
	}
	w.WriteHeader(204)
}

func (a *API) handleRoomMembers(w http.ResponseWriter, r *http.Request) {
	p, _ := principalFrom(r.Context())
	members, err := a.svc.Rooms.Members(r.Context(), p, r.PathValue("roomId"))
	if err != nil {
		a.mapError(w, err)
		return
	}
	writeJSON(w, 200, members)
}

func (a *API) handleAddMember(w http.ResponseWriter, r *http.Request) {
	p, _ := principalFrom(r.Context())
	var in struct {
		UserID string `json:"user_id"`
	}
	if err := decodeJSON(r, &in); err != nil || in.UserID == "" {
		writeError(w, 400, "validation", "user_id required")
		return
	}
	if err := a.svc.Rooms.AddMember(r.Context(), p, r.PathValue("roomId"), in.UserID); err != nil {
		a.mapError(w, err)
		return
	}
	w.WriteHeader(204)
}

func (a *API) handleSetMemberRole(w http.ResponseWriter, r *http.Request) {
	p, _ := principalFrom(r.Context())
	var in struct {
		Role string `json:"role"`
	}
	if err := decodeJSON(r, &in); err != nil || in.Role == "" {
		writeError(w, 400, "validation", "role required")
		return
	}
	if err := a.svc.Rooms.SetMemberRole(r.Context(), p, r.PathValue("roomId"), r.PathValue("userId"), in.Role); err != nil {
		a.mapError(w, err)
		return
	}
	w.WriteHeader(204)
}

func (a *API) handleRemoveMember(w http.ResponseWriter, r *http.Request) {
	p, _ := principalFrom(r.Context())
	if err := a.svc.Rooms.RemoveMember(r.Context(), p, r.PathValue("roomId"), r.PathValue("userId")); err != nil {
		a.mapError(w, err)
		return
	}
	w.WriteHeader(204)
}

func (a *API) handlePin(w http.ResponseWriter, r *http.Request) {
	p, _ := principalFrom(r.Context())
	var in struct {
		MessageID string `json:"message_id"`
	}
	if err := decodeJSON(r, &in); err != nil || in.MessageID == "" {
		writeError(w, 400, "validation", "message_id required")
		return
	}
	if err := a.svc.Rooms.Pin(r.Context(), p, r.PathValue("roomId"), in.MessageID); err != nil {
		a.mapError(w, err)
		return
	}
	w.WriteHeader(204)
}

func (a *API) handleUnpin(w http.ResponseWriter, r *http.Request) {
	p, _ := principalFrom(r.Context())
	if err := a.svc.Rooms.Unpin(r.Context(), p, r.PathValue("roomId")); err != nil {
		a.mapError(w, err)
		return
	}
	w.WriteHeader(204)
}

func (a *API) handleRead(w http.ResponseWriter, r *http.Request) {
	p, _ := principalFrom(r.Context())
	var in struct {
		MessageID string `json:"message_id"`
	}
	if err := decodeJSON(r, &in); err != nil || in.MessageID == "" {
		writeError(w, 400, "validation", "message_id required")
		return
	}
	if err := a.svc.Rooms.Read(r.Context(), p, r.PathValue("roomId"), in.MessageID); err != nil {
		a.mapError(w, err)
		return
	}
	w.WriteHeader(204)
}

// --- invites ---

func (a *API) handleListInvites(w http.ResponseWriter, r *http.Request) {
	p, _ := principalFrom(r.Context())
	invites, err := a.svc.Invites.ForMe(r.Context(), p)
	if err != nil {
		a.mapError(w, err)
		return
	}
	writeJSON(w, 200, invites)
}

func (a *API) handleCreateInvite(w http.ResponseWriter, r *http.Request) {
	p, _ := principalFrom(r.Context())
	var in struct {
		RoomID    string     `json:"room_id"`
		UserID    string     `json:"user_id"`
		ExpiresAt *time.Time `json:"expires_at"`
	}
	if err := decodeJSON(r, &in); err != nil || in.RoomID == "" || in.UserID == "" {
		writeError(w, 400, "validation", "room_id and user_id required")
		return
	}
	idStr, err := a.svc.Invites.Create(r.Context(), p, in.RoomID, in.UserID, in.ExpiresAt)
	if err != nil {
		a.mapError(w, err)
		return
	}
	writeJSON(w, 201, map[string]string{"invite_id": idStr})
}

func (a *API) handleAcceptInvite(w http.ResponseWriter, r *http.Request) {
	p, _ := principalFrom(r.Context())
	if err := a.svc.Invites.Accept(r.Context(), p, r.PathValue("inviteId")); err != nil {
		a.mapError(w, err)
		return
	}
	w.WriteHeader(204)
}

func (a *API) handleDeclineInvite(w http.ResponseWriter, r *http.Request) {
	p, _ := principalFrom(r.Context())
	if err := a.svc.Invites.Decline(r.Context(), p, r.PathValue("inviteId")); err != nil {
		a.mapError(w, err)
		return
	}
	w.WriteHeader(204)
}

// handleDeclineInvitePath aliases decline via POST (docs/WEBSOCKETS-adjacent API surface).
func (a *API) handleDeclineInvitePath(w http.ResponseWriter, r *http.Request) {
	a.handleDeclineInvite(w, r)
}

// --- messages ---

func (a *API) handleListMessages(w http.ResponseWriter, r *http.Request) {
	p, _ := principalFrom(r.Context())
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	page, err := a.svc.Messages.List(r.Context(), p, r.PathValue("roomId"), q.Get("before"), q.Get("q"), limit)
	if err != nil {
		a.mapError(w, err)
		return
	}
	writeJSON(w, 200, page)
}

func (a *API) handleCreateMessage(w http.ResponseWriter, r *http.Request) {
	p, _ := principalFrom(r.Context())
	var in service.MessageInput
	if err := decodeJSON(r, &in); err != nil {
		writeError(w, 400, "validation", "bad body")
		return
	}
	msg, err := a.svc.Messages.Create(r.Context(), p, r.PathValue("roomId"), in)
	if err != nil {
		a.mapError(w, err)
		return
	}
	writeJSON(w, 201, msg)
}

func (a *API) handleGetMessage(w http.ResponseWriter, r *http.Request) {
	p, _ := principalFrom(r.Context())
	msg, err := a.svc.Messages.GetOne(r.Context(), p, r.PathValue("messageId"))
	if err != nil {
		a.mapError(w, err)
		return
	}
	writeJSON(w, 200, msg)
}

func (a *API) handleEditMessage(w http.ResponseWriter, r *http.Request) {
	p, _ := principalFrom(r.Context())
	var in service.MessageEditInput
	if err := decodeJSON(r, &in); err != nil {
		writeError(w, 400, "validation", "bad body")
		return
	}
	msg, err := a.svc.Messages.Edit(r.Context(), p, r.PathValue("messageId"), in)
	if err != nil {
		a.mapError(w, err)
		return
	}
	writeJSON(w, 200, msg)
}

func (a *API) handleDeleteMessage(w http.ResponseWriter, r *http.Request) {
	p, _ := principalFrom(r.Context())
	msg, err := a.svc.Messages.Delete(r.Context(), p, r.PathValue("messageId"))
	if err != nil {
		a.mapError(w, err)
		return
	}
	writeJSON(w, 200, msg)
}

func (a *API) handleListReactions(w http.ResponseWriter, r *http.Request) {
	p, _ := principalFrom(r.Context())
	rxns, err := a.svc.Messages.Reactions(r.Context(), p, r.PathValue("messageId"))
	if err != nil {
		a.mapError(w, err)
		return
	}
	writeJSON(w, 200, rxns)
}

func (a *API) handleReact(w http.ResponseWriter, r *http.Request) {
	p, _ := principalFrom(r.Context())
	var in struct {
		Emoji string `json:"emoji"`
	}
	if err := decodeJSON(r, &in); err != nil || in.Emoji == "" {
		writeError(w, 400, "validation", "emoji required")
		return
	}
	if err := a.svc.Messages.React(r.Context(), p, r.PathValue("messageId"), in.Emoji); err != nil {
		a.mapError(w, err)
		return
	}
	w.WriteHeader(204)
}

func (a *API) handleUnreact(w http.ResponseWriter, r *http.Request) {
	p, _ := principalFrom(r.Context())
	if err := a.svc.Messages.Unreact(r.Context(), p, r.PathValue("messageId"), r.PathValue("emoji")); err != nil {
		a.mapError(w, err)
		return
	}
	w.WriteHeader(204)
}

// --- attachments ---

func (a *API) handleCreateUpload(w http.ResponseWriter, r *http.Request) {
	p, _ := principalFrom(r.Context())
	var in service.CreateUploadInput
	if err := decodeJSON(r, &in); err != nil {
		writeError(w, 400, "validation", "bad body")
		return
	}
	att, url, err := a.svc.Attachments.CreateUpload(r.Context(), p, in)
	if err != nil {
		a.mapError(w, err)
		return
	}
	writeJSON(w, 201, map[string]any{"attachment": att, "upload_url": url})
}

func (a *API) handleCompleteUpload(w http.ResponseWriter, r *http.Request) {
	p, _ := principalFrom(r.Context())
	att, err := a.svc.Attachments.Complete(r.Context(), p, r.PathValue("attachmentId"))
	if err != nil {
		a.mapError(w, err)
		return
	}
	writeJSON(w, 200, att)
}

func (a *API) handleGetAttachment(w http.ResponseWriter, r *http.Request) {
	p, _ := principalFrom(r.Context())
	att, err := a.svc.Attachments.Get(r.Context(), p, r.PathValue("attachmentId"))
	if err != nil {
		a.mapError(w, err)
		return
	}
	writeJSON(w, 200, att)
}

func (a *API) handleDeleteAttachment(w http.ResponseWriter, r *http.Request) {
	p, _ := principalFrom(r.Context())
	if err := a.svc.Attachments.Delete(r.Context(), p, r.PathValue("attachmentId")); err != nil {
		a.mapError(w, err)
		return
	}
	w.WriteHeader(204)
}

// --- devices / push ---

func (a *API) handleRegisterDevice(w http.ResponseWriter, r *http.Request) {
	p, _ := principalFrom(r.Context())
	var in service.RegisterInput
	if err := decodeJSON(r, &in); err != nil {
		writeError(w, 400, "validation", "bad body")
		return
	}
	out, err := a.svc.Devices.Register(r.Context(), p, in)
	if err != nil {
		a.mapError(w, err)
		return
	}
	writeJSON(w, 201, out)
}

func (a *API) handleValidateDevice(w http.ResponseWriter, r *http.Request) {
	p, _ := principalFrom(r.Context())
	var in struct {
		Token string `json:"token"`
	}
	if err := decodeJSON(r, &in); err != nil || in.Token == "" {
		writeError(w, 400, "validation", "token required")
		return
	}
	if err := a.svc.Devices.Validate(r.Context(), p, r.PathValue("deviceId"), in.Token); err != nil {
		a.mapError(w, err)
		return
	}
	w.WriteHeader(204)
}

func (a *API) handleRenewDevice(w http.ResponseWriter, r *http.Request) {
	p, _ := principalFrom(r.Context())
	var in model.PushRegistration
	if err := decodeJSON(r, &in); err != nil {
		writeError(w, 400, "validation", "bad body")
		return
	}
	if err := a.svc.Devices.Renew(r.Context(), p, r.PathValue("deviceId"), in); err != nil {
		a.mapError(w, err)
		return
	}
	w.WriteHeader(204)
}

func (a *API) handleDeleteDevice(w http.ResponseWriter, r *http.Request) {
	p, _ := principalFrom(r.Context())
	if err := a.svc.Devices.Delete(r.Context(), p, r.PathValue("deviceId")); err != nil {
		a.mapError(w, err)
		return
	}
	w.WriteHeader(204)
}

func (a *API) handleVapidKey(w http.ResponseWriter, r *http.Request) {
	key := a.svc.SenderPublicKey()
	if key == "" {
		writeError(w, 503, "push_unavailable", "push not configured")
		return
	}
	writeJSON(w, 200, map[string]string{"public_key": key})
}

// handleHook is the inbound webhook endpoint (docs/WEBHOOKS.md). Auth first,
// so unauthenticated callers never make the server buffer a large body.
// Statuses follow Postmark's retry rules: 200 done, 403 permanent, else retry.
func (a *API) handleHook(w http.ResponseWriter, r *http.Request) {
	hook, err := a.svc.Webhooks.Authenticate(r.Context(), r.PathValue("hookId"), r.PathValue("secret"), visitorIP(r, a.trustProxy))
	if err != nil {
		if errors.Is(err, service.ErrForbidden) {
			writeError(w, 403, "forbidden", "not allowed")
			return
		}
		a.mapError(w, err)
		return
	}
	select {
	case a.hookSem <- struct{}{}:
		defer func() { <-a.hookSem }()
	default:
		w.Header().Set("Retry-After", "30")
		writeError(w, 503, "busy", "try again later")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, a.cfg.WebhookMaxBody)
	var in service.InboundEmail
	if err := decodeJSON(r, &in); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			writeError(w, 413, "too_large", "body too large")
			return
		}
		writeError(w, 400, "validation", "bad body")
		return
	}
	res, err := a.svc.Webhooks.Ingest(r.Context(), hook, in)
	if err != nil {
		a.mapError(w, err)
		return
	}
	switch {
	case res.Dropped:
		writeJSON(w, 200, map[string]any{"dropped": "spam"})
	default:
		writeJSON(w, 200, map[string]any{"message_id": res.MessageID, "duplicate": res.Duplicate})
	}
}

// ipForAudit derives the audit IP from the request (header-aware when proxied).
func ipForAudit(r *http.Request, trustProxy bool) *string {
	ip := net.ParseIP(visitorIP(r, trustProxy))
	if ip == nil {
		return nil
	}
	s := ip.String()
	return &s
}

// --- notification modes ---

// handleMemberArchive toggles the caller's per-member conversation archive.
func (a *API) handleMemberArchive(w http.ResponseWriter, r *http.Request) {
	p, _ := principalFrom(r.Context())
	var in struct {
		Archived bool `json:"archived"`
	}
	if err := decodeJSON(r, &in); err != nil {
		writeError(w, 400, "validation", "bad body")
		return
	}
	if err := a.svc.Rooms.SetMemberArchive(r.Context(), p, r.PathValue("roomId"), in.Archived); err != nil {
		a.mapError(w, err)
		return
	}
	w.WriteHeader(204)
}

func (a *API) handleSetNotifMode(w http.ResponseWriter, r *http.Request) {
	p, _ := principalFrom(r.Context())
	var in struct {
		RoomID *string `json:"room_id"`
		Mode   string  `json:"mode"`
	}
	if err := decodeJSON(r, &in); err != nil || in.Mode == "" {
		writeError(w, 400, "validation", "mode required")
		return
	}
	if err := a.svc.Users.SetNotificationMode(r.Context(), p, in.RoomID, in.Mode); err != nil {
		a.mapError(w, err)
		return
	}
	w.WriteHeader(204)
}

func (a *API) handleListNotifModes(w http.ResponseWriter, r *http.Request) {
	p, _ := principalFrom(r.Context())
	rows, err := a.svc.Users.NotificationModes(r.Context(), p)
	if err != nil {
		a.mapError(w, err)
		return
	}
	writeJSON(w, 200, rows)
}

func (a *API) handleWS(w http.ResponseWriter, r *http.Request) {
	p, _ := principalFrom(r.Context())
	if err := a.hub.Serve(r.Context(), w, r, p.UserID); err != nil {
		writeError(w, 500, "ws_error", "websocket error")
	}
}

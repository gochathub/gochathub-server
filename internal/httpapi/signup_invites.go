package httpapi

import (
	"errors"
	"net/http"

	"github.com/gochathub/gochathub-server/internal/service"
)

// --- admin: /admin/signup-invites (ADR-026) ---

func (a *API) handleListSignupInvites(w http.ResponseWriter, r *http.Request) {
	p, _ := principalFrom(r.Context())
	out, err := a.svc.Signup.List(r.Context(), p)
	if err != nil {
		a.mapError(w, err)
		return
	}
	writeJSON(w, 200, out)
}

func (a *API) handleCreateSignupInvite(w http.ResponseWriter, r *http.Request) {
	p, _ := principalFrom(r.Context())
	var in struct {
		Email string `json:"email"`
	}
	if err := decodeJSON(r, &in); err != nil {
		writeError(w, 400, "validation", "bad body")
		return
	}
	inv, err := a.svc.Signup.Create(r.Context(), p, in.Email)
	if err != nil {
		a.mapError(w, err)
		return
	}
	writeJSON(w, 201, inv)
}

func (a *API) handleResendSignupInvite(w http.ResponseWriter, r *http.Request) {
	p, _ := principalFrom(r.Context())
	inv, err := a.svc.Signup.Resend(r.Context(), p, r.PathValue("inviteId"))
	if err != nil {
		a.mapError(w, err)
		return
	}
	writeJSON(w, 200, inv)
}

func (a *API) handleRevokeSignupInvite(w http.ResponseWriter, r *http.Request) {
	p, _ := principalFrom(r.Context())
	if err := a.svc.Signup.Revoke(r.Context(), p, r.PathValue("inviteId")); err != nil {
		a.mapError(w, err)
		return
	}
	w.WriteHeader(204)
}

// --- public: reachable only with an invite token ---

// signupGuard applies the login brute-force bucket to the public endpoints.
func (a *API) signupGuard(w http.ResponseWriter, r *http.Request) bool {
	if a.loginRate.allow(visitorIP(r, a.trustProxy)) {
		return true
	}
	w.Header().Set("Retry-After", "5")
	writeError(w, 429, "rate_limited", "too many attempts")
	return false
}

func (a *API) handleSignupLookup(w http.ResponseWriter, r *http.Request) {
	if !a.signupGuard(w, r) {
		return
	}
	var in struct {
		Token string `json:"token"`
	}
	if err := decodeJSON(r, &in); err != nil {
		writeError(w, 400, "validation", "bad body")
		return
	}
	email, err := a.svc.Signup.Lookup(r.Context(), in.Token)
	if err != nil {
		a.mapError(w, err)
		return
	}
	writeJSON(w, 200, map[string]string{"email": email})
}

func (a *API) handleSignup(w http.ResponseWriter, r *http.Request) {
	if !a.signupGuard(w, r) {
		return
	}
	var in struct {
		Token          string `json:"token"`
		Username       string `json:"username"`
		DisplayName    string `json:"display_name"`
		Password       string `json:"password"`
		Timezone       string `json:"timezone"`
		PrimaryColor   string `json:"primary_color"`
		TurnstileToken string `json:"turnstile_token"`
	}
	if err := decodeJSON(r, &in); err != nil {
		writeError(w, 400, "validation", "bad body")
		return
	}
	ip := ipForAudit(r, a.trustProxy)
	// before argon2, same as login
	if verify := a.svc.Auth.VerifyCaptcha; verify != nil {
		if err := verify(r.Context(), in.TurnstileToken, ip); err != nil {
			if !errors.Is(err, service.ErrCaptcha) {
				a.log.WarnContext(r.Context(), "turnstile verify", "err", err)
			}
			writeError(w, 403, "captcha_failed", "captcha verification failed")
			return
		}
	}
	token, user, err := a.svc.Signup.Complete(r.Context(), service.CompleteInput{
		Token: in.Token, Username: in.Username, DisplayName: in.DisplayName, Password: in.Password,
		Timezone: in.Timezone, PrimaryColor: in.PrimaryColor,
	}, r.UserAgent(), ip)
	a.finishLogin(w, r, token, user, err, false)
}

// --- admin: /admin/mail-settings (ADR-026) ---

func (a *API) handleGetMailSettings(w http.ResponseWriter, r *http.Request) {
	p, _ := principalFrom(r.Context())
	v, err := a.svc.MailCfg.Get(r.Context(), p)
	if err != nil {
		a.mapError(w, err)
		return
	}
	writeJSON(w, 200, v)
}

func (a *API) handleTestMailSettings(w http.ResponseWriter, r *http.Request) {
	p, _ := principalFrom(r.Context())
	var in struct {
		To string `json:"to"`
	}
	if err := decodeJSON(r, &in); err != nil {
		writeError(w, 400, "validation", "bad body")
		return
	}
	if err := a.svc.MailCfg.Test(r.Context(), p, in.To); err != nil {
		a.mapError(w, err)
		return
	}
	w.WriteHeader(204)
}

// handleSetActiveMail switches the active provider ("" = mail off).
func (a *API) handleSetActiveMail(w http.ResponseWriter, r *http.Request) {
	p, _ := principalFrom(r.Context())
	var in struct {
		Active string `json:"active"`
	}
	if err := decodeJSON(r, &in); err != nil {
		writeError(w, 400, "validation", "bad body")
		return
	}
	v, err := a.svc.MailCfg.SetActive(r.Context(), p, in.Active)
	if err != nil {
		a.mapError(w, err)
		return
	}
	writeJSON(w, 200, v)
}

func (a *API) handleSaveMailProvider(w http.ResponseWriter, r *http.Request) {
	p, _ := principalFrom(r.Context())
	var in struct {
		Values   map[string]string `json:"values"`
		Activate bool              `json:"activate"`
	}
	if err := decodeJSON(r, &in); err != nil {
		writeError(w, 400, "validation", "bad body")
		return
	}
	v, err := a.svc.MailCfg.Save(r.Context(), p, r.PathValue("provider"), in.Values, in.Activate)
	if err != nil {
		a.mapError(w, err)
		return
	}
	writeJSON(w, 200, v)
}

func (a *API) handleClearMailProvider(w http.ResponseWriter, r *http.Request) {
	p, _ := principalFrom(r.Context())
	v, err := a.svc.MailCfg.Clear(r.Context(), p, r.PathValue("provider"))
	if err != nil {
		a.mapError(w, err)
		return
	}
	writeJSON(w, 200, v)
}

package httpapi

import (
	"encoding/json"
	"net/http"

	"github.com/gochathub/gochathub-server/internal/service"
)

// --- passkeys (WebAuthn) + mobile handoff; docs/PASSKEYS.md ---

func passkeyJSON(v service.PasskeyView) map[string]any {
	return map[string]any{
		"id": v.ID, "name": v.Name, "created_at": v.CreatedAt, "last_used_at": v.LastUsedAt,
		"backup_eligible": v.BackupEligible, "transports": v.Transports,
	}
}

// begin is bounded by the global per-IP bucket; only verification attempts
// (finish, code exchange) spend the stricter passkeyRate.
func (a *API) handlePasskeyLoginBegin(w http.ResponseWriter, r *http.Request) {
	ceremony, opts, err := a.svc.Auth.BeginPasskeyLogin(r.Context())
	if err != nil {
		a.mapError(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"ceremony_id": ceremony, "options": opts})
}

func (a *API) handlePasskeyLoginFinish(w http.ResponseWriter, r *http.Request) {
	if !a.passkeyRate.allow(visitorIP(r, a.trustProxy)) {
		w.Header().Set("Retry-After", "5")
		writeError(w, 429, "rate_limited", "too many login attempts")
		return
	}
	var in struct {
		CeremonyID   string          `json:"ceremony_id"`
		Credential   json.RawMessage `json:"credential"`
		TokenRequest bool            `json:"token_request"`
	}
	if err := decodeJSON(r, &in); err != nil {
		writeError(w, 400, "validation", "bad body")
		return
	}
	token, user, err := a.svc.Auth.FinishPasskeyLogin(r.Context(), in.CeremonyID, in.Credential, r.UserAgent(), ipForAudit(r, a.trustProxy))
	a.finishLogin(w, r, token, user, err, in.TokenRequest)
}

func (a *API) handlePasskeyRegisterBegin(w http.ResponseWriter, r *http.Request) {
	p, _ := principalFrom(r.Context())
	var in codeBody
	if err := decodeJSON(r, &in); err != nil {
		writeError(w, 400, "validation", "bad body")
		return
	}
	ceremony, opts, err := a.svc.Auth.BeginPasskeyRegistration(r.Context(), p, in.Password, in.Code)
	if err != nil {
		a.mapError(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"ceremony_id": ceremony, "options": opts})
}

func (a *API) handlePasskeyRegisterFinish(w http.ResponseWriter, r *http.Request) {
	p, _ := principalFrom(r.Context())
	var in struct {
		CeremonyID string          `json:"ceremony_id"`
		Name       string          `json:"name"`
		Credential json.RawMessage `json:"credential"`
	}
	if err := decodeJSON(r, &in); err != nil {
		writeError(w, 400, "validation", "bad body")
		return
	}
	v, err := a.svc.Auth.FinishPasskeyRegistration(r.Context(), p, in.CeremonyID, in.Name, in.Credential)
	if err != nil {
		a.mapError(w, err)
		return
	}
	writeJSON(w, 201, passkeyJSON(v))
}

func (a *API) handlePasskeyList(w http.ResponseWriter, r *http.Request) {
	p, _ := principalFrom(r.Context())
	vs, err := a.svc.Auth.ListPasskeys(r.Context(), p)
	if err != nil {
		a.mapError(w, err)
		return
	}
	out := make([]map[string]any, 0, len(vs))
	for _, v := range vs {
		out = append(out, passkeyJSON(v))
	}
	writeJSON(w, 200, out)
}

func (a *API) handlePasskeyRename(w http.ResponseWriter, r *http.Request) {
	p, _ := principalFrom(r.Context())
	var in struct {
		Name string `json:"name"`
	}
	if err := decodeJSON(r, &in); err != nil {
		writeError(w, 400, "validation", "bad body")
		return
	}
	v, err := a.svc.Auth.RenamePasskey(r.Context(), p, r.PathValue("passkeyId"), in.Name)
	if err != nil {
		a.mapError(w, err)
		return
	}
	writeJSON(w, 200, passkeyJSON(v))
}

func (a *API) handlePasskeyDelete(w http.ResponseWriter, r *http.Request) {
	p, _ := principalFrom(r.Context())
	if err := a.svc.Auth.DeletePasskey(r.Context(), p, r.PathValue("passkeyId")); err != nil {
		a.mapError(w, err)
		return
	}
	w.WriteHeader(204)
}

func (a *API) handleMobileCode(w http.ResponseWriter, r *http.Request) {
	p, _ := principalFrom(r.Context())
	var in struct {
		CodeChallenge string `json:"code_challenge"`
	}
	if err := decodeJSON(r, &in); err != nil {
		writeError(w, 400, "validation", "bad body")
		return
	}
	code, err := a.svc.Auth.CreateMobileCode(r.Context(), p, in.CodeChallenge)
	if err != nil {
		a.mapError(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"code": code, "expires_in": 60})
}

func (a *API) handleMobileToken(w http.ResponseWriter, r *http.Request) {
	if !a.passkeyRate.allow(visitorIP(r, a.trustProxy)) {
		w.Header().Set("Retry-After", "5")
		writeError(w, 429, "rate_limited", "too many attempts")
		return
	}
	var in struct {
		Code         string `json:"code"`
		CodeVerifier string `json:"code_verifier"`
	}
	if err := decodeJSON(r, &in); err != nil {
		writeError(w, 400, "validation", "bad body")
		return
	}
	token, err := a.svc.Auth.ExchangeMobileCode(r.Context(), in.Code, in.CodeVerifier)
	if err != nil {
		a.mapError(w, err)
		return
	}
	writeJSON(w, 200, map[string]string{"token": token})
}

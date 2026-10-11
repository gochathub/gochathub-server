package httpapi_test

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"testing"

	"github.com/descope/virtualwebauthn"

	"github.com/gochathub/gochathub-server/internal/service"
)

const (
	pkOrigin = "https://example.test"
	pkRPID   = "example.test"
)

// TestPasskeyFlow drives full WebAuthn ceremonies with a virtual authenticator:
// feature switch, re-auth, registration, MFA bypass, replay, wrong origin,
// clone warning, disabled user, ownership boundary, last-passkey guard.
func TestPasskeyFlow(t *testing.T) {
	url := testURL(t)
	ts, svc := newServer(t, url, nil)
	seedUsers(t, svc, "pk", "other")
	anon := &client{t: t, b: ts.URL}
	pwLogin := func(c *client, user string) *client {
		return &client{t: t, b: ts.URL, tk: c.str(c.do("POST", "/api/v1/auth/login",
			map[string]any{"username": user, "password": "pw-" + user, "token_request": true}, 200), "token")}
	}
	c := pwLogin(anon, "pk")
	other := pwLogin(anon, "other")

	// feature off without ORIGIN
	anon.do("POST", "/api/v1/auth/passkey/login/begin", nil, 404)
	c.do("POST", "/api/v1/users/me/passkeys/begin", map[string]string{"password": "pw-pk"}, 404)

	var err error
	if svc.Auth.WA, err = service.NewWebAuthn(pkOrigin); err != nil {
		t.Fatal(err)
	}
	rp := virtualwebauthn.RelyingParty{Name: "goChatHub", ID: pkRPID, Origin: pkOrigin}
	authn := virtualwebauthn.NewAuthenticatorWithOptions(virtualwebauthn.AuthenticatorOptions{})
	cred := virtualwebauthn.NewCredential(virtualwebauthn.KeyTypeEC2)

	options := func(res map[string]any) string {
		raw, _ := json.Marshal(res["options"])
		return string(raw)
	}
	register := func(cl *client, a *virtualwebauthn.Authenticator, cr virtualwebauthn.Credential, name string) map[string]any {
		t.Helper()
		res := cl.do("POST", "/api/v1/users/me/passkeys/begin", map[string]string{"password": "pw-pk"}, 200)
		opts, err := virtualwebauthn.ParseAttestationOptions(options(res))
		if err != nil {
			t.Fatal(err)
		}
		a.Options.UserHandle = []byte(opts.UserID)
		resp := virtualwebauthn.CreateAttestationResponse(rp, *a, cr, *opts)
		return cl.do("POST", "/api/v1/users/me/passkeys/finish", map[string]any{
			"ceremony_id": cl.str(res, "ceremony_id"), "name": name, "credential": json.RawMessage(resp),
		}, 201)
	}
	// assertion builds a login finish body; origin lets a test lie about it.
	assertion := func(origin string) map[string]any {
		t.Helper()
		res := anon.do("POST", "/api/v1/auth/passkey/login/begin", nil, 200)
		opts, err := virtualwebauthn.ParseAssertionOptions(options(res))
		if err != nil {
			t.Fatal(err)
		}
		r := rp
		r.Origin = origin
		resp := virtualwebauthn.CreateAssertionResponse(r, authn, cred, *opts)
		return map[string]any{"ceremony_id": anon.str(res, "ceremony_id"), "credential": json.RawMessage(resp), "token_request": true}
	}

	// registration re-auth
	c.do("POST", "/api/v1/users/me/passkeys/begin", map[string]string{"password": "wrong"}, 401)
	pk := register(c, &authn, cred, "laptop")
	authn.AddCredential(cred)
	if len(c.doArr("GET", "/api/v1/users/me/passkeys", nil, 200)) != 1 {
		t.Fatal("want 1 passkey")
	}
	if _, leaked := pk["credential"]; leaked {
		t.Fatalf("passkey view leaks credential: %v", pk)
	}

	// TOTP on: passkey login still gets a session, no challenge; password login does
	ctx := context.Background()
	u, _ := svc.Store.UserByUsername(ctx, "pk")
	if _, err := svc.Store.SetPendingTOTP(ctx, u.ID, "JBSWY3DPEHPK3PXP"); err != nil {
		t.Fatal(err)
	}
	if err := svc.Store.EnableTOTP(ctx, u.ID); err != nil {
		t.Fatal(err)
	}
	c.do("POST", "/api/v1/users/me/passkeys/begin", map[string]string{"password": "pw-pk"}, 401) // needs a code now

	ok := anon.do("POST", "/api/v1/auth/passkey/login/finish", assertion(pkOrigin), 200)
	sess := &client{t: t, b: ts.URL, tk: anon.str(ok, "token")}
	sess.do("GET", "/api/v1/users/me", nil, 200)
	e := anon.do("POST", "/api/v1/auth/login", map[string]any{"username": "pk", "password": "pw-pk"}, 401)["error"].(map[string]any)
	if e["code"] != "two_factor_required" {
		t.Fatalf("password login error = %v", e)
	}

	// replay of a consumed ceremony, wrong origin
	body := assertion(pkOrigin)
	anon.do("POST", "/api/v1/auth/passkey/login/finish", body, 200)
	anon.do("POST", "/api/v1/auth/passkey/login/finish", body, 401)
	anon.do("POST", "/api/v1/auth/passkey/login/finish", assertion("https://evil.test"), 401)

	// sign-count regression = clone warning
	cred.Counter = 10
	authn.Credentials[0] = cred
	anon.do("POST", "/api/v1/auth/passkey/login/finish", assertion(pkOrigin), 200)
	cred.Counter = 3
	authn.Credentials[0] = cred
	anon.do("POST", "/api/v1/auth/passkey/login/finish", assertion(pkOrigin), 401)
	cred.Counter = 11
	authn.Credentials[0] = cred

	// disabled user
	if err := svc.Users.SetEnabled(ctx, "pk", false); err != nil {
		t.Fatal(err)
	}
	anon.do("POST", "/api/v1/auth/passkey/login/finish", assertion(pkOrigin), 401)
	if err := svc.Users.SetEnabled(ctx, "pk", true); err != nil {
		t.Fatal(err)
	}
	if err := svc.Store.DeleteTOTP(ctx, u.ID); err != nil { // so password login works again
		t.Fatal(err)
	}
	c = pwLogin(anon, "pk") // disabling revoked the sessions

	// ownership boundary
	id := pk["id"].(string)
	other.do("PATCH", "/api/v1/users/me/passkeys/"+id, map[string]string{"name": "x"}, 404)
	other.do("DELETE", "/api/v1/users/me/passkeys/"+id, nil, 404)
	if len(other.doArr("GET", "/api/v1/users/me/passkeys", nil, 200)) != 0 {
		t.Fatal("other user sees pk's passkeys")
	}
	c.do("PATCH", "/api/v1/users/me/passkeys/"+id, map[string]string{"name": "renamed"}, 200)

	// last-passkey guard for a user without a usable password
	if _, err := svc.Store.Q.Exec(ctx, `UPDATE users SET password_hash = '' WHERE id = $1`, u.ID); err != nil {
		t.Fatal(err)
	}
	c.do("DELETE", "/api/v1/users/me/passkeys/"+id, nil, 409)
	if _, err := svc.Store.Q.Exec(ctx, `UPDATE users SET password_hash = (SELECT password_hash FROM users WHERE username = 'other') WHERE id = $1`, u.ID); err != nil {
		t.Fatal(err)
	}
	c.do("DELETE", "/api/v1/users/me/passkeys/"+id, nil, 204)
	anon.do("POST", "/api/v1/auth/passkey/login/finish", assertion(pkOrigin), 401) // deleted credential
}

// TestMobileHandoff: PKCE-bound one-time code → API token.
func TestMobileHandoff(t *testing.T) {
	url := testURL(t)
	ts, svc := newServer(t, url, nil)
	seedUsers(t, svc, "mob")
	anon := &client{t: t, b: ts.URL}
	c := &client{t: t, b: ts.URL, tk: anon.str(anon.do("POST", "/api/v1/auth/login",
		map[string]any{"username": "mob", "password": "pw-mob", "token_request": true}, 200), "token")}

	challenge := func(verifier string) string {
		sum := sha256.Sum256([]byte(verifier))
		return base64.RawURLEncoding.EncodeToString(sum[:])
	}
	code := func(verifier string) string {
		return c.str(c.do("POST", "/api/v1/auth/mobile/code", map[string]string{"code_challenge": challenge(verifier)}, 200), "code")
	}

	c.do("POST", "/api/v1/auth/mobile/code", map[string]string{"code_challenge": "short"}, 400)
	anon.do("POST", "/api/v1/auth/mobile/code", map[string]string{"code_challenge": challenge("v")}, 401)

	cd := code("verifier-1")
	anon.do("POST", "/api/v1/auth/mobile/token", map[string]string{"code": cd, "code_verifier": "wrong"}, 401)
	anon.do("POST", "/api/v1/auth/mobile/token", map[string]string{"code": cd, "code_verifier": "verifier-1"}, 401) // burned by the failed try

	cd = code("verifier-1")
	tok := anon.str(anon.do("POST", "/api/v1/auth/mobile/token", map[string]string{"code": cd, "code_verifier": "verifier-1"}, 200), "token")
	anon.do("POST", "/api/v1/auth/mobile/token", map[string]string{"code": cd, "code_verifier": "verifier-1"}, 401) // reuse

	api := &client{t: t, b: ts.URL, tk: tok}
	api.do("GET", "/api/v1/users/me", nil, 200)
	api.do("POST", "/api/v1/auth/mobile/code", map[string]string{"code_challenge": challenge("v")}, 403) // API tokens can't mint codes
}

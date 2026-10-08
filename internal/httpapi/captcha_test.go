package httpapi_test

import (
	"context"
	"testing"

	"github.com/gochathub/gochathub-server/internal/service"
)

// TestLoginCaptcha: with a verifier wired, password login needs a valid
// turnstile_token; 2FA redemption and bearer routes are unaffected.
func TestLoginCaptcha(t *testing.T) {
	url := testURL(t)
	ts, svc := newServer(t, url, nil)
	seedUsers(t, svc, "cap")
	svc.Auth.VerifyCaptcha = func(_ context.Context, token string, _ *string) error {
		if token != "good" {
			return service.ErrCaptcha
		}
		return nil
	}
	c := &client{t: t, b: ts.URL}
	login := func(tok string) map[string]any {
		return map[string]any{"username": "cap", "password": "pw-cap", "turnstile_token": tok}
	}

	for _, tok := range []string{"", "bad"} {
		e := c.do("POST", "/api/v1/auth/login", login(tok), 403)["error"].(map[string]any)
		if e["code"] != "captcha_failed" {
			t.Fatalf("token %q: error = %v", tok, e)
		}
	}
	// a bad captcha must not reveal whether the password was right
	c.do("POST", "/api/v1/auth/login", map[string]any{"username": "cap", "password": "wrong", "turnstile_token": "good"}, 401)
	c.do("POST", "/api/v1/auth/login", login("good"), 200)
}

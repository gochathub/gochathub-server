package httpapi_test

import (
	"testing"
	"time"

	"github.com/pquerna/otp/totp"
)

// TestTOTPFlow: setup → enable → two-step login, replay, backup codes,
// challenge attempt cap, disable. A fake clock steps 30s per code so replay
// protection (one use per time step) never makes the test wait.
func TestTOTPFlow(t *testing.T) {
	url := testURL(t)
	ts, svc := newServer(t, url, nil)
	seedUsers(t, svc, "tfa")

	clock := time.Now()
	svc.Auth.Now = func() time.Time { return clock }
	code := func(secret string) string {
		t.Helper()
		c, err := totp.GenerateCode(secret, clock)
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	next := func() { clock = clock.Add(30 * time.Second) }

	anon := &client{t: t, b: ts.URL}
	login := map[string]any{"username": "tfa", "password": "pw-tfa", "token_request": true}
	c := &client{t: t, b: ts.URL, tk: anon.str(anon.do("POST", "/api/v1/auth/login", login, 200), "token")}

	if me := c.do("GET", "/api/v1/users/me", nil, 200); me["two_factor_enabled"] != false {
		t.Fatalf("fresh user two_factor_enabled = %v", me["two_factor_enabled"])
	}

	setup := c.do("POST", "/api/v1/users/me/2fa/setup", nil, 200)
	secret := c.str(setup, "secret")
	c.str(setup, "otpauth_url")

	c.do("POST", "/api/v1/users/me/2fa/enable", map[string]string{"code": "000000"}, 400)
	en := c.do("POST", "/api/v1/users/me/2fa/enable", map[string]string{"code": code(secret)}, 200)
	backups, _ := en["backup_codes"].([]any)
	if len(backups) != 10 {
		t.Fatalf("backup_codes = %v", en["backup_codes"])
	}
	c.do("POST", "/api/v1/users/me/2fa/setup", nil, 409) // already enabled
	if me := c.do("GET", "/api/v1/users/me", nil, 200); me["two_factor_enabled"] != true {
		t.Fatalf("two_factor_enabled = %v", me["two_factor_enabled"])
	}

	challenge := func() string {
		t.Helper()
		e := anon.do("POST", "/api/v1/auth/login", login, 401)["error"].(map[string]any)
		if e["code"] != "two_factor_required" {
			t.Fatalf("error = %v", e)
		}
		return e["challenge"].(string)
	}

	// the code that enabled 2FA is spent for its time step (replay)
	anon.do("POST", "/api/v1/auth/login/2fa", map[string]any{"challenge": challenge(), "code": code(secret), "token_request": true}, 401)

	next()
	ok := anon.do("POST", "/api/v1/auth/login/2fa", map[string]any{"challenge": challenge(), "code": code(secret), "token_request": true}, 200)
	anon.str(ok, "token")
	anon.do("POST", "/api/v1/auth/login/2fa", map[string]any{"challenge": challenge(), "code": code(secret), "token_request": true}, 401) // replay

	// backup code: works once
	bc := backups[0].(string)
	anon.do("POST", "/api/v1/auth/login/2fa", map[string]any{"challenge": challenge(), "code": bc, "token_request": true}, 200)
	anon.do("POST", "/api/v1/auth/login/2fa", map[string]any{"challenge": challenge(), "code": bc, "token_request": true}, 401)

	// five wrong codes burn the challenge, even for the right code afterwards
	next()
	ch := challenge()
	for i := 0; i < 5; i++ {
		anon.do("POST", "/api/v1/auth/login/2fa", map[string]any{"challenge": ch, "code": "000000"}, 401)
	}
	anon.do("POST", "/api/v1/auth/login/2fa", map[string]any{"challenge": ch, "code": code(secret)}, 401)

	// regenerate backup codes (old ones die)
	re := c.do("POST", "/api/v1/users/me/2fa/backup-codes", map[string]string{"code": code(secret)}, 200)
	if n, _ := re["backup_codes"].([]any); len(n) != 10 {
		t.Fatalf("regenerated = %v", re)
	}
	anon.do("POST", "/api/v1/auth/login/2fa", map[string]any{"challenge": challenge(), "code": backups[1].(string)}, 401)

	// disable needs password and a fresh code
	next()
	c.do("DELETE", "/api/v1/users/me/2fa", map[string]string{"password": "wrong", "code": code(secret)}, 401)
	c.do("DELETE", "/api/v1/users/me/2fa", map[string]string{"password": "pw-tfa", "code": code(secret)}, 204)
	anon.do("POST", "/api/v1/auth/login", login, 200) // plain login again
}

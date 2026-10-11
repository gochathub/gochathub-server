package httpapi_test

import (
	"context"
	"strings"
	"testing"

	"github.com/gochathub/gochathub-server/internal/service"
)

// TestMailSettings: admin-only, password sealed at rest and never echoed,
// SSRF guard on the host, and signup invites follow the saved configuration.
func TestMailSettings(t *testing.T) {
	url := testURL(t)
	ts, svc := newServer(t, url, nil)
	seedUsers(t, svc, "plain")
	if _, err := svc.Users.CreateUser(context.Background(), "root", "Root", "root@test.invalid", "pw-root", "admin"); err != nil {
		t.Fatal(err)
	}
	anon := &client{t: t, b: ts.URL}
	admin := anon.forUser(svc, "root")
	plain := anon.forUser(svc, "plain")

	anon.do("GET", "/api/v1/admin/mail-settings", nil, 401)
	plain.do("GET", "/api/v1/admin/mail-settings", nil, 403)
	plain.do("PUT", "/api/v1/admin/mail-settings", map[string]string{"host": "h"}, 403)
	plain.do("POST", "/api/v1/admin/mail-settings/test", map[string]string{"to": "a@x.test"}, 403)

	v := admin.do("GET", "/api/v1/admin/mail-settings", nil, 200)
	if v["configured"] != false || v["encryption_available"] != false {
		t.Fatalf("fresh view: %v", v)
	}
	body := map[string]any{"host": "smtp.example.test", "port": "465", "user": "u", "password": "s3cret", "tls": "implicit", "from": "Chat <chat@example.test>"}
	admin.do("PUT", "/api/v1/admin/mail-settings", body, 400) // no SECRETS_KEY

	// feature wiring: key, allow-listed host, capturing transport
	mail := &fakeMailer{}
	var got service.SMTPConfig
	svc.MailCfg.Key = service.NewSecretsKey("test-key")
	svc.MailCfg.AllowHosts = []string{"smtp.example.test"}
	svc.MailCfg.NewSender = func(c service.SMTPConfig) service.Mailer { got = c; return mail }
	svc.BaseOrigin = "https://chat.test"

	admin.do("GET", "/api/v1/admin/signup-invites", nil, 404) // key but no config yet

	for name, mut := range map[string]func(map[string]any){
		"bad port":       func(m map[string]any) { m["port"] = "99999" },
		"bad tls":        func(m map[string]any) { m["tls"] = "ssl" },
		"bad from":       func(m map[string]any) { m["from"] = "nope" },
		"auth plaintext": func(m map[string]any) { m["tls"] = "none" },
		"host with path": func(m map[string]any) { m["host"] = "a.test/x" },
	} {
		bad := map[string]any{}
		for k, val := range body {
			bad[k] = val
		}
		mut(bad)
		t.Run(name, func(t *testing.T) { admin.do("PUT", "/api/v1/admin/mail-settings", bad, 400) })
	}

	v = admin.do("PUT", "/api/v1/admin/mail-settings", body, 200)
	if v["has_password"] != true || v["configured"] != true || v["host"] != "smtp.example.test" {
		t.Fatalf("saved view: %v", v)
	}
	if strings.Contains(toJSON(t, v), "s3cret") {
		t.Fatal("view leaks password")
	}
	raw, err := svc.Store.GetConfigValue(context.Background(), "mail.smtp")
	if err != nil || strings.Contains(raw, "s3cret") {
		t.Fatalf("password stored in the clear (or missing): %q %v", raw, err)
	}

	// test send decrypts the password and uses the saved settings
	admin.do("POST", "/api/v1/admin/mail-settings/test", map[string]string{"to": "me@x.test"}, 204)
	if got.Pass != "s3cret" || got.Host != "smtp.example.test" || got.Port != "465" || got.TLS != "implicit" {
		t.Fatalf("sender config: %+v", got)
	}
	if len(mail.sent) != 1 || mail.sent[0].To != "me@x.test" {
		t.Fatalf("sent: %+v", mail.sent)
	}

	// omitted password keeps the stored one; invites now work
	body2 := map[string]any{"host": "smtp.example.test", "port": "465", "user": "u2", "tls": "implicit", "from": "chat@example.test"}
	admin.do("PUT", "/api/v1/admin/mail-settings", body2, 200)
	admin.do("POST", "/api/v1/admin/signup-invites", map[string]string{"email": "n@x.test"}, 201)
	if got.Pass != "s3cret" || got.User != "u2" {
		t.Fatalf("password not kept: %+v", got)
	}

	// SSRF guard: private host that is not allow-listed is refused at send time
	body2["host"] = "127.0.0.1"
	admin.do("PUT", "/api/v1/admin/mail-settings", body2, 200)
	admin.do("POST", "/api/v1/admin/mail-settings/test", map[string]string{"to": "me@x.test"}, 400)

	// clearing the host switches invites back off
	admin.do("PUT", "/api/v1/admin/mail-settings", map[string]any{"host": ""}, 200)
	admin.do("GET", "/api/v1/admin/signup-invites", nil, 404)
}

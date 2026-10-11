package httpapi_test

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"strings"
	"testing"

	"github.com/gochathub/gochathub-server/internal/service"
)

type sentVia struct {
	provider string
	values   map[string]string
}

func providerView(t *testing.T, v map[string]any, id string) map[string]any {
	t.Helper()
	for _, p := range v["providers"].([]any) {
		if m := p.(map[string]any); m["id"] == id {
			return m
		}
	}
	t.Fatalf("provider %s missing in %v", id, v)
	return nil
}

// TestMailProviders: per-provider storage, secrets sealed and write-only,
// switching keeps other providers' configs, SSRF guard (SMTP), invites follow
// the active provider.
func TestMailProviders(t *testing.T) {
	url := testURL(t)
	ts, svc := newServer(t, url, nil)
	seedUsers(t, svc, "plain")
	if _, err := svc.Users.CreateUser(context.Background(), "root", "Root", "root@test.invalid", "pw-root", "admin"); err != nil {
		t.Fatal(err)
	}
	anon := &client{t: t, b: ts.URL}
	admin := anon.forUser(svc, "root")
	plain := anon.forUser(svc, "plain")
	const base = "/api/v1/admin/mail-settings"

	anon.do("GET", base, nil, 401)
	plain.do("GET", base, nil, 403)
	plain.do("PUT", base, map[string]string{"active": ""}, 403)
	plain.do("PUT", base+"/postmark", map[string]any{"values": map[string]string{}}, 403)
	plain.do("DELETE", base+"/postmark", nil, 403)
	plain.do("POST", base+"/test", map[string]string{"to": "a@x.test"}, 403)

	v := admin.do("GET", base, nil, 200)
	if v["active"] != "" || v["encryption_available"] != false || len(v["providers"].([]any)) != 7 {
		t.Fatalf("fresh view: %v", v)
	}
	pm := map[string]string{"server_token": "pm-secret-token", "message_stream": "invites", "from": "Chat <chat@example.test>"}
	admin.do("PUT", base+"/postmark", map[string]any{"values": pm}, 400) // no SECRETS_KEY

	// wiring: key, allow-listed SMTP host, capturing transport
	mail := &fakeMailer{}
	var via []sentVia
	svc.MailCfg.Key = service.NewSecretsKey("test-key")
	svc.MailCfg.AllowHosts = []string{"smtp.example.test"}
	svc.MailCfg.NewSender = func(p string, vals map[string]string) (service.Mailer, error) {
		via = append(via, sentVia{p, vals})
		return mail, nil
	}
	svc.BaseOrigin = "https://chat.test"

	admin.do("PUT", base+"/nope", map[string]any{"values": pm}, 404)
	for name, c := range map[string]struct {
		provider string
		values   map[string]string
	}{
		"postmark missing token": {"postmark", map[string]string{"from": "c@x.test"}},
		"postmark bad from":      {"postmark", map[string]string{"server_token": "t", "from": "nope"}},
		"ses bad region":         {"ses", map[string]string{"region": "evil.example/x", "access_key_id": "a", "secret_access_key": "s", "from": "c@x.test"}},
		"mailgun bad region":     {"mailgun", map[string]string{"api_key": "k", "domain": "mg.x.test", "region": "mars", "from": "c@x.test"}},
		"smtp bad port":          {"smtp", map[string]string{"host": "smtp.example.test", "port": "99999", "from": "c@x.test"}},
		"smtp login plaintext":   {"smtp", map[string]string{"host": "smtp.example.test", "tls": "none", "user": "u", "password": "p", "from": "c@x.test"}},
		"smtp host with path":    {"smtp", map[string]string{"host": "a.test/x", "from": "c@x.test"}},
	} {
		t.Run(name, func(t *testing.T) {
			admin.do("PUT", base+"/"+c.provider, map[string]any{"values": c.values}, 400)
		})
	}

	// save without activating: configured, mail still off, secret write-only
	v = admin.do("PUT", base+"/postmark", map[string]any{"values": pm}, 200)
	pv := providerView(t, v, "postmark")
	if pv["configured"] != true || v["active"] != "" || pv["has_secret"].(map[string]any)["server_token"] != true {
		t.Fatalf("saved view: %v", v)
	}
	if strings.Contains(toJSON(t, v), "pm-secret-token") {
		t.Fatal("view leaks secret")
	}
	if raw, err := svc.Store.GetConfigValue(context.Background(), "mail.settings"); err != nil || strings.Contains(raw, "pm-secret-token") {
		t.Fatalf("secret stored in the clear (or missing): %q %v", raw, err)
	}
	admin.do("GET", "/api/v1/admin/signup-invites", nil, 404)

	// activate, test send decrypts and routes to the provider
	v = admin.do("PUT", base, map[string]string{"active": "postmark"}, 200)
	if v["active"] != "postmark" {
		t.Fatalf("active: %v", v)
	}
	admin.do("POST", base+"/test", map[string]string{"to": "me@x.test"}, 204)
	if len(via) != 1 || via[0].provider != "postmark" || via[0].values["server_token"] != "pm-secret-token" || via[0].values["message_stream"] != "invites" {
		t.Fatalf("sender wiring: %+v", via)
	}

	// a second provider keeps the first one's config; empty secret keeps stored
	admin.do("PUT", base+"/smtp", map[string]any{"values": map[string]string{
		"host": "smtp.example.test", "port": "465", "tls": "implicit", "user": "u", "password": "smtp-pass", "from": "c@x.test"}, "activate": true}, 200)
	v = admin.do("GET", base, nil, 200)
	if v["active"] != "smtp" || providerView(t, v, "postmark")["configured"] != true {
		t.Fatalf("switch lost config: %v", v)
	}
	admin.do("PUT", base+"/postmark", map[string]any{"values": map[string]string{"server_token": "", "from": "New <new@example.test>"}}, 200)
	admin.do("PUT", base, map[string]string{"active": "postmark"}, 200)
	admin.do("POST", base+"/test", map[string]string{"to": "me@x.test"}, 204)
	if last := via[len(via)-1]; last.values["server_token"] != "pm-secret-token" || last.values["from"] != "New <new@example.test>" {
		t.Fatalf("secret not kept / from not updated: %+v", last)
	}
	admin.do("PUT", base, map[string]string{"active": "sendgrid"}, 400) // not configured

	// invites follow the active provider
	admin.do("POST", "/api/v1/admin/signup-invites", map[string]string{"email": "n@x.test"}, 201)

	// SSRF guard applies to SMTP: private host not allow-listed
	admin.do("PUT", base+"/smtp", map[string]any{"values": map[string]string{"host": "127.0.0.1", "port": "25", "tls": "starttls", "from": "c@x.test"}, "activate": true}, 200)
	admin.do("POST", base+"/test", map[string]string{"to": "me@x.test"}, 400)

	// clearing the active provider switches mail off; other configs survive
	v = admin.do("DELETE", base+"/smtp", nil, 200)
	if v["active"] != "" || providerView(t, v, "smtp")["configured"] != false || providerView(t, v, "postmark")["configured"] != true {
		t.Fatalf("after clear: %v", v)
	}
	admin.do("GET", "/api/v1/admin/signup-invites", nil, 404)
	admin.do("PUT", base, map[string]string{"active": ""}, 200)
}

// seal mirrors MailSettings.seal so a legacy single-provider value can be planted.
func seal(t *testing.T, key *[32]byte, plain string) string {
	t.Helper()
	b, _ := aes.NewCipher(key[:])
	aead, _ := cipher.NewGCM(b)
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(aead.Seal(nonce, nonce, []byte(plain), nil))
}

// TestMailSettingsLegacyUpgrade: the first ADR-026 cut stored one SMTP config
// under mail.smtp; it must keep working as the active smtp provider.
func TestMailSettingsLegacyUpgrade(t *testing.T) {
	url := testURL(t)
	ts, svc := newServer(t, url, nil)
	if _, err := svc.Users.CreateUser(context.Background(), "root", "Root", "root@test.invalid", "pw-root", "admin"); err != nil {
		t.Fatal(err)
	}
	admin := (&client{t: t, b: ts.URL}).forUser(svc, "root")
	key := service.NewSecretsKey("test-key")
	var got sentVia
	svc.MailCfg.Key = key
	svc.MailCfg.AllowHosts = []string{"smtp.example.test"}
	svc.MailCfg.NewSender = func(p string, vals map[string]string) (service.Mailer, error) {
		got = sentVia{p, vals}
		return &fakeMailer{}, nil
	}
	legacy := fmt.Sprintf(`{"host":"smtp.example.test","port":"465","user":"u","pass_enc":%q,"tls":"implicit","from":"c@x.test"}`, seal(t, key, "old-pass"))
	if err := svc.Store.SetConfigValue(context.Background(), "mail.smtp", legacy); err != nil {
		t.Fatal(err)
	}
	v := admin.do("GET", "/api/v1/admin/mail-settings", nil, 200)
	if v["active"] != "smtp" || providerView(t, v, "smtp")["configured"] != true {
		t.Fatalf("legacy not upgraded: %v", v)
	}
	admin.do("POST", "/api/v1/admin/mail-settings/test", map[string]string{"to": "me@x.test"}, 204)
	if got.provider != "smtp" || got.values["password"] != "old-pass" || got.values["port"] != "465" {
		t.Fatalf("legacy sender: %+v", got)
	}
}

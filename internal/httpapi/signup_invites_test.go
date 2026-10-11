package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/gochathub/gochathub-server/internal/service"
)

// fakeMailer captures sent mail; token() digs the invite token out of the link.
type fakeMailer struct {
	mu   sync.Mutex
	sent []service.Mail
	fail bool
}

func (f *fakeMailer) Send(_ context.Context, m service.Mail) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, m)
	if f.fail {
		return context.DeadlineExceeded
	}
	return nil
}

func (f *fakeMailer) Enabled(context.Context) bool { return true }

func (f *fakeMailer) token(t *testing.T, to string) string {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := len(f.sent) - 1; i >= 0; i-- {
		if f.sent[i].To != to {
			continue
		}
		_, rest, ok := strings.Cut(f.sent[i].Body, "/signup#token=")
		if !ok {
			t.Fatalf("no link in %q", f.sent[i].Body)
		}
		tok, _, _ := strings.Cut(rest, "\n")
		return tok
	}
	t.Fatalf("no mail to %s", to)
	return ""
}

// post is a raw anonymous POST so tests can read the status and Set-Cookie.
func post(t *testing.T, base, path string, body any) *http.Response {
	t.Helper()
	raw, _ := json.Marshal(body)
	res, err := http.Post(base+path, "application/json", bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	return res
}

func signupBody(tok, user string) map[string]any {
	return map[string]any{"token": tok, "username": user, "display_name": "Neo " + user,
		"password": "correct-horse-battery", "timezone": "America/Chicago", "primary_color": "#db2777"}
}

func TestSignupInvites(t *testing.T) {
	url := testURL(t)
	ts, svc := newServer(t, url, nil)
	seedUsers(t, svc, "boss", "plain")
	if _, err := svc.Users.CreateUser(context.Background(), "root", "Root", "root@test.invalid", "pw-root", "admin"); err != nil {
		t.Fatal(err)
	}
	anon := &client{t: t, b: ts.URL}
	admin := anon.forUser(svc, "root")
	plain := anon.forUser(svc, "plain")

	// feature off without a mailer: every route 404s
	admin.do("GET", "/api/v1/admin/signup-invites", nil, 404)
	anon.do("POST", "/api/v1/signup/lookup", map[string]string{"token": "x"}, 404)

	mail := &fakeMailer{}
	svc.Signup.Mail = mail
	svc.BaseOrigin = "https://chat.test"

	// authorization boundary: non-admin 403, anonymous 401
	plain.do("GET", "/api/v1/admin/signup-invites", nil, 403)
	plain.do("POST", "/api/v1/admin/signup-invites", map[string]string{"email": "a@x.test"}, 403)
	anon.do("GET", "/api/v1/admin/signup-invites", nil, 401)

	// validation + existing account
	admin.do("POST", "/api/v1/admin/signup-invites", map[string]string{"email": "nope"}, 400)
	admin.do("POST", "/api/v1/admin/signup-invites", map[string]string{"email": "plain@test.invalid"}, 409)

	// create, lookup, complete
	inv := admin.do("POST", "/api/v1/admin/signup-invites", map[string]string{"email": "New@Test.Invalid"}, 201)
	if inv["email"] != "new@test.invalid" || inv["status"] != "open" {
		t.Fatalf("invite view: %v", inv)
	}
	if _, leaked := inv["token"]; leaked {
		t.Fatal("invite view leaks token")
	}
	tok := mail.token(t, "new@test.invalid")
	if !strings.Contains(mail.sent[0].Body, "https://chat.test/signup#token=") {
		t.Fatalf("link base: %q", mail.sent[0].Body)
	}
	anon.do("POST", "/api/v1/signup/lookup", map[string]string{"token": "bogus"}, 404)
	if got := anon.do("POST", "/api/v1/signup/lookup", map[string]string{"token": tok}, 200)["email"]; got != "new@test.invalid" {
		t.Fatalf("lookup email %v", got)
	}

	// bad inputs fail without consuming the token (public routes share the 10-burst login bucket)
	weak := signupBody(tok, "neo")
	weak["password"] = "short"
	anon.do("POST", "/api/v1/signup", weak, 400)
	badTZ := signupBody(tok, "neo")
	badTZ["timezone"] = "Mars/Base"
	anon.do("POST", "/api/v1/signup", badTZ, 400)
	badColor := signupBody(tok, "neo")
	badColor["primary_color"] = "#123456"
	anon.do("POST", "/api/v1/signup", badColor, 400)
	anon.do("POST", "/api/v1/signup", signupBody(tok, "plain"), 409) // username taken

	res := post(t, ts.URL, "/api/v1/signup", signupBody(tok, "neo"))
	if res.StatusCode != 200 {
		t.Fatalf("signup status %d", res.StatusCode)
	}
	cookies := res.Cookies()
	if len(cookies) == 0 {
		t.Fatal("no session cookie")
	}
	req, _ := http.NewRequest("GET", ts.URL+"/api/v1/users/me", nil)
	req.AddCookie(cookies[0])
	me, err := http.DefaultClient.Do(req)
	if err != nil || me.StatusCode != 200 {
		t.Fatalf("cookie session rejected: %v %v", err, me)
	}
	me.Body.Close()
	row, err := svc.Store.UserByUsername(context.Background(), "neo")
	if err != nil || row.Role != "user" || row.Timezone == nil || *row.Timezone != "America/Chicago" ||
		!strings.Contains(string(row.Preferences), "#db2777") {
		t.Fatalf("created user: %+v %v", row, err)
	}

	// token is single-use
	anon.do("POST", "/api/v1/signup", signupBody(tok, "neo2"), 404)
	if st := admin.doArr("GET", "/api/v1/admin/signup-invites", nil, 200)[0].(map[string]any)["status"]; st != "used" {
		t.Fatalf("status %v", st)
	}
}

func TestSignupInviteLifecycle(t *testing.T) {
	url := testURL(t)
	ts, svc := newServer(t, url, nil)
	if _, err := svc.Users.CreateUser(context.Background(), "root", "Root", "root@test.invalid", "pw-root", "admin"); err != nil {
		t.Fatal(err)
	}
	mail := &fakeMailer{}
	svc.Signup.Mail = mail
	svc.BaseOrigin = "https://chat.test"
	anon := &client{t: t, b: ts.URL}
	admin := anon.forUser(svc, "root")

	// re-invite of an open email = resend: one row, old token dead
	a := admin.do("POST", "/api/v1/admin/signup-invites", map[string]string{"email": "r@test.invalid"}, 201)
	old := mail.token(t, "r@test.invalid")
	b := admin.do("POST", "/api/v1/admin/signup-invites", map[string]string{"email": "r@test.invalid"}, 201)
	if a["id"] != b["id"] || len(admin.doArr("GET", "/api/v1/admin/signup-invites", nil, 200)) != 1 {
		t.Fatalf("want one row, got %v / %v", a, b)
	}
	anon.do("POST", "/api/v1/signup/lookup", map[string]string{"token": old}, 404)
	cur := mail.token(t, "r@test.invalid")
	anon.do("POST", "/api/v1/signup/lookup", map[string]string{"token": cur}, 200)

	// explicit resend rotates again
	admin.do("POST", "/api/v1/admin/signup-invites/"+admin.str(a, "id")+"/resend", nil, 200)
	anon.do("POST", "/api/v1/signup/lookup", map[string]string{"token": cur}, 404)
	cur = mail.token(t, "r@test.invalid")

	// revoke kills it; second revoke 404; resend of revoked 404
	admin.do("DELETE", "/api/v1/admin/signup-invites/"+admin.str(a, "id"), nil, 204)
	admin.do("DELETE", "/api/v1/admin/signup-invites/"+admin.str(a, "id"), nil, 404)
	admin.do("POST", "/api/v1/admin/signup-invites/"+admin.str(a, "id")+"/resend", nil, 404)
	anon.do("POST", "/api/v1/signup/lookup", map[string]string{"token": cur}, 404)

	// expiry
	c := admin.do("POST", "/api/v1/admin/signup-invites", map[string]string{"email": "e@test.invalid"}, 201)
	et := mail.token(t, "e@test.invalid")
	if _, err := svc.Store.Q.Exec(context.Background(), `UPDATE signup_invites SET expires_at = now() - interval '1 second' WHERE id = $1`, c["id"]); err != nil {
		t.Fatal(err)
	}
	anon.do("POST", "/api/v1/signup/lookup", map[string]string{"token": et}, 404)
	if st := admin.doArr("GET", "/api/v1/admin/signup-invites", nil, 200)[0].(map[string]any)["status"]; st != "expired" {
		t.Fatalf("status %v", st)
	}

	// CLI user create for an invited email revokes the open invite
	d := admin.do("POST", "/api/v1/admin/signup-invites", map[string]string{"email": "cli@test.invalid"}, 201)
	dt := mail.token(t, "cli@test.invalid")
	if _, err := svc.Users.CreateUser(context.Background(), "cliuser", "Cli", "cli@test.invalid", "pw-cli", "user"); err != nil {
		t.Fatal(err)
	}
	anon.do("POST", "/api/v1/signup/lookup", map[string]string{"token": dt}, 404)
	_ = d

	// mail failure: 502, row kept, resend works once mail recovers
	mail.fail = true
	admin.do("POST", "/api/v1/admin/signup-invites", map[string]string{"email": "m@test.invalid"}, 502)
	mail.fail = false
	var mid string
	for _, v := range admin.doArr("GET", "/api/v1/admin/signup-invites", nil, 200) {
		if m := v.(map[string]any); m["email"] == "m@test.invalid" {
			mid, _ = m["id"].(string)
		}
	}
	admin.do("POST", "/api/v1/admin/signup-invites/"+mid+"/resend", nil, 200)
}

func TestSignupConcurrentSingleUse(t *testing.T) {
	url := testURL(t)
	ts, svc := newServer(t, url, nil)
	if _, err := svc.Users.CreateUser(context.Background(), "root", "Root", "root@test.invalid", "pw-root", "admin"); err != nil {
		t.Fatal(err)
	}
	mail := &fakeMailer{}
	svc.Signup.Mail = mail
	svc.BaseOrigin = "https://chat.test"
	admin := (&client{t: t, b: ts.URL}).forUser(svc, "root")
	admin.do("POST", "/api/v1/admin/signup-invites", map[string]string{"email": "race@test.invalid"}, 201)
	tok := mail.token(t, "race@test.invalid")

	var wg sync.WaitGroup
	var mu sync.Mutex
	ok := 0
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			name := "racer" + string(rune('a'+i))
			if post(t, ts.URL, "/api/v1/signup", signupBody(tok, name)).StatusCode == 200 {
				mu.Lock()
				ok++
				mu.Unlock()
			}
		}(i)
	}
	wg.Wait()
	if ok != 1 {
		t.Fatalf("want exactly 1 account, got %d", ok)
	}
}

func toJSON(t *testing.T, v any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

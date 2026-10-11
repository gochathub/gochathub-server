package mail

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gochathub/gochathub-server/internal/service"
)

type seen struct {
	method, path, body string
	h                  http.Header
	user, pass         string
}

// fake records one request and answers with status/body.
func fake(t *testing.T, status int, reply string) (*httptest.Server, *seen) {
	t.Helper()
	s := &seen{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		s.method, s.path, s.body, s.h = r.Method, r.URL.Path, string(raw), r.Header.Clone()
		s.user, s.pass, _ = r.BasicAuth()
		w.WriteHeader(status)
		io.WriteString(w, reply)
	}))
	t.Cleanup(srv.Close)
	return srv, s
}

var msg = service.Mail{To: "to@x.test", Subject: "Hi", Body: "Open https://chat.test/signup#token=abc\nbye"}

func TestProviderRequests(t *testing.T) {
	ctx := context.Background()
	has := func(t *testing.T, got string, subs ...string) {
		t.Helper()
		for _, s := range subs {
			if !strings.Contains(got, s) {
				t.Errorf("missing %q in %q", s, got)
			}
		}
	}

	t.Run("postmark", func(t *testing.T) {
		srv, s := fake(t, 200, `{"ErrorCode":0}`)
		if err := (&Postmark{Token: "tok", From: "Chat <c@x.test>", BaseURL: srv.URL}).Send(ctx, msg); err != nil {
			t.Fatal(err)
		}
		if s.method != "POST" || s.path != "/email" || s.h.Get("X-Postmark-Server-Token") != "tok" {
			t.Fatalf("%+v", s)
		}
		var pm map[string]string
		if err := json.Unmarshal([]byte(s.body), &pm); err != nil || pm["From"] != "Chat <c@x.test>" || pm["To"] != "to@x.test" || pm["TextBody"] == "" {
			t.Fatalf("postmark body: %s", s.body)
		}
	})
	t.Run("sendgrid", func(t *testing.T) {
		srv, s := fake(t, 202, ``)
		if err := (&SendGrid{Key: "SG.k", From: "Chat <c@x.test>", BaseURL: srv.URL}).Send(ctx, msg); err != nil {
			t.Fatal(err)
		}
		if s.path != "/v3/mail/send" || s.h.Get("Authorization") != "Bearer SG.k" {
			t.Fatalf("%+v", s)
		}
		has(t, s.body, `"personalizations"`, `"email":"to@x.test"`, `"email":"c@x.test"`, `"name":"Chat"`, `"text/plain"`)
	})
	t.Run("resend", func(t *testing.T) {
		srv, s := fake(t, 200, `{"id":"1"}`)
		if err := (&Resend{Key: "re_k", From: "c@x.test", BaseURL: srv.URL}).Send(ctx, msg); err != nil {
			t.Fatal(err)
		}
		if s.path != "/emails" || s.h.Get("Authorization") != "Bearer re_k" {
			t.Fatalf("%+v", s)
		}
		has(t, s.body, `"to":["to@x.test"]`, `"text"`)
	})
	t.Run("brevo", func(t *testing.T) {
		srv, s := fake(t, 201, `{"messageId":"m"}`)
		if err := (&Brevo{Key: "bk", From: "Chat <c@x.test>", BaseURL: srv.URL}).Send(ctx, msg); err != nil {
			t.Fatal(err)
		}
		if s.path != "/v3/smtp/email" || s.h.Get("api-key") != "bk" {
			t.Fatalf("%+v", s)
		}
		var bv struct {
			Sender                   map[string]string
			HTMLContent, TextContent string
		}
		if err := json.Unmarshal([]byte(s.body), &bv); err != nil || bv.Sender["email"] != "c@x.test" || bv.Sender["name"] != "Chat" || bv.TextContent == "" {
			t.Fatalf("brevo body: %s", s.body)
		}
		has(t, bv.HTMLContent, `<a href="https://chat.test/signup#token=abc">`)
	})
	t.Run("mailgun", func(t *testing.T) {
		srv, s := fake(t, 200, `{"id":"1"}`)
		if err := (&Mailgun{Key: "mgk", Domain: "mg.x.test", Region: "us", From: "c@x.test", BaseURL: srv.URL}).Send(ctx, msg); err != nil {
			t.Fatal(err)
		}
		if s.path != "/v3/mg.x.test/messages" || s.user != "api" || s.pass != "mgk" {
			t.Fatalf("%+v", s)
		}
		has(t, s.body, "to=to%40x.test", "subject=Hi", "from=c%40x.test")
	})
	t.Run("ses", func(t *testing.T) {
		srv, s := fake(t, 200, `{"MessageId":"m"}`)
		if err := (&SES{Region: "us-east-1", KeyID: "AKIDEXAMPLE", Secret: "secret", From: "c@x.test", BaseURL: srv.URL}).Send(ctx, msg); err != nil {
			t.Fatal(err)
		}
		if s.path != "/v2/email/outbound-emails" {
			t.Fatalf("%+v", s)
		}
		has(t, s.h.Get("Authorization"), "AWS4-HMAC-SHA256", "Credential=AKIDEXAMPLE/", "/us-east-1/ses/aws4_request")
		has(t, s.body, `"ToAddresses":["to@x.test"]`, `"FromEmailAddress":"c@x.test"`)
	})
	t.Run("ses rejects bad region", func(t *testing.T) {
		if err := (&SES{Region: "evil.example.com/x", KeyID: "a", Secret: "b", From: "c@x.test"}).Send(ctx, msg); err == nil {
			t.Fatal("want region error")
		}
	})
	t.Run("provider error surfaces", func(t *testing.T) {
		srv, _ := fake(t, 401, `{"Message":"Bad token"}`)
		err := (&Postmark{Token: "x", From: "c@x.test", BaseURL: srv.URL}).Send(ctx, msg)
		if err == nil || !strings.Contains(err.Error(), "401") || !strings.Contains(err.Error(), "Bad token") {
			t.Fatalf("got %v", err)
		}
	})
}

func TestNewUnknownProvider(t *testing.T) {
	if _, err := New("nope", nil); err == nil {
		t.Fatal("want error")
	}
	for _, p := range []string{"smtp", "postmark", "sendgrid", "resend", "brevo", "mailgun", "ses"} {
		if _, err := New(p, map[string]string{}); err != nil {
			t.Fatalf("%s: %v", p, err)
		}
	}
}

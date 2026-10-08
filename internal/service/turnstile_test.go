package service

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// fake siteverify: success only for token "good"; echoes a hostname.
func fakeSiteverify(t *testing.T, hostname, action string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.FormValue("secret") != "s3cret" {
			t.Errorf("secret = %q", r.FormValue("secret"))
		}
		json.NewEncoder(w).Encode(map[string]any{
			"success":  r.FormValue("response") == "good",
			"hostname": hostname,
			"action":   action,
		})
	}))
}

func TestTurnstileVerify(t *testing.T) {
	ctx := context.Background()
	srv := fakeSiteverify(t, "chat.example.com", "login")
	defer srv.Close()

	tv := &Turnstile{Secret: "s3cret", URL: srv.URL}
	if err := tv.Verify(ctx, "good", nil); err != nil {
		t.Fatalf("good token: %v", err)
	}
	if err := tv.Verify(ctx, "bad", nil); err == nil {
		t.Fatal("bad token accepted")
	}
	if err := tv.Verify(ctx, "", nil); err == nil {
		t.Fatal("empty token accepted")
	}

	// hostname pinning: only enforced when configured
	tv.Hostname = "chat.example.com"
	if err := tv.Verify(ctx, "good", nil); err != nil {
		t.Fatalf("matching hostname: %v", err)
	}
	tv.Hostname = "evil.example.com"
	if err := tv.Verify(ctx, "good", nil); err == nil {
		t.Fatal("wrong hostname accepted")
	}

	// a token minted for another action must not unlock login
	other := fakeSiteverify(t, "chat.example.com", "signup")
	defer other.Close()
	if err := (&Turnstile{Secret: "s3cret", URL: other.URL}).Verify(ctx, "good", nil); err == nil {
		t.Fatal("wrong action accepted")
	}

	// siteverify unreachable → closed
	srv.Close()
	if err := (&Turnstile{Secret: "s3cret", URL: srv.URL}).Verify(ctx, "good", nil); err == nil {
		t.Fatal("unreachable siteverify accepted")
	}
}

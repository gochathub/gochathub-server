package httpapi

import (
	"net/http/httptest"
	"testing"
)

// The webhook secret rides in the URL path and must never reach logs.
func TestLogPathRedactsWebhookSecret(t *testing.T) {
	for in, want := range map[string]string{
		"/hooks/abc/SECRET": "/hooks/abc/***",
		"/hooks/abc/":       "/hooks/abc/***",
		"/hooks/abc":        "/hooks/abc/***",
		"/api/v1/rooms":     "/api/v1/rooms",
	} {
		if got := logPath(httptest.NewRequest("POST", in, nil)); got != want {
			t.Errorf("logPath(%q) = %q, want %q", in, got, want)
		}
	}
}

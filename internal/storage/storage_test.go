package storage

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// fakeS3 answers the bucket-exists HEAD and counts every request it sees.
func fakeS3(t *testing.T) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

// Behind an upstream TLS proxy the server talks to MinIO internally but the
// browser needs presigned URLs signed for the public host.
func TestPresignUsesPublicEndpointWhileCallsStayInternal(t *testing.T) {
	srv, hits := fakeS3(t)
	internal := strings.TrimPrefix(srv.URL, "http://")

	s, err := NewS3(context.Background(), internal, "https://chat.example.com", "us-east-1", "gochathub", "ak", "sk", false)
	if err != nil {
		t.Fatalf("NewS3: %v", err)
	}
	if hits.Load() == 0 {
		t.Fatal("bucket check never reached the internal endpoint")
	}

	for name, sign := range map[string]func() (string, error){
		"get": func() (string, error) { return s.PresignGet(context.Background(), "k") },
		"put": func() (string, error) { return s.PresignPut(context.Background(), "k", "text/plain", 1) },
	} {
		u, err := sign()
		if err != nil {
			t.Fatalf("presign %s: %v", name, err)
		}
		if !strings.HasPrefix(u, "https://chat.example.com/gochathub/k?") {
			t.Errorf("presign %s = %q, want https://chat.example.com/gochathub/k?...", name, u)
		}
	}
}

func TestPresignDefaultsToInternalEndpoint(t *testing.T) {
	srv, _ := fakeS3(t)
	internal := strings.TrimPrefix(srv.URL, "http://")

	s, err := NewS3(context.Background(), internal, "", "us-east-1", "gochathub", "ak", "sk", false)
	if err != nil {
		t.Fatalf("NewS3: %v", err)
	}
	u, err := s.PresignGet(context.Background(), "k")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(u, srv.URL+"/gochathub/k?") {
		t.Errorf("presign = %q, want prefix %s/gochathub/k?", u, srv.URL)
	}
}

package id

import (
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestNewIDIsUUIDv7(t *testing.T) {
	for i := 0; i < 16; i++ {
		s := NewID()
		u, err := uuid.Parse(s)
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		if u.Version() != 7 {
			t.Fatalf("not v7: %s", s)
		}
	}
}

func TestNewTokenShape(t *testing.T) {
	for i := 0; i < 16; i++ {
		tok := NewToken()
		if len(tok) != 43 {
			t.Fatalf("token length %d, want 43 (32 bytes raw url base64)", len(tok))
		}
		if strings.ContainsAny(tok, "+/=") {
			t.Fatalf("token contains non-url-safe chars: %s", tok)
		}
		if NewToken() == tok {
			t.Fatal("tokens repeated")
		}
	}
}

func TestHashTokenStable(t *testing.T) {
	if string(HashToken("abc")) == string(HashToken("abd")) {
		t.Fatal("different tokens hash equal")
	}
	if HashToken("abc") == nil {
		t.Fatal("nil hash")
	}
}

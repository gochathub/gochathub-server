package service

import (
	"testing"
	"time"

	"github.com/gochathub/gochathub-server/internal/store"
)

func TestCursorRoundtrip(t *testing.T) {
	in := store.Cursor{At: time.Now().UTC().Truncate(time.Microsecond), ID: "018f6f2a-0000-7000-8000-000000000000"}
	enc := encodeCursor(in)
	out, err := decodeCursor(enc)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !out.At.Equal(in.At) || out.ID != in.ID {
		t.Fatalf("roundtrip mismatch: %+v != %+v", out, in)
	}
}

func TestDecodeCursorRejectsGarbage(t *testing.T) {
	for _, s := range []string{"", "!!!", "AAAA", "v1:notanumber:018f6f2a-0000-7000-8000-000000000000", "v2:1:018f6f2a-0000-7000-8000-000000000000"} {
		if _, err := decodeCursor(s); err == nil {
			t.Errorf("cursor %q accepted", s)
		}
	}
}

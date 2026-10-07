package pwd

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"golang.org/x/crypto/bcrypt"
)

// rocketHash builds a migrated Rocket.Chat hash: bcrypt over the hex SHA-256
// digest string (Meteor accounts-password scheme).
func rocketHash(t *testing.T, password string) string {
	t.Helper()
	sum := sha256.Sum256([]byte(password))
	h, err := bcrypt.GenerateFromPassword([]byte(hex.EncodeToString(sum[:])), bcrypt.MinCost)
	if err != nil {
		t.Fatalf("bcrypt: %v", err)
	}
	return BCryptPrefix + string(h)
}

func TestVerifyLegacyBcrypt(t *testing.T) {
	stored := rocketHash(t, "hunter2")
	if want := "bcrypt$"; len(stored) < len(want) || stored[:len(want)] != want {
		t.Fatalf("missing prefix: %s", stored)
	}
	ok, err := Verify(stored, "hunter2")
	if err != nil || !ok {
		t.Fatalf("legacy bcrypt verify failed: ok=%v err=%v", ok, err)
	}
	if ok, err := Verify(stored, "wrong"); err != nil || ok {
		t.Fatalf("wrong password accepted: ok=%v err=%v", ok, err)
	}
}

func TestHashVerifyRoundtrip(t *testing.T) {
	h, err := Hash("correct horse battery staple")
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	if want := "argon2id$"; len(h) < len(want) || h[:len(want)] != want {
		t.Fatalf("hash missing prefix: %s", h)
	}
	ok, err := Verify(h, "correct horse battery staple")
	if err != nil || !ok {
		t.Fatalf("verify roundtrip failed: ok=%v err=%v", ok, err)
	}
	ok, err = Verify(h, "wrong password")
	if err != nil {
		t.Fatalf("verify error: %v", err)
	}
	if ok {
		t.Fatal("wrong password accepted")
	}
}

func TestVerifyRejectsHostileParams(t *testing.T) {
	// 1 GiB memory claim in a stored hash would DoS login; must refuse.
	hostile := "argon2id$1,262145,4,32$AAAAAAAAAAAAAAAAAAAAAA$BBBBBBBBBBBBBBBBBBBB"
	if _, err := Verify(hostile, "x"); err == nil {
		t.Fatal("hostile memory param accepted")
	}
	craft := "argon2id$1,65536,999,32$AAAAAAAAAAAAAAAAAAAAAA$BBBBBBBBBBBBBBBBBBBB"
	if _, err := Verify(craft, "x"); err == nil {
		t.Fatal("hostile threads param accepted")
	}
	if _, err := Verify("not-a-hash", "x"); err == nil {
		t.Fatal("malformed hash accepted")
	}
}

func TestVerifyWrongFormat(t *testing.T) {
	if _, err := Verify("", "x"); err == nil {
		t.Fatal("empty hash accepted")
	}
}

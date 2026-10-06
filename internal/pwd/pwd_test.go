package pwd

import "testing"

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

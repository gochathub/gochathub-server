package push

// Live ntfy integration test (docs/UNIFIEDPUSH.md [verify] items §3.4/§3.5).
//
//	TEST_NTFY_URL=https://ntfy.example.com \
//	TEST_NTFY_TOKEN=tk_... \
//	TEST_NTFY_TOPIC=upverify \
//	go test ./internal/push/ -run TestLiveNTFY -count=1 -v
//
// Verifies against the self-hosted ntfy:
//  1. RFC 8030 send (TTL, Urgency, Content-Encoding: aes128gcm) is accepted.
//  2. An RFC 8291-encrypted message survives the server and the base64
//     re-encoding of the `up=1` flag and decrypts to the original payload.
//  3. Dead topics do NOT yield 404/410 from ntfy — sends are always 2xx —
//     so the sender cannot liveness-probe via status codes; cleanup is
//     distributor-driven (30-day no-ack unregister).

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	webpush "github.com/SherClockHolmes/webpush-go"

	"github.com/gochathub/gochathub-server/internal/store"
	"github.com/gochathub/gochathub-server/internal/testutil"
)

const testPayload = `{"type":"chat.message","room_id":"r1","message_id":"m2"}`

func TestLiveNTFY(t *testing.T) {
	baseURL := strings.TrimSuffix(os.Getenv("TEST_NTFY_URL"), "/")
	token, topic := os.Getenv("TEST_NTFY_TOKEN"), os.Getenv("TEST_NTFY_TOPIC")
	if baseURL == "" || token == "" || topic == "" {
		t.Skip("TEST_NTFY_URL/TEST_NTFY_TOKEN/TEST_NTFY_TOPIC not set")
	}

	// receiver (client device) keys per RFC 8291: p256dh + auth secret
	priv, p256dh, auth, err := testutil.ReceiverKey()
	if err != nil {
		t.Fatal(err)
	}

	s := &Sender{
		Subscriber:    "mailto:live-test@chat.test",
		NtfyQueryFlag: "up",
		HTTP:          &http.Client{Timeout: 20 * time.Second},
	}
	// VAPID pair direct-set (same state EnsureVAPID persists).
	vPriv, vPub, err := webpush.GenerateVAPIDKeys()
	if err != nil {
		t.Fatalf("vapid gen: %v", err)
	}
	s.keyPair = &KeyPair{Private: vPriv, Public: vPub}

	e := store.PushEndpointRow{
		ID:         "live-test",
		Endpoint:   baseURL + "/" + topic,
		PublicKey:  base64.RawURLEncoding.EncodeToString(p256dh),
		AuthSecret: base64.RawURLEncoding.EncodeToString(auth),
	}

	// subscribe BEFORE sending (no replay on a fresh stream).
	wireCh := sseSubscribe(t, baseURL, topic, token)

	status, err := s.send(context.Background(), e, []byte(testPayload), webpush.UrgencyHigh)
	if err != nil {
		t.Fatalf("push send: status=%d err=%v", status, err)
	}
	if status/100 != 2 {
		t.Fatalf("push send status %d", status)
	}

	select {
	case wire := <-wireCh:
		got, err := testutil.DecryptRFC8291(wire, priv, p256dh, auth)
		if err != nil {
			t.Fatalf("decrypt: %v (wire=%q)", err, wire)
		}
		if string(got) != testPayload {
			t.Fatalf("roundtrip mismatch: got %q want %q", got, testPayload)
		}
	case <-time.After(25 * time.Second):
		t.Fatal("no message event within 25s")
	}

	// dead topic — no subscriber, unknown topic name.
	dead := store.PushEndpointRow{ID: "dead", Endpoint: fmt.Sprintf("%s/updead_%d", baseURL, time.Now().UnixNano()),
		PublicKey: e.PublicKey, AuthSecret: e.AuthSecret}
	deadStatus, deadErr := s.send(context.Background(), dead, []byte("{}"), webpush.UrgencyNormal)
	if deadErr == nil && deadStatus/100 != 2 {
		t.Fatalf("dead topic expected 2xx, got %d", deadStatus)
	}
	t.Logf("dead topic: status=%d err=%v (ntfy never 404s unknown topics)", deadStatus, deadErr)
}

// sseSubscribe starts an SSE subscription immediately and returns the first
// message event body over a channel.
func sseSubscribe(t *testing.T, baseURL, topic, token string) <-chan []byte {
	t.Helper()
	req, _ := http.NewRequest("GET", baseURL+"/"+topic+"/sse", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("sse: %v", err)
	}
	ch := make(chan []byte, 1)
	go func() {
		defer resp.Body.Close()
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
		for sc.Scan() {
			line := sc.Text()
			after, ok := strings.CutPrefix(line, "data: {")
			if !ok {
				continue
			}
			var ev struct {
				Message string `json:"message"`
				Event   string `json:"event"`
			}
			if json.Unmarshal([]byte("{"+after), &ev) != nil || ev.Event != "message" || ev.Message == "" {
				continue
			}
			clean := strings.ReplaceAll(ev.Message, "\n", "")
			b, err := base64.StdEncoding.DecodeString(clean)
			if err != nil {
				b = []byte(ev.Message)
			}
			ch <- b
			return
		}
	}()
	return ch
}

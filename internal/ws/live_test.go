package ws_test

// Live integration test against a running server:
//
//	TEST_WS_URL=ws://127.0.0.1:18123/api/v1/ws \
//	TEST_API=http://127.0.0.1:18123/api/v1 \
//	TEST_TOKEN_A=... TEST_TOKEN_B=... \
//	TEST_ROOM_ID=... go test ./internal/ws/
//
// Verifies: connected frame, unauthorized-room subscribe rejection (error
// envelope), and message.created fan-out to a subscribed member. Skipped
// when env is unset (tests stay deterministic by default).

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/gochathub/gochathub-server/internal/model"
)

func TestSubscribeFanoutAndAuthz(t *testing.T) {
	base := os.Getenv("TEST_WS_URL")
	apiBase := os.Getenv("TEST_API") // base path to post messages as alice
	tokA, tokB, roomID := os.Getenv("TEST_TOKEN_A"), os.Getenv("TEST_TOKEN_B"), os.Getenv("TEST_ROOM_ID")
	if base == "" || tokA == "" || tokB == "" || roomID == "" || apiBase == "" {
		t.Skip("TEST_WS_URL/TEST_API/TEST_TOKEN_A/TEST_TOKEN_B/TEST_ROOM_ID not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	dial := func(tok string) *websocket.Conn {
		c, resp, err := websocket.Dial(ctx, base, &websocket.DialOptions{
			HTTPHeader: http.Header{"Authorization": []string{"Bearer " + tok}},
		})
		if err != nil {
			status := 0
			if resp != nil {
				status = resp.StatusCode
			}
			t.Fatalf("dial: %v (status %d)", err, status)
		}
		return c
	}

	bob := dial(tokB)
	defer func() { _ = bob.Close(websocket.StatusNormalClosure, "") }()

	mustFrame(t, ctx, bob, "connected")

	// authorization boundary: subscribing to an unknown room is rejected
	send(t, ctx, bob, map[string]string{"type": "subscribe", "room_id": "01900000-0000-7000-8000-000000000000"})
	mustFrame(t, ctx, bob, "error")

	// membership subscribe, then a message from alice arrives as event
	send(t, ctx, bob, map[string]string{"type": "subscribe", "room_id": roomID})
	body, _ := json.Marshal(map[string]string{"body": "ws fanout test"})
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, apiBase+"/rooms/"+roomID+"/messages", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+tokA)
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil || res.StatusCode != 201 {
		t.Fatalf("post message: status=%v err=%v", statusCodeOf(res), err)
	}
	res.Body.Close()

	env := mustFrame(t, ctx, bob, "message.created")
	if env.RoomID != roomID {
		t.Fatalf("event room %q != %q", env.RoomID, roomID)
	}
}

func mustFrame(t *testing.T, ctx context.Context, c *websocket.Conn, want string) model.WSEnvelope {
	t.Helper()
	_, raw, err := c.Read(ctx)
	if err != nil {
		t.Fatalf("read frame: %v", err)
	}
	var env model.WSEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("bad frame json: %v", err)
	}
	if env.Type != want {
		t.Fatalf("want frame %q got %q", want, env.Type)
	}
	return env
}

func send(t *testing.T, ctx context.Context, c *websocket.Conn, v any) {
	t.Helper()
	raw, _ := json.Marshal(v)
	if err := c.Write(ctx, websocket.MessageText, raw); err != nil {
		t.Fatalf("write: %v", err)
	}
}

func statusCodeOf(r *http.Response) int {
	if r == nil {
		return 0
	}
	return r.StatusCode
}

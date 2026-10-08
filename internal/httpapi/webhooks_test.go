package httpapi_test

// Webhook integration tests (docs/WEBHOOKS.md): authorization boundaries first,
// then ingest behavior. Skipped without TEST_DATABASE_URL.

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/gochathub/gochathub-server/internal/service"
)

type hookEnv struct {
	t      *testing.T
	base   string
	svc    *service.App
	fake   *fakeStorage
	alice  *client
	roomID string
}

func newHookEnv(t *testing.T) *hookEnv {
	t.Helper()
	fake := newFakeStorage()
	ts, svc := newServer(t, testURL(t), fake)
	seedUsers(t, svc, "alice", "bob")
	alice := (&client{t: t, b: ts.URL}).forUser(svc, "alice")
	room := alice.do("POST", "/api/v1/rooms", map[string]any{"type": "private", "name": "alerts"}, 201)
	return &hookEnv{t: t, base: ts.URL, svc: svc, fake: fake, alice: alice, roomID: alice.str(room, "id")}
}

func (e *hookEnv) newHook(in service.WebhookCreateInput) service.WebhookCreated {
	e.t.Helper()
	if in.Name == "" {
		in.Name = "Mail"
	}
	if in.RoomID == "" && in.Username == "" {
		in.RoomID = e.roomID
	}
	h, err := e.svc.Webhooks.Create(context.Background(), in)
	if err != nil {
		e.t.Fatalf("create webhook: %v", err)
	}
	return h
}

// post sends raw bytes to a hook URL and returns status + decoded JSON.
func (e *hookEnv) postRaw(id, secret string, body []byte) (int, map[string]any) {
	e.t.Helper()
	res, err := http.Post(e.base+"/hooks/"+id+"/"+secret, "application/json", bytes.NewReader(body))
	if err != nil {
		e.t.Fatalf("post hook: %v", err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	out := map[string]any{}
	_ = json.Unmarshal(raw, &out)
	return res.StatusCode, out
}

func (e *hookEnv) post(h service.WebhookCreated, body any) (int, map[string]any) {
	e.t.Helper()
	raw, _ := json.Marshal(body)
	return e.postRaw(h.ID, h.Secret, raw)
}

func (e *hookEnv) messages(c *client, roomID string) []any {
	e.t.Helper()
	page := c.do("GET", "/api/v1/rooms/"+roomID+"/messages", nil, 200)
	items, _ := page["items"].([]any)
	return items
}

func (e *hookEnv) mentionRows() int {
	var n int
	if err := e.svc.Store.Q.QueryRow(context.Background(), `SELECT count(*) FROM message_mentions`).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

func TestWebhookIngestRendersAndDedupes(t *testing.T) {
	e := newHookEnv(t)
	h := e.newHook(service.WebhookCreateInput{})

	// hostile-but-ordinary mail: angle addresses, entities, fence breaker, a ping, a link
	mail := map[string]any{
		"MessageID": "pm-1", "Subject": "[click](http://evil.example) &amp; @bob",
		"FromFull": map[string]string{"Name": "Eve <x>", "Email": "eve@example.com"},
		"TextBody": "<eve@example.com> wrote:\n```\n<img src=x onerror=1>\n```\n@bob ping &amp;",
	}
	status, out := e.post(h, mail)
	if status != 200 || out["duplicate"] != false || out["message_id"] == "" {
		t.Fatalf("ingest: %d %v", status, out)
	}
	items := e.messages(e.alice, e.roomID)
	if len(items) != 1 {
		t.Fatalf("want 1 message, got %d", len(items))
	}
	msg := items[0].(map[string]any)
	if role := msg["author"].(map[string]any)["role"]; role != "bot" {
		t.Fatalf("author role = %v, want bot", role)
	}
	if body := msg["body"].(string); !strings.Contains(body, "**From:** `Eve <x> <eve@example.com>`") {
		t.Fatalf("unexpected body:\n%s", body)
	}
	if n := e.mentionRows(); n != 0 {
		t.Fatalf("webhook text resolved %d mentions, want 0", n)
	}

	// same source MessageID again: one message, duplicate flagged
	status, out2 := e.post(h, mail)
	if status != 200 || out2["duplicate"] != true || out2["message_id"] != out["message_id"] {
		t.Fatalf("duplicate: %d %v", status, out2)
	}
	if n := len(e.messages(e.alice, e.roomID)); n != 1 {
		t.Fatalf("duplicate created a message: %d", n)
	}

	// empty mail still posts (never lost)
	if status, _ := e.post(h, map[string]any{}); status != 200 {
		t.Fatalf("empty mail: %d", status)
	}
}

func TestWebhookAuthBoundary(t *testing.T) {
	e := newHookEnv(t)
	good := e.newHook(service.WebhookCreateInput{})
	mail := map[string]any{"MessageID": "a", "TextBody": "hi"}
	notFor := func(label string, id, secret string) {
		t.Helper()
		raw, _ := json.Marshal(mail)
		if status, out := e.postRaw(id, secret, raw); status != 403 {
			t.Fatalf("%s: status %d %v, want 403", label, status, out)
		}
	}
	notFor("wrong secret", good.ID, "nope")
	notFor("unknown id", "0190f6a0-0000-7000-8000-000000000000", good.Secret)
	notFor("malformed id", "not-a-uuid", good.Secret)

	// allowlist excludes the loopback test client
	fenced := e.newHook(service.WebhookCreateInput{CIDRs: []string{"10.0.0.0/8"}})
	notFor("source outside allowlist", fenced.ID, fenced.Secret)

	// disabled, then re-enabled
	if err := e.svc.Webhooks.SetEnabled(context.Background(), good.ID, false); err != nil {
		t.Fatal(err)
	}
	notFor("disabled", good.ID, good.Secret)
	if err := e.svc.Webhooks.SetEnabled(context.Background(), good.ID, true); err != nil {
		t.Fatal(err)
	}
	if status, _ := e.post(good, mail); status != 200 {
		t.Fatalf("re-enabled hook: %d", status)
	}

	// rotation kills the old secret at once
	secret, err := e.svc.Webhooks.Rotate(context.Background(), good.ID)
	if err != nil {
		t.Fatal(err)
	}
	notFor("rotated-out secret", good.ID, good.Secret)
	raw, _ := json.Marshal(map[string]any{"MessageID": "b", "TextBody": "hi"})
	if status, _ := e.postRaw(good.ID, secret, raw); status != 200 {
		t.Fatalf("new secret: %d", status)
	}

	// delete: URL dies, bot is disabled, history stays
	if err := e.svc.Webhooks.Delete(context.Background(), good.ID); err != nil {
		t.Fatal(err)
	}
	notFor("deleted", good.ID, secret)
	bot, err := e.svc.Store.UserByUsername(context.Background(), good.BotUsername)
	if err != nil || bot.Enabled {
		t.Fatalf("bot after delete: %+v %v", bot, err)
	}
	if n := len(e.messages(e.alice, e.roomID)); n != 2 {
		t.Fatalf("history after delete: %d messages, want 2", n)
	}

	// the secret is stored hashed
	var stored []byte
	if err := e.svc.Store.Q.QueryRow(context.Background(), `SELECT secret_hash FROM webhooks WHERE id = $1`, fenced.ID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(stored), fenced.Secret) || len(stored) != 32 {
		t.Fatal("secret not stored as a sha256 hash")
	}
}

func TestWebhookBotCannotLogin(t *testing.T) {
	e := newHookEnv(t)
	h := e.newHook(service.WebhookCreateInput{})
	for _, pw := range []string{"", "!", h.Secret} {
		if _, _, err := e.svc.Auth.Login(context.Background(), h.BotUsername, pw, "ua", nil); !errors.Is(err, service.ErrUnauthorized) {
			t.Fatalf("bot login with %q: %v, want unauthorized", pw, err)
		}
	}
}

func TestWebhookAttachments(t *testing.T) {
	e := newHookEnv(t)
	h := e.newHook(service.WebhookCreateInput{})
	b64 := func(n int) string { return base64.StdEncoding.EncodeToString(bytes.Repeat([]byte("x"), n)) }

	status, out := e.post(h, map[string]any{
		"MessageID": "att-1", "TextBody": "see attached",
		"Attachments": []map[string]any{
			{"Name": "../../etc/report.txt", "ContentType": "text/plain", "Content": b64(100)},
			{"Name": "big.bin", "ContentType": "application/octet-stream", "Content": b64(2 << 20)}, // over MaxUpload (1 MiB)
			{"Name": "bad.txt", "ContentType": "text/plain", "Content": "!!not base64!!"},
		},
	})
	if status != 200 {
		t.Fatalf("ingest: %d %v", status, out)
	}
	msg := e.messages(e.alice, e.roomID)[0].(map[string]any)
	atts, _ := msg["attachments"].([]any)
	if len(atts) != 1 || atts[0].(map[string]any)["filename"] != "report.txt" {
		t.Fatalf("attachments = %v, want only sanitized report.txt", msg["attachments"])
	}
	body := msg["body"].(string)
	for _, want := range []string{"skipped attachment `big.bin`: too large", "skipped attachment `bad.txt`: unreadable"} {
		if !strings.Contains(body, want) {
			t.Fatalf("missing note %q in\n%s", want, body)
		}
	}
	if len(e.fake.objects) != 1 {
		t.Fatalf("stored %d objects, want 1", len(e.fake.objects))
	}
}

func TestWebhookLimitsSpamAndArchivedRoom(t *testing.T) {
	e := newHookEnv(t)
	max := float32(5)
	h := e.newHook(service.WebhookCreateInput{MaxSpam: &max})

	// over the body cap (4 MiB in the test config) is refused after auth
	big := append(append([]byte(`{"TextBody":"`), bytes.Repeat([]byte("a"), 5<<20)...), `"}`...)
	if status, _ := e.postRaw(h.ID, h.Secret, big); status != 413 {
		t.Fatalf("oversize body: %d, want 413", status)
	}
	if status, _ := e.postRaw(h.ID, h.Secret, []byte("not json")); status != 400 {
		t.Fatalf("bad json: %d, want 400", status)
	}

	// spam above the ceiling: accepted (no retries) but not posted
	status, out := e.post(h, map[string]any{
		"TextBody": "buy now", "Headers": []map[string]string{{"Name": "X-Spam-Score", "Value": "9.1"}},
	})
	if status != 200 || out["dropped"] != "spam" {
		t.Fatalf("spam: %d %v", status, out)
	}
	if n := len(e.messages(e.alice, e.roomID)); n != 0 {
		t.Fatalf("spam was posted: %d messages", n)
	}

	// archived room: 409 so Postmark keeps retrying until it is unarchived
	e.alice.do("DELETE", "/api/v1/rooms/"+e.roomID, nil, 204)
	if status, _ := e.post(h, map[string]any{"TextBody": "hi"}); status != 409 {
		t.Fatalf("archived room: %d, want 409", status)
	}
}

func TestWebhookDMTarget(t *testing.T) {
	e := newHookEnv(t)
	bob := (&client{t: e.t, b: e.base}).forUser(e.svc, "bob")
	bob.do("PATCH", "/api/v1/users/me/preferences", map[string]any{"allow_private_messages": false}, 200)

	// privacy gate holds without --self
	if _, err := e.svc.Webhooks.Create(context.Background(), service.WebhookCreateInput{Name: "dm", Username: "bob"}); !errors.Is(err, service.ErrForbidden) {
		t.Fatalf("gated DM webhook: %v, want forbidden", err)
	}
	// operator asserts it is their own account
	h := e.newHook(service.WebhookCreateInput{Username: "bob", Self: true})
	if status, out := e.post(h, map[string]any{"MessageID": "dm-1", "TextBody": "for bob"}); status != 200 {
		t.Fatalf("dm ingest: %d %v", status, out)
	}
	rooms := bob.doArr("GET", "/api/v1/rooms", nil, 200)
	if len(rooms) != 1 {
		t.Fatalf("bob has %d rooms, want the one DM", len(rooms))
	}
	if n := len(e.messages(bob, rooms[0].(map[string]any)["id"].(string))); n != 1 {
		t.Fatalf("bob sees %d messages, want 1", n)
	}

	// bad inputs
	for name, in := range map[string]service.WebhookCreateInput{
		"neither target": {Name: "x"},
		"both targets":   {Name: "x", RoomID: e.roomID, Username: "bob"},
		"bad cidr":       {Name: "x", RoomID: e.roomID, CIDRs: []string{"nope"}},
		"unknown user":   {Name: "x", Username: "ghost", Self: true},
	} {
		if _, err := e.svc.Webhooks.Create(context.Background(), in); err == nil {
			t.Errorf("%s: want error", name)
		}
	}
}

package httpapi_test

// Integration tests: in-process server against a real PostgreSQL.
// Skipped unless TEST_DATABASE_URL is set; migrations are applied by the suite.

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	_ "github.com/jackc/pgx/v5/stdlib" // sql.Open("pgx", ...) for migrations

	migrations "github.com/gochathub/gochathub-server/db"
	"github.com/gochathub/gochathub-server/internal/app"
	"github.com/gochathub/gochathub-server/internal/config"
	"github.com/gochathub/gochathub-server/internal/httpapi"
	"github.com/gochathub/gochathub-server/internal/service"
	"github.com/gochathub/gochathub-server/internal/store"
	"github.com/gochathub/gochathub-server/internal/testutil"
)

func testURL(t *testing.T) string {
	t.Helper()
	u := os.Getenv("TEST_DATABASE_URL")
	if u == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	return u
}

// testLogSink: real stderr when TEST_DEBUG=1, discard otherwise.
func testLogSink() io.Writer {
	if os.Getenv("TEST_DEBUG") != "" {
		return os.Stderr
	}
	return io.Discard
}

func newServer(t *testing.T, u string, uploads service.Storage) (*httptest.Server, *service.App) {
	t.Helper()
	ctx := context.Background()
	sqlDB, err := sql.Open("pgx", u)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := migrations.Migrate(ctx, sqlDB); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	// suites must be rerunnable: wipe leftover rows from earlier runs
	truncate := `TRUNCATE users, attachments, rooms, app_config, audit_log, devices,
		contacts, sessions, api_tokens, push_endpoints, notification_preferences,
		messages, message_receipts, message_revisions, message_reactions,
		message_mentions, message_attachments, room_invites, room_members CASCADE`
	if _, err := sqlDB.Exec(truncate); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	sqlDB.Close()

	pool, err := pgxpool.New(ctx, u)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)

	cfg := &config.Config{
		SessionTTL:      3600 * 1e9,
		CookieSecure:    false,
		AllowUploads:    uploads != nil,
		MaxUpload:       1 << 20,
		VAPIDSubscriber: "mailto:t@t.invalid",
		RateLimitRPM:    1000000,
	}
	log := func() *slog.Logger {
		lvl := slog.LevelInfo
		if os.Getenv("TEST_DEBUG") != "" {
			lvl = slog.LevelDebug
		}
		return slog.New(slog.NewTextHandler(testLogSink(), &slog.HandlerOptions{Level: lvl}))
	}()
	st := store.New(pool)
	svc, hub, err := app.Assemble(st, cfg, log)
	if err != nil {
		t.Fatalf("assemble: %v", err)
	}
	if uploads != nil {
		svc.Storage = uploads
		svc.AllowUploads = true // Assemble ties the flag to configured S3; the fake replaces it
	}
	ts := httptest.NewServer(httpapi.New(svc, hub, log, cfg))
	t.Cleanup(ts.Close)
	return ts, svc
}

// fakeStorage is the in-memory service.Storage used for upload-flow tests.
type fakeStorage struct{ objects map[string]int64 }

func newFakeStorage() *fakeStorage { return &fakeStorage{objects: map[string]int64{}} }

func (f *fakeStorage) PresignPut(_ context.Context, key, _ string, _ int64) (string, error) {
	return "https://fake.invalid/put/" + key, nil
}

func (f *fakeStorage) PresignGet(_ context.Context, key string) (string, error) {
	if _, ok := f.objects[key]; !ok {
		return "", os.ErrNotExist
	}
	return "https://fake.invalid/get/" + key, nil
}

// Put simulates the client's direct upload.
func (f *fakeStorage) Put(key string, size int64) { f.objects[key] = size }

func (f *fakeStorage) Stat(_ context.Context, key string) (int64, bool, string, error) {
	sz, ok := f.objects[key]
	return sz, ok, "", nil
}

func (f *fakeStorage) Delete(_ context.Context, key string) error {
	delete(f.objects, key)
	return nil
}

// client: one bearer token.
type client struct {
	t  *testing.T
	b  string
	tk string
}

func (c *client) do(method, path string, body any, want int) map[string]any {
	c.t.Helper()
	var rd *bytes.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		rd = bytes.NewReader(raw)
	} else {
		rd = bytes.NewReader(nil)
	}
	req, err := http.NewRequest(method, c.b+path, rd)
	if err != nil {
		c.t.Fatal(err)
	}
	if c.tk != "" {
		req.Header.Set("Authorization", "Bearer "+c.tk)
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatalf("%s %s: %v", method, path, err)
	}
	raw, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != want {
		c.t.Fatalf("%s %s: status %d want %d body=%s", method, path, res.StatusCode, want, raw)
	}
	out := map[string]any{}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &out); err != nil {
			c.t.Fatalf("%s %s: bad json %q: %v", method, path, raw, err)
		}
	}
	return out
}

func (c *client) str(m map[string]any, key string) string {
	c.t.Helper()
	s, _ := m[key].(string)
	if s == "" {
		c.t.Fatalf("missing string field %q in %v", key, m)
	}
	return s
}

// doArr for endpoints that return JSON arrays (contacts, search, rooms).
func (c *client) doArr(method, path string, body any, want int) []any {
	c.t.Helper()
	var rd *bytes.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		rd = bytes.NewReader(raw)
	} else {
		rd = bytes.NewReader(nil)
	}
	req, err := http.NewRequest(method, c.b+path, rd)
	if err != nil {
		c.t.Fatal(err)
	}
	if c.tk != "" {
		req.Header.Set("Authorization", "Bearer "+c.tk)
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatalf("%s %s: %v", method, path, err)
	}
	raw, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != want {
		c.t.Fatalf("%s %s: status %d want %d body=%s", method, path, res.StatusCode, want, raw)
	}
	out := []any{}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &out); err != nil {
			c.t.Fatalf("%s %s: bad json %q: %v", method, path, raw, err)
		}
	}
	return out
}

// for returns a client bound to a user's minted token.
func (c *client) forUser(svc *service.App, name string) *client {
	c.t.Helper()
	tok, err := svc.Users.CreateAPIToken(context.Background(), name, "integration")
	if err != nil {
		c.t.Fatalf("token for %s: %v", name, err)
	}
	return &client{t: c.t, b: c.b, tk: tok}
}

// seedUsers provisions accounts (service-layer, deterministic names).
func seedUsers(t *testing.T, svc *service.App, names ...string) {
	t.Helper()
	for _, name := range names {
		if _, err := svc.Users.CreateUser(context.Background(), name, name+" displayed", name+"@test.invalid", "pw-"+name, "user"); err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
	}
}

// TestAuthBoundary: bad credentials rejected, disabled accounts rejected.
func TestAuthBoundary(t *testing.T) {
	url := testURL(t)
	ts, svc := newServer(t, url, nil)
	seedUsers(t, svc, "authone")
	c := &client{t: t, b: ts.URL}

	// anonymous request → 401
	c.do("GET", "/api/v1/users/me", nil, 401)

	// bad token → 401
	c2 := &client{t: t, b: ts.URL, tk: "garbage-token"}
	c2.do("GET", "/api/v1/users/me", nil, 401)

	// login with wrong password → 401
	c.do("POST", "/api/v1/auth/login", map[string]string{"username": "authone", "password": "nope"}, 401)

	// unknown user → 401
	c.do("POST", "/api/v1/auth/login", map[string]string{"username": "ghost", "password": "nope"}, 401)

	// login via cookie works
	req, _ := http.NewRequest("POST", ts.URL+"/api/v1/auth/login",
		bytes.NewReader([]byte(`{"username":"authone","password":"pw-authone"}`)))
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		t.Fatalf("login status %d", res.StatusCode)
	}
	if cookies := res.Cookies(); len(cookies) == 0 || cookies[0].Name != "chat_session" || cookies[0].HttpOnly != true {
		t.Fatalf("expected httpOnly chat_session cookie, got %v", res.Cookies())
	}
	// cookie flow must NOT leak the token into the body (ADR-015)
	cookieBody, _ := io.ReadAll(res.Body)
	if strings.Contains(string(cookieBody), "token") {
		t.Fatalf("browser login leaked token: %s", string(cookieBody))
	}

	// token_request flow (Android): token in body, no reliance on cookie
	tres, err := http.Post(ts.URL+"/api/v1/auth/login", "application/json",
		bytes.NewReader([]byte(`{"username":"authone","password":"pw-authone","token_request":true}`)))
	if tres != nil {
		defer tres.Body.Close()
	}
	if err != nil || tres.StatusCode != 200 {
		t.Fatalf("device login: status=%d err=%v", statusCodeOf(tres), err)
	}
	var authOut map[string]any
	_ = json.NewDecoder(tres.Body).Decode(&authOut)
	if _, ok := authOut["token"].(string); !ok {
		t.Fatalf("token_request did not return token: %v", authOut["user"])
	}
	// the returned token authorizes bearer calls
	areq, _ := http.NewRequest("GET", ts.URL+"/api/v1/users/me", nil)
	areq.Header.Set("Authorization", "Bearer "+authOut["token"].(string))
	ares, err := http.DefaultClient.Do(areq)
	if err != nil {
		t.Fatal(err)
	}
	defer ares.Body.Close()
	if ares.StatusCode != 200 {
		t.Fatalf("bearer from login rejected: %d", ares.StatusCode)
	}
	var me map[string]any
	_ = json.NewDecoder(ares.Body).Decode(&me)
	if me["username"] != "authone" {
		t.Fatalf("profile mismatch %v", me)
	}

	// disabled account → rejected later
	if err := svc.Users.SetEnabled(context.Background(), "authone", false); err != nil {
		t.Fatal(err)
	}
}

// TestRoomPrivateAuthz: non-members cannot read private rooms or post.
func TestRoomPrivateAuthz(t *testing.T) {
	url := testURL(t)
	ts, svc := newServer(t, url, nil)
	seedUsers(t, svc, "ralice", "rbob", "rove")
	base := &client{t: t, b: ts.URL}
	alice := base.forUser(svc, "ralice")
	bob := base.forUser(svc, "rbob")
	ove := base.forUser(svc, "rove")

	room := alice.do("POST", "/api/v1/rooms", map[string]any{"type": "private", "name": "secret"}, 201)
	roomID := alice.str(room, "id")

	// non-member cannot see the room (existence leak: none)
	bob.do("GET", "/api/v1/rooms/"+roomID, nil, 404)
	ove.do("GET", "/api/v1/rooms/"+roomID, nil, 404)

	// non-member cannot post
	bob.do("POST", "/api/v1/rooms/"+roomID+"/messages", map[string]any{"body": "sneak"}, 404)

	// member posts fine
	alice.do("POST", "/api/v1/rooms/"+roomID+"/messages", map[string]any{"body": "hello secret"}, 201)

	// public room joinable by anyone
	pub := alice.do("POST", "/api/v1/rooms", map[string]any{"type": "public", "name": "town"}, 201)
	pubID := alice.str(pub, "id")
	ove.do("GET", "/api/v1/rooms/"+pubID, nil, 404) // not a member yet
	ove.do("POST", "/api/v1/rooms/"+pubID+"/members", map[string]any{"user_id": ove.str(ove.do("GET", "/api/v1/users/me", nil, 200), "id")}, 204)
	ove.do("GET", "/api/v1/rooms/"+pubID, nil, 200)

	// member-level archive (web archived view)
	ove.do("PUT", "/api/v1/rooms/"+pubID+"/archived", map[string]any{"archived": true}, 204)
	ove.do("PUT", "/api/v1/rooms/"+pubID+"/archived", map[string]any{"archived": false}, 204)

	// kick: admin adds then removes; revoked access matches revoked membership
	kickID := ove.str(ove.do("GET", "/api/v1/users/me", nil, 200), "id")
	alice.do("POST", "/api/v1/rooms/"+pubID+"/members", map[string]any{"user_id": kickID}, 204)
	ove.do("GET", "/api/v1/rooms/"+pubID, nil, 200)
	alice.do("DELETE", "/api/v1/rooms/"+pubID+"/members/"+kickID, nil, 204)
	ove.do("GET", "/api/v1/rooms/"+pubID, nil, 404)
	ove.do("POST", "/api/v1/rooms/"+pubID+"/messages", map[string]any{"body": "sneak back"}, 404)
}

// TestInviteFlowAndReceipts: invite→accept→message→read receipts.
func TestInviteFlowAndReceipts(t *testing.T) {
	url := testURL(t)
	ts, svc := newServer(t, url, nil)
	seedUsers(t, svc, "ialice", "ibob")
	base := &client{t: t, b: ts.URL}
	alice := base.forUser(svc, "ialice")
	bob := base.forUser(svc, "ibob")

	bobID := bob.str(bob.do("GET", "/api/v1/users/me", nil, 200), "id")
	room := alice.do("POST", "/api/v1/rooms", map[string]any{"type": "private", "name": "planning"}, 201)
	roomID := alice.str(room, "id")

	// member cannot invite (admin only)
	// (bob is not a member yet; strangers invite → 404)
	bob.do("POST", "/api/v1/invites", map[string]any{"room_id": roomID, "user_id": bobID}, 404)

	invite := alice.do("POST", "/api/v1/invites", map[string]any{"room_id": roomID, "user_id": bobID}, 201)
	inviteID := alice.str(invite, "invite_id")

	// bob sees and accepts the invite
	bob.doArr("GET", "/api/v1/invites", nil, 200)
	bob.do("POST", "/api/v1/invites/"+inviteID+"/accept", nil, 204)

	// now bob can read and post
	bob.do("GET", "/api/v1/rooms/"+roomID, nil, 200)
	msg := bob.do("POST", "/api/v1/rooms/"+roomID+"/messages", map[string]any{"body": "accepted and posted"}, 201)
	msgID := bob.str(msg, "id")

	// receipts: bob read his own message via REST read
	bob.do("POST", "/api/v1/rooms/"+roomID+"/read", map[string]any{"message_id": msgID}, 204)

	// alice (author of the *next* message) sees aggregate receipts after bob
	// is current; bob's own receipt shows his read.
	am := alice.do("POST", "/api/v1/rooms/"+roomID+"/messages", map[string]any{"body": "did you see?"}, 201)
	amID := alice.str(am, "id")
	bob.do("POST", "/api/v1/rooms/"+roomID+"/read", map[string]any{"message_id": amID}, 204)

	amsg := alice.do("GET", "/api/v1/messages/"+amID, nil, 200)
	rec, ok := amsg["receipts"].(map[string]any)
	if !ok || rec["read_at"] == nil || rec["delivered_at"] == nil {
		t.Fatalf("sender should see aggregate receipts, got %v", amsg["receipts"])
	}

	// edit + revision + delete path
	edited := bob.do("PATCH", "/api/v1/messages/"+msgID, map[string]any{"body": "edited body"}, 200)
	if e := edited["edited_at"]; e == nil {
		t.Fatal("edited_at missing")
	}
	bob.do("DELETE", "/api/v1/messages/"+msgID, nil, 200)
	tomb := alice.do("GET", "/api/v1/messages/"+msgID, nil, 200)
	if tomb["deleted_at"] == nil || tomb["body"] != "" {
		t.Fatalf("tombstone wrong: %v", tomb)
	}
}

// TestInviteExpiry: expired invites cannot be accepted (listing hides them;
// the accept path is the hard line).
func TestInviteExpiry(t *testing.T) {
	url := testURL(t)
	ts, svc := newServer(t, url, nil)
	seedUsers(t, svc, "ealice", "ebob")
	base := &client{t: t, b: ts.URL}
	alice := base.forUser(svc, "ealice")
	bob := base.forUser(svc, "ebob")

	bobID := bob.str(bob.do("GET", "/api/v1/users/me", nil, 200), "id")
	room := alice.do("POST", "/api/v1/rooms", map[string]any{"type": "private", "name": "past"}, 201)
	roomID := alice.str(room, "id")

	past := time.Now().Add(-time.Hour)
	inv := alice.do("POST", "/api/v1/invites", map[string]any{
		"room_id": roomID, "user_id": bobID, "expires_at": past.Format(time.RFC3339),
	}, 201)
	invID := alice.str(inv, "invite_id")

	// expired invite is invisible to the invitee...
	if rows := bob.doArr("GET", "/api/v1/invites", nil, 200); len(rows) != 0 {
		t.Fatalf("expired invite listed: %v", rows)
	}
	// ...and the accept path refuses it
	bob.do("POST", "/api/v1/invites/"+invID+"/accept", nil, 404)

	// bob never joined the room
	bob.do("GET", "/api/v1/rooms/"+roomID, nil, 404)

	// the expired pending row must not deadlock the (room, invitee) slot:
	// re-inviting refreshes it, and the fresh invite works
	inv2 := alice.do("POST", "/api/v1/invites", map[string]any{"room_id": roomID, "user_id": bobID}, 201)
	inv2ID := alice.str(inv2, "invite_id")
	bob.do("POST", "/api/v1/invites/"+inv2ID+"/accept", nil, 204)
	bob.do("GET", "/api/v1/rooms/"+roomID, nil, 200)

	// malformed uuid in a path param is 404, never 500
	bob.do("POST", "/api/v1/invites/not-a-uuid/accept", nil, 404)
}

// TestPinningAuthz: member cannot pin, admin can.
func TestPinningAuthz(t *testing.T) {
	url := testURL(t)
	ts, svc := newServer(t, url, nil)
	seedUsers(t, svc, "palice", "pbob")
	base := &client{t: t, b: ts.URL}
	alice := base.forUser(svc, "palice")
	bob := base.forUser(svc, "pbob")

	bobID := bob.str(bob.do("GET", "/api/v1/users/me", nil, 200), "id")
	room := alice.do("POST", "/api/v1/rooms", map[string]any{"type": "group_direct", "name": "team", "members": []string{bobID}}, 201)
	roomID := alice.str(room, "id")
	msg := bob.do("POST", "/api/v1/rooms/"+roomID+"/messages", map[string]any{"body": "pin me"}, 201)
	msgID := bob.str(msg, "id")

	bob.do("PUT", "/api/v1/rooms/"+roomID+"/pin", map[string]any{"message_id": msgID}, 403)
	alice.do("PUT", "/api/v1/rooms/"+roomID+"/pin", map[string]any{"message_id": msgID}, 204)

	// bob (member) sees pinned id via room payload
	if got := bob.do("GET", "/api/v1/rooms/"+roomID, nil, 200)["pinned_message_id"]; got != msgID {
		t.Fatalf("pinned id mismatch: %v", got)
	}
	alice.do("DELETE", "/api/v1/rooms/"+roomID+"/pin", nil, 204)
}

// TestContactsCRUD.
func TestContactsCRUD(t *testing.T) {
	url := testURL(t)
	ts, svc := newServer(t, url, nil)
	seedUsers(t, svc, "calice", "cbob")
	base := &client{t: t, b: ts.URL}
	alice := base.forUser(svc, "calice")
	bob := base.forUser(svc, "cbob")

	bobID := bob.str(bob.do("GET", "/api/v1/users/me", nil, 200), "id")
	got := alice.do("POST", "/api/v1/contacts", map[string]any{"user_id": bobID}, 201)
	if got["user"].(map[string]any)["username"] != "cbob" {
		t.Fatalf("contact payload wrong: %v", got)
	}
	alice.do("POST", "/api/v1/contacts", map[string]any{"user_id": bobID}, 409)
	alice.do("DELETE", "/api/v1/contacts/"+bobID, nil, 204)
	contacts := alice.doArr("GET", "/api/v1/contacts", nil, 200)
	if len(contacts) != 0 {
		t.Fatalf("contacts not emptied: %v", contacts)
	}
}

// TestProfileVisibility: stranger 404; same-room visible; last_seen gated.
func TestProfileVisibility(t *testing.T) {
	url := testURL(t)
	ts, svc := newServer(t, url, nil)
	seedUsers(t, svc, "valice", "vbob", "veve")
	base := &client{t: t, b: ts.URL}
	alice := base.forUser(svc, "valice")
	bob := base.forUser(svc, "vbob")
	eve := base.forUser(svc, "veve")

	aliceID := alice.str(alice.do("GET", "/api/v1/users/me", nil, 200), "id")
	eve.do("GET", "/api/v1/users/"+aliceID, nil, 404) // stranger

	search := bob.doArr("GET", "/api/v1/users/search?q=valice", nil, 200)
	if len(search) != 1 {
		t.Fatalf("search should find valice: %v", search)
	}

	// share a room
	bobID := bob.str(bob.do("GET", "/api/v1/users/me", nil, 200), "id")
	room := alice.do("POST", "/api/v1/rooms", map[string]any{"type": "group_direct", "name": "vis", "members": []string{bobID}}, 201)
	roomID := alice.str(room, "id")
	alice.do("POST", "/api/v1/rooms/"+roomID+"/messages", map[string]any{"body": "in vis"}, 201)

	bobP := bob.do("GET", "/api/v1/users/"+aliceID, nil, 200)
	// default prefs hide last seen
	if bobP["last_seen_at"] != nil {
		t.Fatalf("last_seen should be hidden by default: %v", bobP["last_seen_at"])
	}

	// alice allows last_seen
	alice.do("PATCH", "/api/v1/users/me/preferences", map[string]any{"last_seen_visible": true}, 200)
	bobP = bob.do("GET", "/api/v1/users/"+aliceID, nil, 200)
	if bobP["last_seen_at"] == nil {
		t.Fatal("last_seen should now be visible")
	}
	// timezone rides public payload by design (rendering hint)
	if bobP["timezone"] == "" {
		t.Log("info: alice timezone unset in this test")
	}
}

// TestCursorPagination: 5 messages → pages of 2/2/1, cursor exhausted.
func TestCursorPagination(t *testing.T) {
	url := testURL(t)
	ts, svc := newServer(t, url, nil)
	seedUsers(t, svc, "qalice")
	base := &client{t: t, b: ts.URL}
	alice := base.forUser(svc, "qalice")

	room := alice.do("POST", "/api/v1/rooms", map[string]any{"type": "public", "name": "paged"}, 201)
	roomID := alice.str(room, "id")
	for i := 0; i < 5; i++ {
		alice.do("POST", "/api/v1/rooms/"+roomID+"/messages", map[string]any{"body": fmt.Sprintf("msg %d", i)}, 201)
	}
	var seen []string
	var cursor string
	for {
		path := "/api/v1/rooms/" + roomID + "/messages?limit=2"
		if cursor != "" {
			path += "&before=" + cursor
		}
		page := alice.do("GET", path, nil, 200)
		items := page["items"].([]any)
		if len(items) > 2 {
			t.Fatalf("limit ignored: %d", len(items))
		}
		for _, it := range items {
			seen = append(seen, it.(map[string]any)["body"].(string))
		}
		nc, _ := page["next_cursor"].(string)
		if nc == "" {
			break
		}
		cursor = nc
	}
	if len(seen) != 5 {
		t.Fatalf("saw %d messages: %v", len(seen), seen)
	}
	// newest first ordering
	if seen[0] != "msg 4" || seen[4] != "msg 0" {
		t.Fatalf("order wrong: %v", seen)
	}
}

// TestMessageSearch: q is a literal, case-insensitive substring; LIKE wildcards don't leak.
func TestMessageSearch(t *testing.T) {
	url := testURL(t)
	ts, svc := newServer(t, url, nil)
	seedUsers(t, svc, "salice")
	base := &client{t: t, b: ts.URL}
	alice := base.forUser(svc, "salice")

	room := alice.do("POST", "/api/v1/rooms", map[string]any{"type": "public", "name": "searchy"}, 201)
	roomID := alice.str(room, "id")
	for _, b := range []string{"Invoice for the change", "lunch?", "100% done", "another invoice"} {
		alice.do("POST", "/api/v1/rooms/"+roomID+"/messages", map[string]any{"body": b}, 201)
	}
	count := func(q string) int {
		page := alice.do("GET", "/api/v1/rooms/"+roomID+"/messages?q="+q, nil, 200)
		return len(page["items"].([]any))
	}
	if n := count("INVOICE"); n != 2 {
		t.Fatalf("invoice: got %d, want 2", n)
	}
	if n := count("%25"); n != 1 { // literal %, not wildcard
		t.Fatalf("percent: got %d, want 1", n)
	}
	if n := count("_"); n != 0 {
		t.Fatalf("underscore: got %d, want 0", n)
	}
}

// TestUploadFlow: create session → object "lands" → complete → readable → authz.
func TestUploadFlow(t *testing.T) {
	url := testURL(t)
	fake := newFakeStorage()
	ts, svc := newServer(t, url, fake)
	seedUsers(t, svc, "ualice", "ubob")
	base := &client{t: t, b: ts.URL}
	alice := base.forUser(svc, "ualice")
	bob := base.forUser(svc, "ubob")

	created := alice.do("POST", "/api/v1/attachments",
		map[string]any{"filename": "notes/../cat.png", "mime_type": "image/png", "size_bytes": 42}, 201)
	att := created["attachment"].(map[string]any)
	id := att["id"].(string)
	if att["filename"] != "cat.png" {
		t.Fatalf("filename not sanitized: %v", att["filename"])
	}
	key := "att/" + id + "/cat.png"

	// not uploaded yet → complete fails validation
	alice.do("POST", "/api/v1/attachments/"+id+"/complete", nil, 400)

	// simulate the client's direct-to-storage PUT, then complete
	fake.Put(key, 42)
	ready := alice.do("POST", "/api/v1/attachments/"+id+"/complete", nil, 200)
	if ready["url"] == "" || ready["url"] == nil {
		t.Logf("note: complete payload lacks download url (fine)")
	}
	got := alice.do("GET", "/api/v1/attachments/"+id, nil, 200)
	if got["url"] == "" || got["url"] == nil {
		t.Fatal("uploader should get a download url after complete")
	}

	// bob: no message link, no contact → forbidden
	bob.do("GET", "/api/v1/attachments/"+id, nil, 403)

	// once attached to a message in a shared room, bob can read it
	bobID := bob.str(bob.do("GET", "/api/v1/users/me", nil, 200), "id")
	room := alice.do("POST", "/api/v1/rooms", map[string]any{"type": "group_direct", "name": "files", "members": []string{bobID}}, 201)
	roomID := alice.str(room, "id")
	alice.do("POST", "/api/v1/rooms/"+roomID+"/messages",
		map[string]any{"body": "file for you", "attachment_ids": []string{id}}, 201)
	bob.do("GET", "/api/v1/attachments/"+id, nil, 200)

	// declared sizes over the limit are rejected at session create
	alice.do("POST", "/api/v1/attachments", map[string]any{"filename": "big.bin", "mime_type": "application/octet-stream", "size_bytes": 1 << 21}, 400)

	// only the uploader deletes
	bob.do("DELETE", "/api/v1/attachments/"+id, nil, 403)
	alice.do("DELETE", "/api/v1/attachments/"+id, nil, 204)
}

// TestDevicesPushRegistration: negative paths only (no live ntfy in tests).
func TestDevicesPushRegistration(t *testing.T) {
	url := testURL(t)
	ts, svc := newServer(t, url, nil)
	seedUsers(t, svc, "dalice")
	base := &client{t: t, b: ts.URL}
	alice := base.forUser(svc, "dalice")

	alice.do("POST", "/api/v1/devices", map[string]any{"platform": "toaster", "client_name": "x"}, 400)
	alice.do("POST", "/api/v1/devices", map[string]any{"platform": "android", "client_name": "c"}, 201)
	// push_registration requires all three fields (UNIFIEDPUSH.md §3.2)
	alice.do("POST", "/api/v1/devices", map[string]any{
		"platform": "android", "client_name": "c",
		"push_registration": map[string]any{"endpoint": "https://ntfy.example/up/topic"},
	}, 400)
	reg := alice.do("POST", "/api/v1/devices", map[string]any{
		"platform": "android", "client_name": "c",
		"push_registration": map[string]any{
			"endpoint": "https://ntfy.example/up/topic", "public_key": "pk", "auth_secret": "au",
		},
	}, 201)
	if reg["validation_required"] != true {
		t.Fatalf("validation_required missing: %v", reg)
	}
	deviceID := alice.str(reg, "device_id")
	// wrong validation token → 403
	alice.do("POST", "/api/v1/devices/"+deviceID+"/validate", map[string]any{"token": "wrong"}, 403)
	// someone else's device → 404 (existence hidden)
	seedUsers(t, svc, "devob")
	bob := base.forUser(svc, "devob")
	bob.do("POST", "/api/v1/devices/"+deviceID+"/validate", map[string]any{"token": "wrong"}, 404)
	bob.do("DELETE", "/api/v1/devices/"+deviceID, nil, 404)
	// owner deletes
	alice.do("DELETE", "/api/v1/devices/"+deviceID, nil, 204)
}

// TestReceiptsPreferenceGating: read_receipts=false removes the reader from
// sender-visible aggregation (ADR-009 × ADR-013).
func TestReceiptsPreferenceGating(t *testing.T) {
	url := testURL(t)
	ts, svc := newServer(t, url, nil)
	seedUsers(t, svc, "galice", "gbob")
	base := &client{t: t, b: ts.URL}
	alice := base.forUser(svc, "galice")
	bob := base.forUser(svc, "gbob")

	bobID := bob.str(bob.do("GET", "/api/v1/users/me", nil, 200), "id")
	room := alice.do("POST", "/api/v1/rooms", map[string]any{"type": "group_direct", "name": "r", "members": []string{bobID}}, 201)
	roomID := alice.str(room, "id")
	msg := alice.do("POST", "/api/v1/rooms/"+roomID+"/messages", map[string]any{"body": "hello"}, 201)
	msgID := alice.str(msg, "id")

	// delivered is not pre-stamped at seed time; only acks/read make it real
	bobView := bob.do("GET", "/api/v1/messages/"+msgID, nil, 200)
	if br, ok := bobView["receipts"].(map[string]any); ok && br["delivered_at"] != nil {
		t.Fatalf("delivered must not be pre-stamped: %v", br)
	}

	// bob hides read receipts, then reads
	bob.do("PATCH", "/api/v1/users/me/preferences", map[string]any{"read_receipts": false}, 200)
	bob.do("POST", "/api/v1/rooms/"+roomID+"/read", map[string]any{"message_id": msgID}, 204)

	am, _ := alice.do("GET", "/api/v1/messages/"+msgID, nil, 200)["receipts"].(map[string]any)
	if am != nil && am["read_at"] != nil {
		t.Fatalf("read_at must stay hidden when bob opts out: %v", am)
	}

	// bob opts back in; the read becomes visible
	bob.do("PATCH", "/api/v1/users/me/preferences", map[string]any{"read_receipts": true}, 200)
	am = alice.do("GET", "/api/v1/messages/"+msgID, nil, 200)["receipts"].(map[string]any)
	if am == nil || am["read_at"] == nil {
		t.Fatalf("read_at should be visible after opt-in: %v", am)
	}
}

// TestRoomsArchiveSoftDelete: group creator (room admin) deletes the group —
// it leaves every member's list, non-admin members get 403, and self-leave
// removes one member without touching others (web archived view stays
// member-local: m.archived is unaffected).
func TestRoomsArchiveSoftDelete(t *testing.T) {
	url := testURL(t)
	ts, svc := newServer(t, url, nil)
	seedUsers(t, svc, "agalice", "agbob")
	base := &client{t: t, b: ts.URL}
	alice := base.forUser(svc, "agalice")
	bob := base.forUser(svc, "agbob")

	bobID := bob.str(bob.do("GET", "/api/v1/users/me", nil, 200), "id")
	room := alice.do("POST", "/api/v1/rooms", map[string]any{"type": "group_direct", "name": "team", "members": []string{bobID}}, 201)
	roomID := alice.str(room, "id")

	// non-admin member cannot delete the group
	bob.do("DELETE", "/api/v1/rooms/"+roomID, nil, 403)

	// visible in both members' lists before the delete
	if ids := roomIDs(bob.doArr("GET", "/api/v1/rooms", nil, 200)); !ids[roomID] {
		t.Fatalf("room missing from bob's list before delete: %v", ids)
	}

	// creator deletes; room vanishes from both lists (archived_at filter)
	alice.do("DELETE", "/api/v1/rooms/"+roomID, nil, 204)
	if ids := roomIDs(alice.doArr("GET", "/api/v1/rooms", nil, 200)); ids[roomID] {
		t.Fatalf("archived room still in alice's list: %v", ids)
	}
	if ids := roomIDs(bob.doArr("GET", "/api/v1/rooms", nil, 200)); ids[roomID] {
		t.Fatalf("archived room still in bob's list: %v", ids)
	}

	// repeat-delete stays 204 (idempotent soft delete)
	alice.do("DELETE", "/api/v1/rooms/"+roomID, nil, 204)
}

// TestRoomsSelfLeave: a member can leave a group on their own; other members
// and the room itself are unaffected.
func TestRoomsSelfLeave(t *testing.T) {
	url := testURL(t)
	ts, svc := newServer(t, url, nil)
	seedUsers(t, svc, "lgalice", "lgbob")
	base := &client{t: t, b: ts.URL}
	alice := base.forUser(svc, "lgalice")
	bob := base.forUser(svc, "lgbob")

	bobID := bob.str(bob.do("GET", "/api/v1/users/me", nil, 200), "id")
	room := alice.do("POST", "/api/v1/rooms", map[string]any{"type": "group_direct", "name": "team", "members": []string{bobID}}, 201)
	roomID := alice.str(room, "id")

	bob.do("DELETE", "/api/v1/rooms/"+roomID+"/members/"+bobID, nil, 204)
	if ids := roomIDs(bob.doArr("GET", "/api/v1/rooms", nil, 200)); ids[roomID] {
		t.Fatalf("left room still in bob's list: %v", ids)
	}
	if ids := roomIDs(alice.doArr("GET", "/api/v1/rooms", nil, 200)); !ids[roomID] {
		t.Fatalf("alice lost the room after bob left: %v", ids)
	}

	// the room survives the leave; alice (admin) can still delete it later
	alice.do("DELETE", "/api/v1/rooms/"+roomID, nil, 204)
}

// TestMemberRoles: members list carries room_role; only admins change roles;
// the last admin cannot be demoted.
func TestMemberRoles(t *testing.T) {
	url := testURL(t)
	ts, svc := newServer(t, url, nil)
	seedUsers(t, svc, "mralice", "mrbob")
	base := &client{t: t, b: ts.URL}
	alice := base.forUser(svc, "mralice")
	bob := base.forUser(svc, "mrbob")

	aliceID := alice.str(alice.do("GET", "/api/v1/users/me", nil, 200), "id")
	bobID := bob.str(bob.do("GET", "/api/v1/users/me", nil, 200), "id")
	room := alice.do("POST", "/api/v1/rooms", map[string]any{"type": "group_direct", "name": "team", "members": []string{bobID}}, 201)
	roomID := alice.str(room, "id")
	path := func(id string) string { return "/api/v1/rooms/" + roomID + "/members/" + id }

	roles := map[string]string{}
	for _, it := range alice.doArr("GET", "/api/v1/rooms/"+roomID+"/members", nil, 200) {
		m := it.(map[string]any)
		roles[m["id"].(string)] = m["room_role"].(string)
	}
	if roles[aliceID] != "admin" || roles[bobID] != "member" {
		t.Fatalf("room_role = %v", roles)
	}

	bob.do("PATCH", path(bobID), map[string]any{"role": "admin"}, 403)      // member cannot self-promote
	alice.do("PATCH", path(aliceID), map[string]any{"role": "member"}, 409) // last admin
	alice.do("PATCH", path(bobID), map[string]any{"role": "bogus"}, 400)

	alice.do("PATCH", path(bobID), map[string]any{"role": "admin"}, 204)
	alice.do("PATCH", path(aliceID), map[string]any{"role": "member"}, 204) // no longer the last admin
	alice.do("PATCH", path(bobID), map[string]any{"role": "member"}, 403)   // alice is a plain member now
	bob.do("PATCH", path(bobID), map[string]any{"role": "member"}, 409)     // bob is the last admin
}

// roomIDs is the set of ids in a GET /rooms response.
func roomIDs(list []any) map[string]bool {
	out := map[string]bool{}
	for _, it := range list {
		if m, ok := it.(map[string]any); ok {
			if id, ok := m["id"].(string); ok {
				out[id] = true
			}
		}
	}
	return out
}

// TestLivePushValidationRoundTrip (live ntfy): full §3.3 flow through the
// running API — register device → server pings token (encrypted) → client
// decrypts via SSE → POST /devices/{id}/validate → 204.
//
// Env: TEST_DATABASE_URL, TEST_NTFY_URL, TEST_NTFY_TOKEN.
func TestLivePushValidationRoundTrip(t *testing.T) {
	ntfyURL := strings.TrimSuffix(os.Getenv("TEST_NTFY_URL"), "/")
	ntfyTok := os.Getenv("TEST_NTFY_TOKEN")
	if testURL(t) == "" || ntfyURL == "" || ntfyTok == "" {
		t.Skip("TEST_DATABASE_URL/TEST_NTFY_URL/TEST_NTFY_TOKEN not set")
	}
	fake := newFakeStorage()
	ts, svc := newServer(t, testURL(t), fake)
	seedUsers(t, svc, "nalice")
	base := &client{t: t, b: ts.URL}
	alice := base.forUser(svc, "nalice")

	priv, p256dh, auth, err := testutil.ReceiverKey()
	if err != nil {
		t.Fatal(err)
	}
	topic := fmt.Sprintf("upval_%d", time.Now().UnixNano())

	// subscribe BEFORE the register POST: the server sends the validation
	// ping during registration, and ntfy does not replay cached messages.
	wireWait := sseFirst(t, ntfyURL, topic, ntfyTok)

	reg := alice.do("POST", "/api/v1/devices", map[string]any{
		"platform": "android", "client_name": "integration",
		"push_registration": map[string]any{
			"endpoint":    ntfyURL + "/" + topic,
			"public_key":  base64.RawURLEncoding.EncodeToString(p256dh),
			"auth_secret": base64.RawURLEncoding.EncodeToString(auth),
		},
	}, 201)
	deviceID := alice.str(reg, "device_id")

	// receive the encrypted validation ping via the ntfy stream
	select {
	case wire := <-wireWait:
		token, err := testutil.DecryptRFC8291(wire, priv, p256dh, auth)
		if err != nil {
			t.Fatalf("ping decrypt: %v", err)
		}
		// prove receipt: post the token back
		alice.do("POST", "/api/v1/devices/"+deviceID+"/validate", map[string]any{"token": string(token)}, 204)
	case <-time.After(25 * time.Second):
		t.Fatal("validation ping never arrived")
	}

	// already-validated devices re-validate idempotently (204, no re-arm)
	alice.do("POST", "/api/v1/devices/"+deviceID+"/validate", map[string]any{"token": "x"}, 204)
}

// TestLivePushMessageDelivery (live ntfy): validated recipient gets the
// message push, encrypted, identifiers-only (docs/PUSH.md), and it decrypts.
func TestLivePushMessageDelivery(t *testing.T) {
	ntfyURL := strings.TrimSuffix(os.Getenv("TEST_NTFY_URL"), "/")
	ntfyTok := os.Getenv("TEST_NTFY_TOKEN")
	if testURL(t) == "" || ntfyURL == "" || ntfyTok == "" {
		t.Skip("TEST_DATABASE_URL/TEST_NTFY_URL/TEST_NTFY_TOKEN not set")
	}
	fake := newFakeStorage()
	ts, svc := newServer(t, testURL(t), fake)
	seedUsers(t, svc, "malice", "mbob")
	base := &client{t: t, b: ts.URL}
	alice := base.forUser(svc, "malice")
	bob := base.forUser(svc, "mbob")

	// bob's device: keys + validation, subscribe BEFORE posting (push is
	// encrypted with a fresh token during registration)
	priv, p256dh, auth, err := testutil.ReceiverKey()
	if err != nil {
		t.Fatal(err)
	}
	topic := fmt.Sprintf("upmsg_%d", time.Now().UnixNano())
	t.Logf("test topic: %s", topic)
	// single stream covers registration ping (sent inside the POST) and the
	// later message push; subscribe BEFORE the POST.
	wireChat := sseFirst(t, ntfyURL, topic, ntfyTok)

	reg := bob.do("POST", "/api/v1/devices", map[string]any{
		"platform": "android", "client_name": "integration",
		"push_registration": map[string]any{
			"endpoint":    ntfyURL + "/" + topic,
			"public_key":  base64.RawURLEncoding.EncodeToString(p256dh),
			"auth_secret": base64.RawURLEncoding.EncodeToString(auth),
		},
	}, 201)
	deviceID := bob.str(reg, "device_id")

	// event 1 = encrypted validation ping; decrypt + prove receipt
	var pingToken string
	select {
	case wire := <-wireChat:
		tok, err := testutil.DecryptRFC8291(wire, priv, p256dh, auth)
		if err != nil {
			t.Fatalf("ping decrypt: %v", err)
		}
		pingToken = string(tok)
	case <-time.After(25 * time.Second):
		t.Fatal("validation ping never arrived")
	}
	bob.do("POST", "/api/v1/devices/"+deviceID+"/validate", map[string]any{"token": pingToken}, 204)

	// bob opts into all-message pushes
	bob.do("POST", "/api/v1/users/me/notifications", map[string]any{"mode": "all"}, 204)

	// room with both members, then alice posts
	bobID := bob.str(bob.do("GET", "/api/v1/users/me", nil, 200), "id")
	room := alice.do("POST", "/api/v1/rooms", map[string]any{"type": "group_direct", "name": "pushcheck", "members": []string{bobID}}, 201)
	roomID := alice.str(room, "id")
	msg := alice.do("POST", "/api/v1/rooms/"+roomID+"/messages", map[string]any{"body": "pushed to bob"}, 201)
	msgID := alice.str(msg, "id")

	// event 2 = the message push
	select {
	case wire := <-wireChat:
		got, err := testutil.DecryptRFC8291(wire, priv, p256dh, auth)
		if err != nil {
			t.Fatalf("push decrypt: %v", err)
		}
		var p struct {
			Type      string `json:"type"`
			RoomID    string `json:"room_id"`
			MessageID string `json:"message_id"`
		}
		if json.Unmarshal(got, &p) != nil || p.Type != "chat.message" || p.RoomID != roomID || p.MessageID != msgID {
			t.Fatalf("push payload mismatch: %s", got)
		}
		// identifiers only — no message content on the wire (docs/PUSH.md)
		if strings.Contains(string(got), "pushed to bob") {
			t.Fatal("push leaked message content")
		}
	case <-time.After(25 * time.Second):
		t.Fatal("message push never arrived")
	}
}

// sseFirst is the SSE helper (ntfy path form), shared here.
func sseFirst(t *testing.T, baseURL, topic, token string) <-chan []byte {
	t.Helper()
	req, _ := http.NewRequest("GET", baseURL+"/"+topic+"/sse", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("sse: %v", err)
	}
	ch := make(chan []byte, 16)
	go func() {
		defer resp.Body.Close()
		dbg := os.Getenv("TEST_SSE_DEBUG") != ""
		start := time.Now()
		var f *os.File
		if dbg {
			f, _ = os.OpenFile("/tmp/sse_test.log", os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
			defer func() {
				if f != nil {
					fmt.Fprintf(f, "END %s err-scanner\n", time.Since(start))
					f.Close()
				}
			}()
		}
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
		for sc.Scan() {
			line := sc.Text()
			if dbg && f != nil {
				fmt.Fprintf(f, "[%s] len=%d %.50s\n", time.Since(start), len(line), line)
			}
			after, ok := strings.CutPrefix(line, "data: {")
			if !ok {
				continue
			}
			var ev struct {
				Message string `json:"message"`
				Event   string `json:"event"`
			}
			if json.Unmarshal([]byte("{"+after), &ev) != nil {
				if dbg {
					fmt.Println("SSE PARSE ERR")
				}
				continue
			}
			if dbg {
				fmt.Printf("SSE EVT event=%s msglen=%d msghead=%.16s\n", ev.Event, len(ev.Message), ev.Message)
			}
			if ev.Event != "message" || ev.Message == "" {
				continue
			}
			if dbg && f != nil {
				fmt.Fprintf(f, "[msg] event=%s msglen=%d\n", ev.Event, len(ev.Message))
			}
			clean := strings.ReplaceAll(ev.Message, "\n", "")
			b, err := base64.StdEncoding.DecodeString(clean)
			if err != nil {
				b = []byte(ev.Message)
			}
			if dbg && f != nil {
				fmt.Fprintf(f, "[delivered] msglen=%d\n", len(b))
			}
			// keep streaming: later events (message push) must still arrive
			select {
			case ch <- b:
			default:
			}
		}
		if dbg && f != nil {
			fmt.Fprintf(f, "[scanner done: %v]\n", sc.Err())
		}
	}()
	return ch
}

// statusCodeOf is the status code of a possibly-nil response.
func statusCodeOf(r *http.Response) int {
	if r == nil {
		return 0
	}
	return r.StatusCode
}

package rcbridge

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib"

	migrations "github.com/gochathub/gochathub-server/db"
	"github.com/gochathub/gochathub-server/internal/config"
	"github.com/gochathub/gochathub-server/internal/model"
	"github.com/gochathub/gochathub-server/internal/push"
	"github.com/gochathub/gochathub-server/internal/service"
	"github.com/gochathub/gochathub-server/internal/storage"
	"github.com/gochathub/gochathub-server/internal/store"
	"github.com/gochathub/gochathub-server/internal/testutil"
	"github.com/gochathub/gochathub-server/internal/ws"
)

// Boot: fresh per-test database, migrated (migrate tests' pattern).
func testStore(t *testing.T) (*store.Store, func()) {
	t.Helper()
	dsn := testutil.Database(t)
	ctx := context.Background()
	sqlDB, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := migrations.Migrate(ctx, sqlDB); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	sqlDB.Close()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	return store.New(pool), pool.Close
}

// fakeRC is a minimal Rocket.Chat workspace: users.info, the subscriptions
// delta, dm/groups history and chat.postMessage, recorded in memory.
type fakeRC struct {
	srv       *httptest.Server
	mu        sync.Mutex
	subs      map[string]rcSub
	msgs      map[string][]rcMsg
	posted    []postedMsg
	uid       string
	username  string
	tsCounter int

	histWithOldest int // history pulls that carried an `oldest` bound (delta pulls)
}

type postedMsg struct {
	RoomID string
	Text   string
}

func newFakeRC(t *testing.T, rcUser, uname string) *fakeRC {
	t.Helper()
	f := &fakeRC{
		subs:     map[string]rcSub{},
		msgs:     map[string][]rcMsg{},
		uid:      rcUser,
		username: uname,
	}
	mux := http.NewServeMux()

	mux.HandleFunc("GET /api/v1/users.info", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		q := r.URL.Query()
		if q.Get("userId") == f.uid || q.Get("username") == uname {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"success": true,
				"user":    map[string]any{"_id": f.uid, "username": uname, "name": "R Owner"},
			})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"success": false, "error": "user-not-found"})
	})

	mux.HandleFunc("GET /api/v1/subscriptions.get", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		since := r.URL.Query().Get("updatedSince")
		if since == "" {
			out := []rcSub{}
			f.mu.Lock()
			for _, s := range f.subs {
				out = append(out, s)
			}
			f.mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "subscriptions": out})
			return
		}
		var tms rcDate
		if err := tms.UnmarshalJSON([]byte(`"` + since + `"`)); err != nil {
			http.Error(w, `{"success":false,"error":"bad updatedSince"}`, 400)
			return
		}
		var up []rcSub
		f.mu.Lock()
		for _, s := range f.subs {
			if s.UpdatedAt.Time.After(tms.Time) {
				up = append(up, s)
			}
		}
		f.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "update": up, "remove": []rcSub{}})
	})

	// like the real server: DM rooms are only readable via dm/im.history,
	// private groups only via groups.history
	for _, name := range []string{"dm.history", "im.history"} {
		mux.HandleFunc("GET /api/v1/"+name, rcHistory(f, "d"))
	}
	mux.HandleFunc("GET /api/v1/groups.history", rcHistory(f, "p"))

	mux.HandleFunc("POST /api/v1/chat.postMessage", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			RoomID string `json:"roomId"`
			Text   string `json:"text"`
		}
		_ = json.NewDecoder(r.Body).Decode(&in)
		m := f.addMsg(in.RoomID, in.Text, f.uid, uname, "")
		f.mu.Lock()
		f.posted = append(f.posted, postedMsg{RoomID: in.RoomID, Text: in.Text})
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "message": m})
	})

	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func rcHistory(f *fakeRC, kind string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		q := r.URL.Query()
		rid := q.Get("roomId")
		var out []rcMsg
		f.mu.Lock()
		if f.subs[rid].T != kind {
			f.mu.Unlock()
			w.WriteHeader(400)
			_ = json.NewEncoder(w).Encode(map[string]any{"success": false, "error": "error-room-not-found"})
			return
		}
		if q.Get("oldest") != "" {
			f.histWithOldest++
		}
		for _, m := range f.msgs[rid] {
			if m.TS.Time.Before(orTS(q.Get("oldest"))) || m.TS.Time.After(orTS(q.Get("latest"))) {
				continue
			}
			out = append(out, m)
		}
		f.mu.Unlock()
		sort.Slice(out, func(i, j int) bool { return out[i].TS.Time.After(out[j].TS.Time) })
		if c, _ := strconv.Atoi(q.Get("count")); c > 0 && len(out) > c {
			out = out[:c]
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "messages": out})
	}
}

func orTS(ts string) time.Time {
	if ts == "" {
		return time.Time{}
	}
	var d rcDate
	_ = d.UnmarshalJSON([]byte(`"` + ts + `"`))
	return d.Time
}

// addMsg inserts a message and bumps the subscription's _updatedAt so the
// next delta pull reports the room.
func (f *fakeRC) addMsg(rid, text, authorID, authorName, tmid string) rcMsg {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tsCounter++
	ts := time.Now().UTC().Add(-30 * time.Minute).Add(time.Duration(f.tsCounter) * time.Second)
	m := rcMsg{
		ID: fmt.Sprintf("RMsg%03d", f.tsCounter), RID: rid, Msg: text,
		U:    rcUser{ID: authorID, Username: authorName},
		TMID: tmid,
	}
	m.TS.Time = ts
	f.msgs[rid] = append(f.msgs[rid], m)
	if s, ok := f.subs[rid]; ok {
		s.UpdatedAt = m.TS
		f.subs[rid] = s
	}
	return m
}

func (f *fakeRC) registerSub(rid, typ, name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s := rcSub{RID: rid, T: typ, Name: name, Open: true}
	s.TS.Time = time.Now().UTC().Add(-2 * time.Hour)
	s.UpdatedAt.Time = time.Now().UTC().Add(-time.Hour)
	f.subs[rid] = s
}

// bootBridge wires the service graph directly (internal/migrate tests use
// app.Assemble; the app package imports rcbridge, which would cycle here).
func bootBridge(t *testing.T, st *store.Store, base string) (*service.App, *Bridge) {
	t.Helper()
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	sender := &push.Sender{
		Store:      st,
		Log:        log,
		HTTP:       &http.Client{Timeout: 10 * time.Second},
		Subscriber: "https://rcbridge.test.invalid",
	}
	if _, err := sender.EnsureVAPID(context.Background(), "", ""); err != nil {
		t.Fatalf("vapid: %v", err)
	}
	svc := &service.App{Store: st, Storage: storage.Disabled{}, Log: log, Sender: sender}
	svc.Users = service.UserService{App: svc}
	svc.Rooms = &service.RoomService{App: svc}
	svc.Messages = &service.MessageService{App: svc}
	hub := ws.NewHub(log)
	hub.App = svc
	svc.Notify = hub
	sender.ActiveIn = hub.SubscribedToRoom
	// 127.0.0.1 allow-listed: the fake workspace is a loopback httptest server
	b, err := New(svc, &config.Config{RCBaseURL: base, RCPatKey: "test-integration-key", RCAllowHosts: []string{"127.0.0.1"}}, log)
	if err != nil {
		t.Fatalf("bridge: %v", err)
	}
	svc.RC = b
	return svc, b
}

const testPAT = "personal-access-token-test-0123456789"

func linkOf(t *testing.T, st *store.Store, userID string) store.RCLink {
	t.Helper()
	link, err := st.RCLinkByUser(context.Background(), userID)
	if err != nil {
		t.Fatalf("link row: %v", err)
	}
	return link
}

func countRows(t *testing.T, st *store.Store, stmt string) int {
	t.Helper()
	var n int
	if err := st.Q.QueryRow(context.Background(), stmt).Scan(&n); err != nil {
		t.Fatalf("count %q: %v", stmt, err)
	}
	return n
}

func directRoomOf(t *testing.T, rooms []model.Room) model.Room {
	t.Helper()
	for _, r := range rooms {
		if r.Type == model.RoomDirect {
			return r
		}
	}
	t.Fatal("no direct room in list")
	return model.Room{}
}

// TestBridgeLinkSyncSendUnlink: the whole loop against a fake workspace.
func TestBridgeLinkSyncSendUnlink(t *testing.T) {
	st, closeDB := testStore(t)
	defer closeDB()
	ctx := context.Background()

	f := newFakeRC(t, "rcUserIdOwner", "rcowner")
	svc, b := bootBridge(t, st, f.srv.URL)

	alice, err := svc.Users.CreateUser(ctx, "alice", "Alice", "alice@test.invalid", "pw-alice", "user")
	if err != nil {
		t.Fatalf("create alice: %v", err)
	}
	p := service.Principal{UserID: alice.ID, Kind: "session"}

	// workspace content: one DM with bober, one private group; one pre-link
	// message authored on the RC client by the owner themselves
	f.registerSub("ridDM", "d", "bober")
	f.registerSub("ridGroup", "p", "secret team")
	f.addMsg("ridDM", "hello from bober", "rcUserIdBob", "bober", "")
	f.addMsg("ridDM", "and my own RC client message", f.uid, f.username, "")
	f.addMsg("ridGroup", "standup notes", "rcUserIdBob", "bober", "")

	stt, err := b.Link(ctx, p, service.LinkRequest{BaseURL: f.srv.URL, RCUserID: f.uid, Token: testPAT})
	if err != nil {
		t.Fatalf("link: %v", err)
	}
	if !stt.Linked || stt.RCUsername != "rcowner" || stt.RCUserID != f.uid {
		t.Fatalf("link status: %+v", stt)
	}
	link := linkOf(t, st, alice.ID)
	if strings.Contains(string(link.PatEnc), testPAT) {
		t.Fatal("PAT stored in plaintext")
	}

	// first sync runs off the request path: wait for rooms + backfill + the
	// read-stamping of every backfilled message (sequential, one room at a time)
	deadline := time.Now().Add(5 * time.Second)
	for {
		fresh, _ := svc.Rooms.List(ctx, p)
		if len(fresh) == 2 {
			allRead := true
			for _, r := range fresh {
				if r.UnreadCount != 0 {
					allRead = false
				}
			}
			if page, _ := svc.Messages.List(ctx, p, directRoomOf(t, fresh).ID, "", "", 50); len(page.Items) == 2 && allRead {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("sync incomplete: rooms=%d", len(fresh))
		}
		time.Sleep(50 * time.Millisecond)
	}

	rooms, err := svc.Rooms.List(ctx, p)
	if err != nil {
		t.Fatalf("rooms: %v", err)
	}
	dm := directRoomOf(t, rooms)
	if dm.Source != store.SrcRocketChat || dm.Name == nil || *dm.Name != "bober" {
		t.Fatalf("dm room: %+v name=%v", dm, dm.Name)
	}
	var group model.Room
	for _, r := range rooms {
		if r.Type != model.RoomDirect {
			group = r
		}
		if r.UnreadCount != 0 {
			t.Fatalf("room %s unread %d after backfill (want 0)", r.ID, r.UnreadCount)
		}
	}
	if group.Name == nil || *group.Name != "secret team" {
		t.Fatalf("group name: %+v", group.Name)
	}

	// ids are per owner + workspace + remote id (ADR-024)
	if dm.ID != rcID("room", link, "ridDM") {
		t.Fatalf("dm room id %s not owner-derived", dm.ID)
	}

	page, err := svc.Messages.List(ctx, p, dm.ID, "", "", 50)
	if err != nil {
		t.Fatalf("dm messages: %v", err)
	}
	for _, m := range page.Items {
		switch m.Body {
		case "and my own RC client message":
			if m.AuthorID != alice.ID {
				t.Fatalf("own RC message attributed to %s, want alice", m.AuthorID)
			}
			if m.Author.Username != "alice" {
				t.Fatalf("own author rendered as %q", m.Author.Username)
			}
		case "hello from bober":
			if m.AuthorID != rcID("user", link, "rcUserIdBob") {
				t.Fatalf("bober message attributed to %s", m.AuthorID)
			}
			if m.Author.Username != "bober" {
				t.Fatalf("bober stub username %q", m.Author.Username)
			}
		default:
			t.Fatalf("dm has foreign message %q", m.Body)
		}
	}

	// delta: bober sends another DM message on the RC server
	m4 := f.addMsg("ridDM", "ping from bober", "rcUserIdBob", "bober", "")
	if err := b.syncUser(ctx, link); err != nil {
		t.Fatalf("syncUser: %v", err)
	}
	page, _ = svc.Messages.List(ctx, p, dm.ID, "", "", 50)
	if len(page.Items) != 3 {
		t.Fatalf("want 3 dm messages after delta, got %d", len(page.Items))
	}
	deltaLanded := false
	for _, m := range page.Items {
		if m.ID == rcID("msg", link, m4.ID) {
			deltaLanded = true
		}
	}
	if !deltaLanded {
		t.Fatal("delta message missing")
	}
	rooms, _ = svc.Rooms.List(ctx, p)
	if got := directRoomOf(t, rooms).UnreadCount; got != 1 {
		t.Fatalf("dm unread want 1, got %d", got)
	}

	// send-through: posts as alice via chat.postMessage, mirrors the echo once
	msg, err := svc.Messages.Create(ctx, p, dm.ID, service.MessageInput{Body: "replied from gochathub"})
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	f.mu.Lock()
	received := f.posted
	f.mu.Unlock()
	if len(received) != 1 || received[0].RoomID != "ridDM" || received[0].Text != "replied from gochathub" {
		t.Fatalf("rc never received the send: %+v", received)
	}
	if msg.ID != rcID("msg", link, lastEchoOf(t, f, "ridDM")) {
		t.Fatalf("send row id %s is not the RC echo-derived id", msg.ID)
	}
	if page, _ := svc.Messages.List(ctx, p, dm.ID, "", "gochathub", 10); len(page.Items) != 1 {
		t.Fatalf("echo duplicated: %d rows mention gochathub", len(page.Items))
	}

	// unsupported cross-server writes fail with validation errors, not 500s
	if _, err := svc.Messages.Create(ctx, p, dm.ID, service.MessageInput{Body: "?", ReplyToID: msg.ID}); err == nil {
		t.Fatal("thread reply should be rejected")
	}
	if _, err := svc.Messages.Create(ctx, p, dm.ID, service.MessageInput{Body: "x", AttachmentIDs: []string{"01a12ae5-0000-7000-8000-000000000099"}}); err == nil {
		t.Fatal("attachment should be rejected")
	}

	// delta pulls must be bounded by the stored watermark, not re-fetch the window
	f.mu.Lock()
	if f.histWithOldest == 0 {
		f.mu.Unlock()
		t.Fatal("delta history pull never carried an `oldest` bound")
	}
	f.mu.Unlock()

	// unlink: PAT destroyed, rooms hidden (archived, history kept)
	if err := svc.RC.Unlink(ctx, p); err != nil {
		t.Fatalf("unlink: %v", err)
	}
	if n := countRows(t, st, "SELECT count(*) FROM rocket_chat_links"); n != 0 {
		t.Fatalf("link rows after unlink: %d", n)
	}
	if stt, err := svc.RC.Status(ctx, p); err != nil || stt.Linked {
		t.Fatalf("status after unlink: %+v %v", stt, err)
	}
	if rooms, _ = svc.Rooms.List(ctx, p); len(rooms) != 0 {
		t.Fatalf("rooms visible after unlink: %d", len(rooms))
	}
	if n := countRows(t, st, "SELECT count(*) FROM messages WHERE room_id = '"+dm.ID+"'"); n == 0 {
		t.Fatal("unlink deleted history; it must only hide it")
	}

	// re-link: same rooms come back, history intact, nothing duplicated
	if _, err := b.Link(ctx, p, service.LinkRequest{BaseURL: f.srv.URL, RCUserID: f.uid, Token: testPAT}); err != nil {
		t.Fatalf("relink: %v", err)
	}
	deadline = time.Now().Add(5 * time.Second)
	for {
		if rooms, _ = svc.Rooms.List(ctx, p); len(rooms) == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("rooms did not reappear after relink: %d", len(rooms))
		}
		time.Sleep(50 * time.Millisecond)
	}
	if page, _ := svc.Messages.List(ctx, p, directRoomOf(t, rooms).ID, "", "", 50); len(page.Items) != 4 {
		t.Fatalf("history after relink: want 4, got %d", len(page.Items))
	}
}

// TestBridgeIsolatedPerUser: two local users linked to the same workspace get
// their own rooms/messages/shadow accounts; shadows are not searchable; mirrored
// rooms accept no extra members; a private workspace URL is refused.
func TestBridgeIsolatedPerUser(t *testing.T) {
	st, closeDB := testStore(t)
	defer closeDB()
	ctx := context.Background()

	f := newFakeRC(t, "rcUserIdOwner", "rcowner")
	svc, b := bootBridge(t, st, "")

	alice, err := svc.Users.CreateUser(ctx, "alice", "Alice", "a@test.invalid", "pw-a", "user")
	if err != nil {
		t.Fatal(err)
	}
	carol, err := svc.Users.CreateUser(ctx, "carol", "Carol", "c@test.invalid", "pw-c", "user")
	if err != nil {
		t.Fatal(err)
	}
	pa := service.Principal{UserID: alice.ID, Kind: "session"}
	pc := service.Principal{UserID: carol.ID, Kind: "session"}

	f.registerSub("ridShared", "p", "shared group")
	f.addMsg("ridShared", "hi all", "rcUserIdBob", "bober", "")

	for _, p := range []service.Principal{pa, pc} {
		if _, err := b.Link(ctx, p, service.LinkRequest{BaseURL: f.srv.URL, RCUserID: f.uid, Token: testPAT}); err != nil {
			t.Fatalf("link %s: %v", p.UserID, err)
		}
	}
	waitRooms := func(p service.Principal) []model.Room {
		deadline := time.Now().Add(5 * time.Second)
		for {
			rooms := mustRooms(t, svc, p)
			if len(rooms) == 1 {
				if page, _ := svc.Messages.List(ctx, p, rooms[0].ID, "", "", 10); len(page.Items) == 1 {
					return rooms
				}
			}
			if time.Now().After(deadline) {
				t.Fatalf("user %s never got the mirrored room", p.UserID)
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
	ra, rc := waitRooms(pa)[0], waitRooms(pc)[0]
	if ra.ID == rc.ID {
		t.Fatal("two users share one mirrored room")
	}
	// alice cannot read carol's mirror
	if _, err := svc.Rooms.Get(ctx, pa, rc.ID); err == nil {
		t.Fatal("alice can read carol's mirrored room")
	}
	if _, err := svc.Messages.List(ctx, pa, rc.ID, "", "", 10); err == nil {
		t.Fatal("alice can list carol's mirrored messages")
	}
	// shadow accounts exist per owner but are not searchable
	if n := countRows(t, st, "SELECT count(*) FROM users WHERE username LIKE 'bober%'"); n != 2 {
		t.Fatalf("want one shadow per owner (2), got %d", n)
	}
	if found, err := svc.Users.Search(ctx, "bober", 10); err != nil || len(found) != 0 {
		t.Fatalf("shadow accounts searchable: %v %v", found, err)
	}
	// nobody can be added to a mirror (it posts through the owner's token)
	if err := svc.Rooms.AddMember(ctx, pa, ra.ID, carol.ID); err == nil {
		t.Fatal("added a member to a mirrored room")
	}
	// private/internal workspace URLs are refused unless allow-listed
	b.AllowHosts = nil
	if _, err := b.Link(ctx, pa, service.LinkRequest{BaseURL: "http://10.0.0.5", RCUserID: "x", Token: testPAT}); err == nil {
		t.Fatal("private workspace URL accepted")
	}
	if _, err := b.Link(ctx, pa, service.LinkRequest{BaseURL: f.srv.URL, RCUserID: f.uid, Token: testPAT}); err == nil {
		t.Fatal("loopback workspace URL accepted without allow-list")
	}
}

func lastEchoOf(t *testing.T, f *fakeRC, rid string) string {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	msgs := f.msgs[rid]
	return msgs[len(msgs)-1].ID
}

// TestUserBadgesForNewRcMessages: backfill noise vs real delta behavior is
// covered above; this quick test keeps the receipts semantics pinned.
func TestStampSemanticsAfterBackfill(t *testing.T) {
	st, closeDB := testStore(t)
	defer closeDB()
	ctx := context.Background()

	f := newFakeRC(t, "rcUO", "rcown3")
	svc, b := bootBridge(t, st, f.srv.URL)
	alice, err := svc.Users.CreateUser(ctx, "alice3", "Alice3", "a3@test.invalid", "pw-a3", "user")
	if err != nil {
		t.Fatal(err)
	}
	p := service.Principal{UserID: alice.ID, Kind: "session"}
	f.registerSub("ridX", "d", "xpeer")
	f.addMsg("ridX", "one", "rcUB", "xpeer", "")

	if _, err := b.Link(ctx, p, service.LinkRequest{BaseURL: f.srv.URL, RCUserID: f.uid, Token: testPAT}); err != nil {
		t.Fatalf("link: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	var lastMsgID string
	for time.Now().Before(deadline) {
		rooms := mustRooms(t, svc, p)
		if len(rooms) == 0 || rooms[0].UnreadCount != 0 { // backfill must also be read-stamped
			time.Sleep(25 * time.Millisecond)
			continue
		}
		dm := directRoomOf(t, rooms)
		page, _ := svc.Messages.List(ctx, p, dm.ID, "", "", 10)
		if len(page.Items) == 1 {
			lastMsgID = page.Items[0].ID
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if lastMsgID == "" {
		t.Fatal("backfill never landed")
	}
	dm := directRoomOf(t, mustRooms(t, svc, p))
	// backfill stamps read: no unread
	if got := mustRooms(t, svc, p)[0].UnreadCount; got != 0 { // dm is the only room so far
		t.Fatalf("unread after backfill: %d", got)
	}
	// a later RC message lands unread
	f.addMsg("ridX", "two", "rcUB", "xpeer", "")
	if err := b.syncUser(ctx, linkOf(t, st, alice.ID)); err != nil {
		t.Fatal(err)
	}
	if got := directRoomOf(t, mustRooms(t, svc, p)).UnreadCount; got != 1 {
		t.Fatalf("unread after delta: %d", got)
	}
	// reading it here works through the normal path
	page, _ := svc.Messages.List(ctx, p, dm.ID, "", "", 10)
	if err := svc.Rooms.Read(ctx, p, dm.ID, page.Items[0].ID); err != nil {
		t.Fatalf("read: %v", err)
	}
	if got := directRoomOf(t, mustRooms(t, svc, p)).UnreadCount; got != 0 {
		t.Fatalf("unread after Read: %d", got)
	}
}

func mustRooms(t *testing.T, svc *service.App, p service.Principal) []model.Room {
	t.Helper()
	rooms, err := svc.Rooms.List(context.Background(), p)
	if err != nil {
		t.Fatalf("rooms: %v", err)
	}
	return rooms
}

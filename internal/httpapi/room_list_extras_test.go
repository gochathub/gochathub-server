package httpapi_test

import (
	"strings"
	"testing"

	"github.com/gochathub/gochathub-server/internal/service"
)

// onlineNotifier overrides presence on top of the server's real notifier.
type onlineNotifier struct {
	service.Notifier
	online map[string]bool
}

func (n onlineNotifier) Connected(uid string) bool { return n.online[uid] }

// TestRoomListExtras: last_message, first_unread_message_id and peer_online
// on GET /rooms (classic sidebar row + unread divider).
func TestRoomListExtras(t *testing.T) {
	url := testURL(t)
	ts, svc := newServer(t, url, nil)
	seedUsers(t, svc, "xalice", "xbob")
	base := &client{t: t, b: ts.URL}
	alice := base.forUser(svc, "xalice")
	bob := base.forUser(svc, "xbob")
	bobID := bob.str(bob.do("GET", "/api/v1/users/me", nil, 200), "id")

	find := func(c *client, id string) map[string]any {
		for _, r := range c.doArr("GET", "/api/v1/rooms", nil, 200) {
			if m := r.(map[string]any); m["id"] == id {
				return m
			}
		}
		t.Fatalf("room %s not listed", id)
		return nil
	}

	room := alice.do("POST", "/api/v1/rooms", map[string]any{"type": "group_direct", "name": "team", "members": []string{bobID}}, 201)
	roomID := alice.str(room, "id")
	if r := find(alice, roomID); r["last_message"] != nil || r["first_unread_message_id"] != nil {
		t.Fatalf("empty room has extras: %v", r)
	}

	m1 := alice.do("POST", "/api/v1/rooms/"+roomID+"/messages", map[string]any{"body": "first"}, 201)
	alice.do("POST", "/api/v1/rooms/"+roomID+"/messages", map[string]any{"body": strings.Repeat("x", 300)}, 201)

	r := find(bob, roomID)
	lm, _ := r["last_message"].(map[string]any)
	if lm == nil || len(lm["body"].(string)) != 200 || lm["author_id"] == bobID || lm["has_attachment"] != false {
		t.Fatalf("bob last_message: %v", r["last_message"])
	}
	if r["first_unread_message_id"] != alice.str(m1, "id") {
		t.Fatalf("first_unread_message_id = %v, want %v", r["first_unread_message_id"], m1["id"])
	}
	// the author has nothing unread
	if a := find(alice, roomID); a["first_unread_message_id"] != nil {
		t.Fatalf("author sees unread: %v", a)
	}

	// presence: connected AND opted in to last-seen
	dm := alice.do("POST", "/api/v1/rooms", map[string]any{"type": "direct", "members": []string{bobID}}, 201)
	dmID := alice.str(dm, "id")
	svc.Notify = onlineNotifier{Notifier: svc.Notify, online: map[string]bool{bobID: true}}
	if find(alice, dmID)["peer_online"] != nil {
		t.Fatal("peer_online leaked while last_seen_visible is off")
	}
	bob.do("PATCH", "/api/v1/users/me/preferences", map[string]any{"last_seen_visible": true}, 200)
	if find(alice, dmID)["peer_online"] != true {
		t.Fatal("peer_online missing for a connected, opted-in peer")
	}
	if find(alice, roomID)["peer_online"] != nil {
		t.Fatal("peer_online on a non-direct room")
	}
}

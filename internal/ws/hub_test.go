package ws

import (
	"context"
	"testing"

	"github.com/gochathub/gochathub-server/internal/model"
)

// TestRemoveFromRoom: server-side membership revocation drops the room from
// the hub index and every live connection's own subscription list.
func TestRemoveFromRoom(t *testing.T) {
	h := NewHub(nil)
	mk := func(uid string) *clientConn {
		return &clientConn{
			hub:    h,
			UserID: uid,
			ctx:    context.Background(),
			outbox: make(chan model.WSEnvelope, 4),
			rooms:  map[string]struct{}{},
		}
	}
	a, b := mk("user-a"), mk("user-b")
	h.Connect(a)
	h.Connect(b)
	h.Subscribe(a, "room-1")
	h.Subscribe(b, "room-1")
	h.Subscribe(a, "room-2")
	a.rooms["room-1"] = struct{}{}
	b.rooms["room-1"] = struct{}{}

	if !h.SubscribedToRoom("user-a", "room-1") {
		t.Fatal("precondition: a subscribed")
	}

	h.RemoveFromRoom("user-a", "room-1")

	if h.SubscribedToRoom("user-a", "room-1") {
		t.Fatal("hub index still has user-a in room-1")
	}
	if _, ok := a.rooms["room-1"]; ok {
		t.Fatal("conn-local subscription not purged")
	}
	if h.SubscribedToRoom("user-a", "room-2") {
		// other rooms untouched
	} else {
		t.Fatal("other subscriptions must survive")
	}
	if !h.SubscribedToRoom("user-b", "room-1") {
		t.Fatal("innocent subscriber must survive")
	}

	// fan-out stops for the removed member
	h.ToRoom("room-1", model.WSEnvelope{Type: "message.created"})
	select {
	case env := <-a.outbox:
		t.Fatalf("removed member received %q", env.Type)
	default:
	}
	if len(b.outbox) == 0 {
		t.Fatal("remaining member must receive the event")
	}

	// last index entry prunes the room key
	h.RemoveFromRoom("user-b", "room-1")
	if _, ok := h.rooms["room-1"]; ok {
		t.Fatal("empty room key not pruned")
	}
}

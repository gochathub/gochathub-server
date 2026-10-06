// Package ws implements the WebSocket realtime path: per-user connections,
// room subscriptions, message fan-out, receipts on acks, typing relay, and
// presence. PostgreSQL stays authoritative; a dropped connection recovers
// through REST resync (docs/WEBSOCKETS.md).
package ws

import (
	"log/slog"
	"sync"

	"github.com/gochathub/gochathub-server/internal/model"
	"github.com/gochathub/gochathub-server/internal/service"
)

// Hub owns all connected clients in-process. Multi-instance deployments will
// add PostgreSQL LISTEN/NOTIFY later ("only if needed").
type Hub struct {
	Log *slog.Logger
	App *service.App

	mu      sync.Mutex
	clients map[string]map[*clientConn]struct{} // userID → conns
	rooms   map[string]map[string]struct{}      // roomID → subscribed userIDs
}

// PrincipalFor builds the socket principal (role re-resolved by services
// that need admin checks).
func PrincipalFor(userID string) service.Principal {
	return service.Principal{UserID: userID, Kind: "session"}
}

func NewHub(log *slog.Logger) *Hub {
	return &Hub{
		Log:     log,
		clients: map[string]map[*clientConn]struct{}{},
		rooms:   map[string]map[string]struct{}{},
	}
}

// Connect registers a connection for a user.
func (h *Hub) Connect(c *clientConn) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.clients[c.UserID] == nil {
		h.clients[c.UserID] = map[*clientConn]struct{}{}
	}
	wasConnected := len(h.clients[c.UserID]) > 0
	h.clients[c.UserID][c] = struct{}{}
	if !wasConnected {
		// locked variant: h.mu is already held (non-reentrant mutex)
		h.broadcastPresenceLocked(c.UserID, true)
	}
}

// Disconnect removes the connection and prunes empty presence.
func (h *Hub) Disconnect(c *clientConn) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if set := h.clients[c.UserID]; set != nil {
		delete(set, c)
		if len(set) == 0 {
			delete(h.clients, c.UserID)
			h.broadcastPresenceLocked(c.UserID, false)
		}
	}
	for room, uids := range h.rooms {
		delete(uids, c.UserID)
		if len(uids) == 0 {
			delete(h.rooms, room)
		}
	}
}

// Subscribe marks the user live in a room.
func (h *Hub) Subscribe(c *clientConn, roomID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.rooms[roomID] == nil {
		h.rooms[roomID] = map[string]struct{}{}
	}
	h.rooms[roomID][c.UserID] = struct{}{}
}

func (h *Hub) Unsubscribe(c *clientConn, roomID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if uids := h.rooms[roomID]; uids != nil {
		delete(uids, c.UserID)
		if len(uids) == 0 {
			delete(h.rooms, roomID)
		}
	}
}

// SubscribedToRoom implements service.Notifier push suppression.
func (h *Hub) SubscribedToRoom(userID, roomID string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	_, ok := h.rooms[roomID][userID]
	return ok
}

// Connected implements service.Notifier presence queries.
func (h *Hub) Connected(userID string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.clients[userID]) > 0
}

// ToUser implements service.Notifier: deliver to every connection of one user.
func (h *Hub) ToUser(userID string, env model.WSEnvelope) {
	h.mu.Lock()
	conns := make([]*clientConn, 0, len(h.clients[userID]))
	for c := range h.clients[userID] {
		conns = append(conns, c)
	}
	h.mu.Unlock()
	for _, c := range conns {
		c.send(env)
	}
}

// ToRoom implements service.Notifier: deliver to every subscriber of the room.
func (h *Hub) ToRoom(roomID string, env model.WSEnvelope) {
	h.mu.Lock()
	uids := make([]string, 0, len(h.rooms[roomID]))
	for uid := range h.rooms[roomID] {
		uids = append(uids, uid)
	}
	h.mu.Unlock()
	for _, uid := range uids {
		h.ToUser(uid, env)
	}
}

// broadcastPresenceLocked: connected users appear online to their room peers.
// Callers hold h.mu (or know it's free); it never locks.
func (h *Hub) broadcastPresenceLocked(userID string, online bool) {
	env := model.WSEnvelope{
		Type: "presence.changed",
		Data: map[string]any{"user_id": userID, "state": presenceState(online)},
	}
	// deliver to every other user who shares any room with this user
	affected := map[string]struct{}{}
	for _, uids := range h.rooms {
		if _, isHere := uids[userID]; !isHere {
			continue
		}
		for uid := range uids {
			if uid != userID {
				affected[uid] = struct{}{}
			}
		}
	}
	for uid := range affected {
		for c := range h.clients[uid] {
			c.send(env)
		}
	}
}

func presenceState(online bool) string {
	if online {
		return "online"
	}
	return "offline"
}

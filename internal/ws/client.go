package ws

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/gochathub/gochathub-server/internal/model"
)

// clientConn wraps one socket bound to an authenticated user.
type clientConn struct {
	hub    *Hub
	UserID string
	ctx    context.Context
	conn   *websocket.Conn
	outbox chan model.WSEnvelope

	mu    sync.Mutex
	rooms map[string]struct{}
}

func (c *clientConn) send(env model.WSEnvelope) {
	select {
	case c.outbox <- env:
	default:
		// slow client: drop the event, REST resync covers it
		// (docs/WEBSOCKETS.md reliability model)
	}
}

// Serve upgrades and streams until close. The HTTP layer must have resolved
// and passed an authenticated userID (cookie or bearer — ADR-015).
func (h *Hub) Serve(ctx context.Context, w http.ResponseWriter, r *http.Request, userID string) error {
	// ADR-016 parity with the HTTP same-origin middleware: when BaseOrigin is
	// set, the upgrade Origin must match it (deployment is proxied, so Host
	// cannot be compared). Empty keeps coder's strict same-origin default.
	opts := &websocket.AcceptOptions{}
	if base := h.App.BaseOrigin; base != "" {
		if u, perr := url.Parse(base); perr == nil && u.Host != "" {
			opts.OriginPatterns = []string{u.Host}
		}
	}
	conn, err := websocket.Accept(w, r, opts)
	if err != nil {
		h.Log.WarnContext(ctx, "ws accept failed", "err", err)
		return err
	}
	h.Log.InfoContext(ctx, "ws accepted", "user", userID)
	defer conn.Close(websocket.StatusInternalError, "server closed")
	c := &clientConn{
		hub:    h,
		UserID: userID,
		// r.Context() stays live while we block in the handler; the client
		// going away cancels it.
		ctx:    ctx,
		conn:   conn,
		outbox: make(chan model.WSEnvelope, 64),
		rooms:  map[string]struct{}{},
	}
	h.Connect(c)
	defer h.Disconnect(c)

	c.send(model.WSEnvelope{Type: "connected", Data: map[string]any{"user_id": userID}})
	go h.writeLoop(c)
	h.readLoop(c)
	return nil
}

// writeLoop drains the outbox; heartbeat pings every 30s.
func (h *Hub) writeLoop(c *clientConn) {
	ctx := c.ctx
	h.Log.InfoContext(ctx, "ws writeLoop start", "user", c.UserID)
	ping := time.NewTicker(30 * time.Second)
	defer ping.Stop()
	for {
		select {
		case env := <-c.outbox:
			raw, err := json.Marshal(env)
			if err != nil {
				h.Log.DebugContext(ctx, "ws marshal failed", "err", err)
				continue
			}
			if werr := c.conn.Write(ctx, websocket.MessageText, raw); werr != nil {
				h.Log.WarnContext(ctx, "ws write failed", "user", c.UserID, "err", werr)
				return
			}
			h.Log.DebugContext(ctx, "ws frame sent", "user", c.UserID, "type", env.Type)
		case <-ping.C:
			if perr := c.conn.Ping(ctx); perr != nil {
				return
			}
		case <-ctx.Done():
			return
		}
	}
}

// readLoop consumes frames: subscribe (membership-checked), unsubscribe,
// ack (delivered receipts), read, typing relay.
func (h *Hub) readLoop(c *clientConn) {
	const maxFrame = 64 * 1024
	type frameIn struct {
		Type       string   `json:"type"`
		RoomID     string   `json:"room_id"`
		MessageIDs []string `json:"message_ids"`
		MessageID  string   `json:"message_id"`
	}
	for {
		_, raw, err := c.conn.Read(c.ctx)
		if err != nil {
			return
		}
		if len(raw) > maxFrame {
			c.send(errorEnvelope("frame too large"))
			continue
		}
		var f frameIn
		if err := json.Unmarshal(raw, &f); err != nil {
			c.send(errorEnvelope("bad frame"))
			continue
		}
		switch f.Type {
		case "subscribe":
			if h.App == nil || h.App.Rooms == nil {
				c.send(errorEnvelope("server not ready"))
				continue
			}
			if err := h.App.Rooms.CheckSubscribable(c.ctx, PrincipalFor(c.UserID), f.RoomID); err != nil {
				c.send(errorEnvelope("not a member of room"))
				continue
			}
			c.mu.Lock()
			c.rooms[f.RoomID] = struct{}{}
			c.mu.Unlock()
			h.Subscribe(c, f.RoomID)
		case "unsubscribe":
			c.mu.Lock()
			delete(c.rooms, f.RoomID)
			c.mu.Unlock()
			h.Unsubscribe(c, f.RoomID)
		case "typing.started", "typing.stopped":
			c.mu.Lock()
			_, member := c.rooms[f.RoomID]
			c.mu.Unlock()
			if !member {
				c.send(errorEnvelope("not subscribed"))
				continue
			}
			h.ToRoom(f.RoomID, model.WSEnvelope{
				Type: f.Type, Data: map[string]any{"user_id": c.UserID},
			})
		case "ack":
			for _, mid := range f.MessageIDs {
				if err := h.ackDelivered(c.ctx, c.UserID, mid); err != nil {
					h.Log.DebugContext(c.ctx, "ack failed", "message", mid, "err", err)
				}
			}
		case "read":
			if f.RoomID != "" && f.MessageID != "" && h.App != nil && h.App.Rooms != nil {
				if err := h.App.Rooms.Read(c.ctx, PrincipalFor(c.UserID), f.RoomID, f.MessageID); err != nil {
					c.send(errorEnvelope("read failed"))
				}
			}
		default:
			c.send(errorEnvelope("unknown frame type"))
		}
	}
}

// ackDelivered records a delivered receipt and notifies the author.
func (h *Hub) ackDelivered(ctx context.Context, userID, messageID string) error {
	if h.App == nil {
		return errNotReady
	}
	return h.App.Messages.SocketAck(ctx, userID, messageID)
}

func errorEnvelope(msg string) model.WSEnvelope {
	return model.WSEnvelope{Type: "error", Data: map[string]any{"error": msg}}
}

// errNotReady: hub used before app wiring.
type errNotReadyT struct{}

func (errNotReadyT) Error() string { return "hub not wired to app" }

var errNotReady = errNotReadyT{}

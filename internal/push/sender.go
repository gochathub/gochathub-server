// Package push sends Web Push notifications (RFC 8030/8291/8292) to
// registered UnifiedPush endpoints, per docs/UNIFIEDPUSH.md.
// Contract details: endpoints are opaque capability URLs; payloads stay
// minimal (identifiers only, docs/PUSH.md); the SSRF guard rejects
// non-global endpoint addresses on every send (UNIFIEDPUSH.md §3.4).
package push

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"

	webpush "github.com/SherClockHolmes/webpush-go"

	"github.com/gochathub/gochathub-server/internal/store"
)

// Sender posts encrypted pushes to endpoint URLs.
type Sender struct {
	Store      *store.Store
	Log        *slog.Logger
	HTTP       *http.Client
	AllowHosts []string
	// Subscriber is the VAPID `sub` claim (mailto: or https: URI).
	Subscriber string
	// NtfyQueryFlag appends "?up=1"-style flags for ntfy UnifiedPush topics
	// (docs.ntfy.sh: alias query parameter must be lowercase). Empty = off.
	NtfyQueryFlag string

	// ActiveIn is the push-suppression hook: reports whether the user is
	// subscribed to the room over WS right now (docs/PUSH.md).
	ActiveIn func(userID, roomID string) bool

	keyPair *KeyPair
}

type KeyPair struct {
	Private string // base64url per webpush-go
	Public  string // base64url, 87 chars — matches UPC spec VAPID shape
}

// EnsureVAPID returns the stored key pair, generating and persisting one when
// absent (config wins; DB persistence keeps the public key stable across
// reboots so the Android app's distributor registration stays valid).
func (s *Sender) EnsureVAPID(ctx context.Context, cfgPrivate, cfgPublic string) (*KeyPair, error) {
	if cfgPrivate != "" && cfgPublic != "" {
		s.keyPair = &KeyPair{Private: cfgPrivate, Public: cfgPublic}
		return s.keyPair, nil
	}
	priv, err := s.Store.GetConfigValue(ctx, "vapid_private_key")
	if err == nil && priv != "" {
		pub, err := s.Store.GetConfigValue(ctx, "vapid_public_key")
		if err == nil && pub != "" {
			s.keyPair = &KeyPair{Private: priv, Public: pub}
			return s.keyPair, nil
		}
	}
	priv, pub, err := webpush.GenerateVAPIDKeys()
	if err != nil {
		return nil, fmt.Errorf("generate vapid keys: %w", err)
	}
	if err := s.Store.SetConfigValue(ctx, "vapid_private_key", priv); err != nil {
		return nil, fmt.Errorf("persist vapid keys: %w", err)
	}
	if err := s.Store.SetConfigValue(ctx, "vapid_public_key", pub); err != nil {
		return nil, fmt.Errorf("persist vapid keys: %w", err)
	}
	s.keyPair = &KeyPair{Private: priv, Public: pub}
	return s.keyPair, nil
}

// PublicKey returns the persisted VAPID public key.
func (s *Sender) PublicKey() string {
	if s.keyPair == nil {
		return ""
	}
	return s.keyPair.Public
}

// payload is the minimal push body (docs/PUSH.md identifiers only).
type payload struct {
	Type      string `json:"type"`
	RoomID    string `json:"room_id"`
	MessageID string `json:"message_id"`
}

// NotifyMessage implements service.PushSender policy: push direct messages,
// mentions, and per-member notification modes; skip the author and members
// already subscribed to the room over WS.
func (s *Sender) NotifyMessage(ctx context.Context, roomID, messageID, authorID string, mentionedUserIDs []string) {
	if s.keyPair == nil {
		return
	}
	memberIDs, roomType, err := s.Store.RoomMemberIDs(ctx, roomID)
	if err != nil {
		s.Log.WarnContext(ctx, "push: member lookup failed", "room", roomID, "err", err)
		return
	}
	isDirect := roomType == "direct"
	mentionSet := map[string]bool{}
	for _, id := range mentionedUserIDs {
		mentionSet[id] = true
	}
	targets := make([]string, 0, len(memberIDs))
	for _, uid := range memberIDs {
		if uid == authorID {
			continue
		}
		mode, err := s.Store.MemberNotifModeWithDefault(ctx, uid, roomID)
		if err != nil {
			mode = "mentions" // store default row is written at user creation
		}
		wants := mode == "all" || (mode == "directs" && isDirect) || (mode == "mentions" && mentionSet[uid])
		if !wants || mode == "never" {
			continue
		}
		if s.ActiveIn != nil && s.ActiveIn(uid, roomID) {
			continue // user is viewing the room
		}
		targets = append(targets, uid)
	}
	if len(targets) == 0 {
		s.Log.DebugContext(ctx, "push: no targets", "room", roomID)
		return
	}
	endpoints, err := s.Store.ValidatedEndpointsForUsers(ctx, targets)
	if err != nil {
		s.Log.WarnContext(ctx, "push: endpoint lookup failed", "room", roomID, "err", err)
		return
	}
	s.Log.DebugContext(ctx, "push: sending", "room", roomID, "message", messageID, "targets", len(targets), "endpoints", len(endpoints))
	body, err := json.Marshal(payload{Type: "chat.message", RoomID: roomID, MessageID: messageID})
	if err != nil {
		return
	}
	for _, e := range endpoints {
		status, err := s.send(ctx, e, body, webpush.UrgencyNormal)
		s.Log.DebugContext(ctx, "push: send result", "endpoint_host", hostOnly(e.Endpoint), "status", status, "err", err)
		if err != nil {
			s.Log.WarnContext(ctx, "push: send failed", "endpoint", e.ID, "status", status, "err", err)
			if status == 404 || status == 410 {
				_ = s.Store.RevokePushEndpointByID(ctx, e.ID)
			}
			continue
		}
		_ = s.Store.TouchPushEndpoint(ctx, e.ID)
	}
}

// hostOnly trims a URL to scheme://host for logs.
func hostOnly(raw string) string {
	if i := strings.Index(raw, "://"); i > 0 {
		if rest := strings.TrimPrefix(raw[i+3:], ""); rest != "" {
			if j := strings.IndexAny(rest, "/?"); j >= 0 {
				return raw[:i+3+j]
			}
		}
		return raw[:i+3]
	}
	return raw
}

// PingValidation sends the validation token to a freshly registered endpoint.
func (s *Sender) PingValidation(ctx context.Context, endpointRow store.PushEndpointRow, tokenB64 string) error {
	_, err := s.send(ctx, endpointRow, []byte(tokenB64), webpush.UrgencyHigh)
	return err
}

// send encrypts and posts one message. Returns the response status on HTTP
// failures; 404/410 classify dead endpoints (UNIFIEDPUSH.md §3.5).
func (s *Sender) send(ctx context.Context, e store.PushEndpointRow, body []byte, urgency webpush.Urgency) (int, error) {
	endpoint := e.Endpoint
	if s.NtfyQueryFlag != "" && !strings.Contains(endpoint, "?") {
		endpoint += "?" + s.NtfyQueryFlag + "=1"
	}
	if err := checkEndpointHost(ctx, endpoint, s.AllowHosts); err != nil {
		return 0, err
	}
	sub := &webpush.Subscription{
		Endpoint: endpoint,
		Keys: webpush.Keys{
			P256dh: e.PublicKey,
			Auth:   e.AuthSecret,
		},
	}
	opts := &webpush.Options{
		Subscriber:      s.Subscriber,
		VAPIDPublicKey:  s.keyPair.Public,
		VAPIDPrivateKey: s.keyPair.Private,
		TTL:             300,
		Urgency:         urgency,
		HTTPClient:      s.HTTP,
	}
	resp, err := webpush.SendNotificationWithContext(ctx, body, sub, opts)
	if err != nil {
		return 0, fmt.Errorf("push send: %w", err)
	}
	defer func() {
		if resp.Body != nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
	}()
	if resp.StatusCode/100 != 2 {
		return resp.StatusCode, fmt.Errorf("push service returned %d", resp.StatusCode)
	}
	return resp.StatusCode, nil
}

// checkEndpointHost resolves the endpoint and rejects non-global addresses —
// for every send, after resolution, per UNC §3.4.
func checkEndpointHost(ctx context.Context, rawEndpoint string, allowHosts []string) error {
	u, err := url.Parse(rawEndpoint)
	if err != nil {
		return fmt.Errorf("bad endpoint url: %w", err)
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return fmt.Errorf("bad endpoint scheme %q", u.Scheme)
	}
	host := u.Hostname()
	if host == "" {
		return errors.New("endpoint has no host")
	}
	for _, a := range allowHosts {
		if strings.EqualFold(a, host) {
			return nil // configured trust wins
		}
	}
	ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return fmt.Errorf("resolve %s: %w", host, err)
	}
	if len(ips) == 0 {
		return errors.New("no addresses for endpoint host")
	}
	for _, ip := range ips {
		if !isGlobalIP(ip.IP) {
			return fmt.Errorf("endpoint host %s resolves to non-global address %s; allow-list it via PUSH_ALLOW_HOSTS if intended", host, ip.IP)
		}
	}
	return nil
}

// isGlobalIP rejects private/reserved ranges (RFC 1918, RFC 4193, loopback,
// link-local, and other non-global space). Minimum set per UNC §3.4.
func isGlobalIP(ip net.IP) bool {
	if ip4 := ip.To4(); ip4 != nil {
		return !isPrivateV4(ip4)
	}
	return isGlobalV6(ip)
}

func isPrivateV4(ip net.IP) bool {
	private := []string{"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "127.0.0.0/8",
		"169.254.0.0/16", "0.0.0.0/8", "100.64.0.0/10", "198.18.0.0/15", "224.0.0.0/4", "240.0.0.0/4"}
	for _, cidr := range private {
		_, network, _ := net.ParseCIDR(cidr)
		if network.Contains(ip) {
			return true
		}
	}
	return false
}

func isGlobalV6(ip net.IP) bool {
	reserved := []string{"fc00::/7", "fe80::/10", "::1/128", "::/128", "ff00::/8", "2002::/16"}
	for _, cidr := range reserved {
		_, network, _ := net.ParseCIDR(cidr)
		if network.Contains(ip) {
			return false
		}
	}
	return true
}

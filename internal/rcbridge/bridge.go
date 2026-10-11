// Package rcbridge mirrors a user's live Rocket.Chat workspace into local
// rooms (docs/ROCKETCHAT-INTEGRATION.md, ADR-024): per-user workspace URL and
// Personal Access Token, subscriptions delta polling, per-owner mirrored rows
// (ids derive from owner + workspace + remote id), and send-through via
// chat.postMessage.
package rcbridge

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gochathub/gochathub-server/internal/config"
	"github.com/gochathub/gochathub-server/internal/model"
	"github.com/gochathub/gochathub-server/internal/service"
	"github.com/gochathub/gochathub-server/internal/store"
	"github.com/google/uuid"
)

// Bridge runs the Rocket.Chat live integration for the whole process; the
// workspace URL and credentials live on each user's link. A nil Bridge
// (RC_PAT_KEY unset) means the feature is disabled everywhere.
type Bridge struct {
	App         *service.App
	DefaultBase string // optional RC_BASE_URL offered when a link omits one
	AllowHosts  []string
	PatKey      [32]byte
	Interval    time.Duration
	HTTP        *http.Client
	Log         *slog.Logger

	mu    sync.Mutex
	paths map[string]string // learned endpoint names per (kind, workspace)
}

// New builds the bridge; nil, nil when the integration is disabled.
func New(app *service.App, cfg *config.Config, log *slog.Logger) (*Bridge, error) {
	if cfg.RCPatKey == "" {
		return nil, nil
	}
	return &Bridge{
		App:         app,
		DefaultBase: cfg.RCBaseURL,
		AllowHosts:  cfg.RCAllowHosts,
		PatKey:      sha256.Sum256([]byte(cfg.RCPatKey)),
		Interval:    max(10*time.Second, cfg.RCLinkSync),
		HTTP: &http.Client{
			Timeout: 15 * time.Second,
			// the API never redirects; following one would sidestep the host guard
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		Log:   log,
		paths: map[string]string{},
	}, nil
}

// Run polls every linked account until ctx ends. One goroutine total; links
// are processed sequentially.
// ponytail: a slow or rate-limited workspace delays the rest of the pass —
// per-workspace worker pools if links grow past a few dozen.
func (b *Bridge) Run(ctx context.Context) {
	b.Log.Info("rocket.chat bridge started", "interval", b.Interval.String())
	t := time.NewTicker(b.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			b.tick(ctx)
		}
	}
}

func (b *Bridge) tick(ctx context.Context) {
	links, err := b.App.Store.ActiveRCLinks(ctx)
	if err != nil {
		b.Log.Warn("rc bridge: link list failed", "err", err)
		return
	}
	for _, l := range links {
		if err := b.syncUser(ctx, l); err != nil {
			var rl RateLimitError
			if errors.As(err, &rl) {
				b.Log.Warn("rc bridge: rate limited", "user", l.UserID, "retry_after", rl.RetryAfter.String())
				select {
				case <-ctx.Done():
					return
				case <-time.After(rl.RetryAfter):
				}
				continue
			}
			b.Log.Warn("rc bridge: sync failed", "user", l.UserID, "err", err)
		}
	}
}

// --- link management (the send path lives in Send()) ---

// Link validates the pasted credentials against the workspace, stores the PAT
// encrypted, and kicks off an immediate first sync (rooms + backfill land in
// seconds, not a full interval later).
func (b *Bridge) Link(ctx context.Context, p service.Principal, in service.LinkRequest) (service.LinkStatus, error) {
	uid := strings.TrimSpace(in.RCUserID)
	pat := strings.TrimSpace(in.Token)
	if uid == "" || len(pat) < 16 {
		return service.LinkStatus{}, service.Bad("rc_user_id (or username) and a Personal Access Token are required")
	}
	base, err := normalizeBase(in.BaseURL, b.DefaultBase)
	if err != nil {
		return service.LinkStatus{}, err
	}
	user, err := b.whoami(ctx, target{base: base, pat: pat, uid: uid})
	if err != nil {
		if isGuardErr(err) {
			return service.LinkStatus{}, service.Bad("%s", err.Error())
		}
		return service.LinkStatus{}, err
	}
	enc, err := b.sealPat([]byte(pat))
	if err != nil {
		return service.LinkStatus{}, fmt.Errorf("seal pat: %w", err)
	}
	l := store.RCLink{UserID: p.UserID, BaseURL: base, RCUserID: user.ID, RCUsername: user.Username, PatEnc: enc}
	if err := b.App.Store.InsertRCLink(ctx, &l); err != nil {
		return service.LinkStatus{}, fmt.Errorf("store link: %w", err)
	}
	_ = b.App.Store.Audit(ctx, p.UserID, "rocket_chat.link", "rocket_chat_link", p.UserID,
		[]byte(`{"base_url":`+jsonStr(base)+`,"rc_user_id":`+jsonStr(user.ID)+`}`), nil)
	// first sync off the request path; errors only logged
	go func() {
		bg := context.WithoutCancel(ctx)
		if err := b.syncUser(bg, l); err != nil { // l already carries the sealed PAT
			b.Log.Warn("rc bridge: first sync failed", "user", p.UserID, "err", err)
		}
	}()
	return linkStatus(l), nil
}

func linkStatus(l store.RCLink) service.LinkStatus {
	return service.LinkStatus{Linked: true, BaseURL: l.BaseURL, RCUserID: l.RCUserID, RCUsername: l.RCUsername}
}

// normalizeBase reduces a user-supplied workspace URL to scheme://host[:port]
// (no path, query, userinfo). Empty falls back to the deployment default.
func normalizeBase(raw, def string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		raw = def
	}
	if raw == "" {
		return "", service.Bad("base_url is required")
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Hostname() == "" || u.User != nil {
		return "", service.Bad("base_url must be an http(s) workspace origin")
	}
	return u.Scheme + "://" + u.Host, nil
}

func jsonStr(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// Status reports the caller's link without ever echoing the PAT.
func (b *Bridge) Status(ctx context.Context, p service.Principal) (service.LinkStatus, error) {
	l, err := b.App.Store.RCLinkByUser(ctx, p.UserID)
	if errors.Is(err, store.ErrNotFound) {
		return service.LinkStatus{}, nil
	}
	if err != nil {
		return service.LinkStatus{}, err
	}
	return linkStatus(l), nil
}

// Unlink destroys the stored PAT and hides the user's mirrored rooms
// (archived, not deleted: a re-link brings the same rooms back).
func (b *Bridge) Unlink(ctx context.Context, p service.Principal) error {
	err := b.App.Store.DeleteRCLink(ctx, p.UserID)
	if err != nil {
		return err
	}
	return b.App.Store.Audit(ctx, p.UserID, "rocket_chat.unlink", "rocket_chat_link", p.UserID, []byte(`{}`), nil)
}

// --- sync ---

// syncUser pulls the subscription delta for one link and refreshes dirty
// rooms. Watermark advances on success; failures leave it (retry next tick).
func (b *Bridge) syncUser(ctx context.Context, l store.RCLink) error {
	pat, err := b.openPat(l.PatEnc)
	if err != nil {
		return fmt.Errorf("open pat: %w", err)
	}
	var since *time.Time
	if l.Watermark != nil {
		since = l.Watermark
	}
	t := target{base: l.BaseURL, pat: pat, uid: l.RCUserID}
	changed, removed, err := b.listSubs(ctx, t, since)
	if err != nil {
		var ae AuthError
		if errors.As(err, &ae) {
			b.Log.Warn("rc bridge: credentials rejected", "user", l.UserID)
			return nil // not an error burst; next tick tries again
		}
		return err
	}
	maxUpd := time.Time{} // zero
	var dirty []rcSub
	for _, s := range changed {
		if s.RID == "" {
			continue
		}
		if s.UpdatedAt.Time.After(maxUpd) {
			maxUpd = s.UpdatedAt.Time
		}
		if s.T != "d" && s.T != "p" && s.T != "c" { // livechat (l): out of scope
			continue
		}
		dirty = append(dirty, s)
	}
	for _, r := range removed {
		if r.RID != "" {
			if err := b.App.Store.ArchiveRCRoom(ctx, rcID("room", l, r.RID)); err != nil {
				b.Log.Warn("rc bridge: archive on remove failed", "rid", r.RID, "err", err)
			}
		}
	}
	for _, s := range dirty {
		if err := b.syncRoom(ctx, l, t, s); err != nil {
			var ae AuthError
			if errors.As(err, &ae) {
				return err
			}
			b.Log.Warn("rc bridge: room sync failed", "rid", s.RID, "err", err)
		}
	}
	if !maxUpd.IsZero() && (l.Watermark == nil || maxUpd.After(*l.Watermark)) {
		if err := b.App.Store.SetRCLinkWatermark(ctx, l.UserID, maxUpd); err != nil {
			return fmt.Errorf("watermark: %w", err)
		}
	}
	return nil
}

// syncRoom ensures the local room exists (backfilling when fresh) and pulls
// new messages when RC reports newer activity than our last pull.
func (b *Bridge) syncRoom(ctx context.Context, l store.RCLink, t target, s rcSub) error {
	room, fresh, err := b.ensureRoom(ctx, l, s)
	if err != nil {
		return err
	}
	return b.pullRoom(ctx, l, t, room, s.T, room.SourceSyncedAt, fresh)
}

// ensureRoom maps the subscription to this owner's local room (ids derive from
// owner + workspace + remote id, so no row is ever shared between users);
// fresh = first sighting. The owner is (re)joined every time: membership is
// idempotent and an unlinked-then-relinked room must stay visible.
func (b *Bridge) ensureRoom(ctx context.Context, l store.RCLink, s rcSub) (store.RoomRow, bool, error) {
	if s.T == "d" && s.Name == "" {
		return store.RoomRow{}, false, errors.New("dm subscription has no counterpart name")
	}
	name := s.Name
	room := store.RoomRow{
		ID:        rcID("room", l, s.RID),
		Type:      "direct",
		Source:    store.SrcRocketChat,
		SourceRID: s.RID,
		CreatedBy: l.UserID,
		Name:      &name,
	}
	if s.T == "p" || s.T == "c" { // channels too: owner-only private mirror, never a public local room
		room.Type = "private"
	}
	fresh, err := b.App.Store.UpsertRCRoom(ctx, &room, s.TS.Time)
	if err != nil {
		return store.RoomRow{}, false, err
	}
	if err := b.App.Store.AddRCRoomMember(ctx, room.ID, l.UserID); err != nil {
		return store.RoomRow{}, false, fmt.Errorf("join link owner: %w", err)
	}
	return room, fresh, nil
}

// pullRoom fetches messages (oldest, latest] newest-first and inserts them.
// fresh also stamps the link owner's receipts read so a fresh link opens with
// zero noise; later deliveries stay unread until read here.
func (b *Bridge) pullRoom(ctx context.Context, l store.RCLink, t target, room store.RoomRow, rcType string, oldest *time.Time, fresh bool) error {
	msgs, err := b.history(ctx, t, room.SourceRID, rcType, oldest)
	if err != nil {
		return err
	}
	n := 0
	maxTS := time.Time{}                  // monotonic watermark floor per room
	for i := len(msgs) - 1; i >= 0; i-- { // ascending: replies land after their parents
		m := msgs[i]
		inserted, err := b.insertRcMsg(ctx, l, room, m)
		if err != nil {
			b.Log.Warn("rc bridge: message insert failed", "rid", room.SourceRID, "mid", m.ID, "err", err)
			continue
		}
		if inserted {
			n++
		}
		if m.TS.Time.After(maxTS) && m.TS.Time.Before(time.Now().Add(time.Minute)) {
			maxTS = m.TS.Time // clock-skew guard: don't stamp the future
		}
	}
	if maxTS.IsZero() && oldest != nil && len(msgs) == 0 {
		maxTS = *oldest // nothing new: watermark unchanged but re-stamped
	} else if maxTS.IsZero() {
		maxTS = time.Now().UTC()
	}
	if err := b.App.Store.SetRoomSourceSynced(ctx, room.ID, maxTS); err != nil {
		return fmt.Errorf("room watermark: %w", err)
	}
	if fresh && n > 0 {
		if err := b.App.Store.StampReceiptsRead(ctx, room.ID, l.UserID, maxTS.Add(time.Second)); err != nil {
			b.Log.Warn("rc bridge: backfill read-stamp failed", "room", room.ID, "err", err)
		}
	}
	return nil
}

// insertRcMsg turns one RC message into a local row; returns true when new.
func (b *Bridge) insertRcMsg(ctx context.Context, l store.RCLink, room store.RoomRow, m rcMsg) (bool, error) {
	if m.Msg == "" || m.ID == "" || m.TS.IsZero() {
		return false, nil // file-only or undeclared system content: skip
	}
	authorID, err := b.stubOrSelf(ctx, l, room, m.U)
	if err != nil {
		return false, err
	}
	var reply *string
	if m.TMID != "" {
		replyTo := rcID("msg", l, m.TMID)
		if ref, rerr := b.App.Store.MessageByID(ctx, replyTo); rerr == nil && ref.RoomID == room.ID {
			reply = &replyTo
		}
	}
	row := store.MessageRow{
		ID:               rcID("msg", l, m.ID),
		RoomID:           room.ID,
		AuthorID:         authorID,
		ReplyToMessageID: reply,
		Body:             m.Msg,
		Format:           "markdown",
		CreatedAt:        m.TS.Time,
	}
	inserted, err := b.App.Store.InsertRCMessage(ctx, &row)
	if err != nil || !inserted {
		return inserted, err
	}
	// standard downstream: real-time room fan-out + push for offline members
	msg, err := b.App.Messages.Hydrate(ctx, row, l.UserID)
	if err != nil {
		return true, fmt.Errorf("hydrate: %w", err)
	}
	b.App.Notify.ToRoom(room.ID, service.NewEnvelope(ctx, "message.created", room.ID, map[string]any{"message": msg}))
	if b.App.Sender != nil {
		go b.App.Sender.NotifyMessage(context.WithoutCancel(ctx), room.ID, row.ID, authorID, nil)
	}
	return true, nil
}

// stubOrSelf maps an RC author to a local user: the link owner's own RC
// account becomes the real user; anyone else gets (or already has) a stub.
func (b *Bridge) stubOrSelf(ctx context.Context, l store.RCLink, room store.RoomRow, u rcUser) (string, error) {
	if u.ID == "" {
		return "", errors.New("rc message without author")
	}
	if u.ID == l.RCUserID {
		return l.UserID, nil
	}
	stub := rcID("user", l, u.ID)
	if err := b.App.Store.UpsertStubUser(ctx, stub, sanitizeName(u.Username)); err != nil {
		return "", err
	}
	// DM shadow member so members-listing renders the conversation normally
	if room.Type == "direct" {
		if err := b.App.Store.AddRCRoomMember(ctx, room.ID, stub); err != nil {
			return "", err
		}
	}
	return stub, nil
}

// sanitizeName coerces an RC username into the local users.username rules
// (lowercase, [a-z0-9_.-], 1..64). Empty/invalid names fall back to the id.
func sanitizeName(raw string) string {
	n := strings.ToLower(strings.TrimSpace(raw))
	var b strings.Builder
	for _, r := range n {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_' || r == '.' || r == '-' {
			b.WriteRune(r)
		}
	}
	out := b.String()
	if out == "" || len(out) > 64 {
		out = "rcuser"
	}
	return out
}

// --- send-through ---

// Send posts a message into the RC room and mirrors the RC echo as the local
// row (deterministic id from the RC _id — the sync race with our own echo
// dedupes itself). Implements service.RCSender; MessageService.Create routes
// source-marked rooms here after its own validation.
func (b *Bridge) Send(ctx context.Context, p service.Principal, roomID string, in service.MessageInput) (model.Message, error) {
	room, _, err := b.App.Store.RoomForUser(ctx, roomID, p.UserID)
	if err != nil {
		return model.Message{}, service.ErrNotFound
	}
	// only the owner's own mirror can post through the owner's link
	if room.Source != store.SrcRocketChat || room.SourceRID == "" || room.CreatedBy != p.UserID {
		return model.Message{}, service.ErrNotFound
	}
	if in.ReplyToID != "" {
		return model.Message{}, service.Bad("thread replies are not supported in cross-server rooms")
	}
	l, err := b.App.Store.RCLinkByUser(ctx, p.UserID)
	if errors.Is(err, store.ErrNotFound) {
		return model.Message{}, service.Bad("rocket.chat account not linked (re-link required)")
	}
	if err != nil {
		return model.Message{}, err
	}
	pat, err := b.openPat(l.PatEnc)
	if err != nil {
		return model.Message{}, service.Bad("stored rocket.chat token unreadable — link again")
	}
	echo, err := b.postMessage(ctx, target{base: l.BaseURL, pat: pat, uid: l.RCUserID}, room.SourceRID, in.Body)
	if err != nil {
		var ae AuthError
		if errors.As(err, &ae) {
			return model.Message{}, service.Bad("rocket.chat rejected the stored token — re-link required")
		}
		if isGuardErr(err) {
			return model.Message{}, service.Bad("%s", err.Error())
		}
		return model.Message{}, fmt.Errorf("rc post: %w", err)
	}
	if echo.ID == "" {
		return model.Message{}, errors.New("rc post: response without message id")
	}
	row := store.MessageRow{
		ID:        rcID("msg", l, echo.ID),
		RoomID:    room.ID,
		AuthorID:  p.UserID, // the local account, not a stub: it is really them
		Body:      in.Body,
		Format:    "markdown",
		CreatedAt: echo.TS.Time,
	}
	inserted, err := b.App.Store.InsertRCMessage(ctx, &row)
	if err != nil {
		return model.Message{}, fmt.Errorf("mirror own echo: %w", err)
	}
	msg, err := b.App.Messages.Hydrate(ctx, row, p.UserID)
	if err != nil {
		return model.Message{}, fmt.Errorf("hydrate: %w", err)
	}
	if inserted { // if the sync loop raced the echo in, row already exists — no replay
		b.App.Notify.ToRoom(room.ID, service.NewEnvelope(ctx, "message.created", room.ID, map[string]any{"message": msg}))
		if b.App.Sender != nil {
			go b.App.Sender.NotifyMessage(context.WithoutCancel(ctx), room.ID, row.ID, p.UserID, nil)
		}
	}
	return msg, nil
}

// --- id space ---

var nsBridge = uuid.NewSHA1(uuid.NameSpaceURL, []byte("https://gochathub.app/bridge/rc"))

// rcID derives the local id of a mirrored room/message/shadow user from the
// link owner, the owner's workspace and the remote id: deterministic (a
// re-sync or re-link lands on the same rows) and unique per owner, so mirrored
// data is never shared between users and two workspaces can't collide.
// Deliberately NOT the rcmigrate namespaces (ADR-024): imported rows are not
// adopted by the live bridge.
func rcID(kind string, l store.RCLink, remote string) string {
	return uuid.NewSHA1(nsBridge, []byte(kind+"|"+l.UserID+"|"+l.BaseURL+"|"+remote)).String()
}

// sealPat / openPat: AES-256-GCM with the env-provided key.
func (b *Bridge) sealPat(plain []byte) ([]byte, error) {
	gcm, aerr := aes.NewCipher(b.PatKey[:])
	if aerr != nil { // unreachable: 32-byte key
		return nil, aerr
	}
	aead, err := cipher.NewGCM(gcm)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return aead.Seal(nonce, nonce, plain, nil), nil
}

func (b *Bridge) openPat(enc []byte) (string, error) {
	gcm, aerr := aes.NewCipher(b.PatKey[:])
	if aerr != nil {
		return "", aerr
	}
	aead, gerr := cipher.NewGCM(gcm)
	if gerr != nil {
		return "", gerr
	}
	if len(enc) < aead.NonceSize() {
		return "", errors.New("pat: too short")
	}
	plain, err := aead.Open(nil, enc[:aead.NonceSize()], enc[aead.NonceSize():], nil)
	if err != nil {
		return "", err
	}
	return string(plain), nil
}

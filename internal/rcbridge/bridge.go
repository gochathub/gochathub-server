// Package rcbridge mirrors a live Rocket.Chat workspace into local rooms
// (docs/ROCKETCHAT-INTEGRATION.md): per-user Personal Access Token
// delegation, subscriptions delta polling, persistence in the shared uuidv5
// id space (import continuity with rcmigrate), and send-through via
// chat.postMessage.
package rcbridge

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
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
	"github.com/gochathub/gochathub-server/internal/rcids"
	"github.com/gochathub/gochathub-server/internal/service"
	"github.com/gochathub/gochathub-server/internal/store"
	"github.com/google/uuid"
)

// Bridge runs the Rocket.Chat live integration for the whole process.
// Zero-value fields beyond App are set by New; nil Bridge (no RC_BASE_URL)
// means the feature is disabled everywhere.
type Bridge struct {
	App      *service.App
	BaseURL  string // workspace origin, no trailing slash
	PatKey   [32]byte
	Interval time.Duration
	HTTP     *http.Client
	Log      *slog.Logger

	mu       sync.Mutex
	listPath string
	histPath string
}

// New builds the bridge; nil, nil when the integration is disabled.
func New(app *service.App, cfg *config.Config, log *slog.Logger) (*Bridge, error) {
	if cfg.RCBaseURL == "" {
		return nil, nil
	}
	if _, err := url.Parse(cfg.RCBaseURL); err != nil {
		return nil, fmt.Errorf("rc bridge: %w", err)
	}
	return &Bridge{
		App:      app,
		BaseURL:  strings.TrimRight(cfg.RCBaseURL, "/"),
		PatKey:   sha256.Sum256([]byte(cfg.RCPatKey)),
		Interval: max(10*time.Second, cfg.RCLinkSync),
		HTTP:     &http.Client{Timeout: 15 * time.Second},
		Log:      log,
	}, nil
}

// Run polls every linked account until ctx ends. One goroutine total; links
// are processed sequentially — a shared REST rate-limit budget at the RC
// workspace makes fan-out counterproductive.
func (b *Bridge) Run(ctx context.Context) {
	b.Log.Info("rocket.chat bridge started", "interval", b.Interval.String(), "base_url", b.BaseURL)
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
	user, err := b.whoami(ctx, pat, uid)
	if err != nil {
		return service.LinkStatus{}, err
	}
	enc, err := b.sealPat([]byte(pat))
	if err != nil {
		return service.LinkStatus{}, fmt.Errorf("seal pat: %w", err)
	}
	l := store.RCLink{UserID: p.UserID, RCUserID: user.ID, RCUsername: user.Username, PatEnc: enc}
	if err := b.App.Store.InsertRCLink(ctx, &l); err != nil {
		return service.LinkStatus{}, fmt.Errorf("store link: %w", err)
	}
	_ = b.App.Store.Audit(ctx, p.UserID, "rocket_chat.link", "rocket_chat_link", p.UserID,
		[]byte(`{"rc_user_id":"`+user.ID+`","rc_username":"`+user.Username+`"}`), nil)
	// first sync off the request path; errors only logged
	go func() {
		bg := context.WithoutCancel(ctx)
		if err := b.syncUser(bg, l); err != nil { // l already carries the sealed PAT
			b.Log.Warn("rc bridge: first sync failed", "user", p.UserID, "err", err)
		}
	}()
	return service.LinkStatus{Linked: true, RCUserID: l.RCUserID, RCUsername: l.RCUsername}, nil
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
	return service.LinkStatus{Linked: true, RCUserID: l.RCUserID, RCUsername: l.RCUsername}, nil
}

// Unlink destroys the stored PAT. Mirrored rooms and their content stay as
// frozen local history (US-4 open question chose "keep").
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
	changed, removed, err := b.listSubs(ctx, pat, l.RCUserID, since)
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
		if s.T != "d" && s.T != "p" { // channels (c), livechat (l): out of scope
			continue
		}
		dirty = append(dirty, s)
	}
	for _, r := range removed {
		if r.RID != "" {
			if err := b.App.Store.ArchiveRCRoom(ctx, r.RID); err != nil {
				b.Log.Warn("rc bridge: archive on remove failed", "rid", r.RID, "err", err)
			}
		}
	}
	for _, s := range dirty {
		if err := b.syncRoom(ctx, l, pat, s); err != nil {
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
func (b *Bridge) syncRoom(ctx context.Context, l store.RCLink, pat string, s rcSub) error {
	room, fresh, err := b.ensureRoom(ctx, l, s)
	if err != nil {
		return err
	}
	return b.pullRoom(ctx, l, pat, room, room.SourceSyncedAt, fresh)
}

// ensureRoom maps the subscription to a local room via the shared nsRoom id
// space; fresh = first sighting.
func (b *Bridge) ensureRoom(ctx context.Context, l store.RCLink, s rcSub) (store.RoomRow, bool, error) {
	if s.T == "d" && s.Name == "" {
		return store.RoomRow{}, false, errors.New("dm subscription has no counterpart name")
	}
	name := s.Name
	room := store.RoomRow{
		ID:        idRoom(s.RID),
		Type:      "direct",
		Source:    store.SrcRocketChat,
		SourceRID: s.RID,
		CreatedBy: l.UserID,
		Name:      &name,
	}
	if s.T == "p" {
		room.Type = "private"
	}
	fresh, err := b.App.Store.UpsertRCRoom(ctx, &room, s.TS.Time)
	if err != nil {
		return store.RoomRow{}, false, err
	}
	if fresh {
		if err := b.App.Store.AddRCRoomMember(ctx, room.ID, l.UserID); err != nil {
			return store.RoomRow{}, false, fmt.Errorf("join link owner: %w", err)
		}
	}
	return room, fresh, nil
}

// pullRoom fetches messages (oldest, latest] newest-first and inserts them.
// fresh also stamps the link owner's receipts read so a fresh link opens with
// zero noise; later deliveries stay unread until read here.
func (b *Bridge) pullRoom(ctx context.Context, l store.RCLink, pat string, room store.RoomRow, oldest *time.Time, fresh bool) error {
	msgs, err := b.history(ctx, pat, l.RCUserID, room.SourceRID, room.Type, oldest)
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
		replyTo := idMsg(m.TMID)
		if ref, rerr := b.App.Store.MessageByID(ctx, replyTo); rerr == nil && ref.RoomID == room.ID {
			reply = &replyTo
		}
	}
	row := store.MessageRow{
		ID:               idMsg(m.ID),
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
	name := sanitizeName(u.Username)
	if _, err := b.App.Store.UpsertStubUser(ctx, idUser(u.ID), name); err != nil {
		return "", err
	}
	// DM shadow member so members-listing renders the conversation normally
	if room.Type == "direct" {
		if err := b.App.Store.AddRCRoomMember(ctx, room.ID, idUser(u.ID)); err != nil {
			return "", err
		}
	}
	return idUser(u.ID), nil
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
	if room.Source != store.SrcRocketChat || room.SourceRID == "" {
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
	echo, err := b.postMessage(ctx, pat, l.RCUserID, room.SourceRID, in.Body)
	if err != nil {
		var ae AuthError
		if errors.As(err, &ae) {
			return model.Message{}, service.Bad("rocket.chat rejected the stored token — re-link required")
		}
		return model.Message{}, fmt.Errorf("rc post: %w", err)
	}
	if echo.ID == "" {
		return model.Message{}, errors.New("rc post: response without message id")
	}
	row := store.MessageRow{
		ID:        idMsg(echo.ID),
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

// --- id space (identical to the one-shot import) ---

func idUser(rcUID string) string {
	return uuid.NewSHA1(uuid.NewSHA1(uuid.NameSpaceURL, []byte(rcids.NSUser)), []byte(rcUID)).String()
}

func idRoom(rid string) string {
	return uuid.NewSHA1(uuid.NewSHA1(uuid.NameSpaceURL, []byte(rcids.NSRoom)), []byte(rid)).String()
}

func idMsg(mid string) string {
	return uuid.NewSHA1(uuid.NewSHA1(uuid.NameSpaceURL, []byte(rcids.NSMsg)), []byte(mid)).String()
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
	aead, err := cipher.NewGCM(gcm)
	if len(enc) < aead.NonceSize() {
		return "", errors.New("pat: too short")
	}
	plain, err := aead.Open(nil, enc[:aead.NonceSize()], enc[aead.NonceSize():], nil)
	if err != nil {
		return "", err
	}
	return string(plain), nil
}

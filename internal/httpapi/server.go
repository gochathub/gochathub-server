package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/gochathub/gochathub-server/internal/config"
	"github.com/gochathub/gochathub-server/internal/service"
	"github.com/gochathub/gochathub-server/internal/ws"
)

// API is the assembled HTTP transport.
type API struct {
	svc        *service.App
	hub        *ws.Hub
	log        *slog.Logger
	cfg        *config.Config
	mux        *http.ServeMux
	rate       *limiter
	loginRate  *limiter
	trustProxy bool
	cookieName string
}

// loginRPM: dedicated per-IP bucket for password attempts; the shared global
// bucket is too generous to be the only brute-force ceiling.
const loginRPM = 10

// New builds routes + middleware chain.
func New(svc *service.App, hub *ws.Hub, log *slog.Logger, cfg *config.Config) *API {
	a := &API{
		svc:        svc,
		hub:        hub,
		log:        log,
		cfg:        cfg,
		rate:       newLimiter(cfg.RateLimitRPM),
		loginRate:  newLimiter(loginRPM),
		trustProxy: cfg.TrustProxy,
		cookieName: cookieName(cfg.CookieSecure),
	}
	a.routes()
	return a
}

// cookieName: __Host- prefix requires Secure + no domain + path=/ (ADR-015).
func cookieName(secure bool) string {
	if secure {
		return "__Host-chat_session"
	}
	return "chat_session"
}

func (a *API) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	a.mux.ServeHTTP(w, r)
}

// routes mounts everything; Go's ServeMux handles method patterns.
func (a *API) routes() {
	mux := http.NewServeMux()

	// health — no auth
	mux.HandleFunc("GET /healthz", a.handleHealthz)
	mux.HandleFunc("GET /readyz", a.handleReadyz)
	mux.HandleFunc("GET /version", a.handleVersion)

	// auth
	mux.Handle("POST /api/v1/auth/login", a.chain(false, http.HandlerFunc(a.handleLogin)))
	mux.Handle("POST /api/v1/auth/logout", a.chain(true, http.HandlerFunc(a.handleLogout)))

	// users
	mux.Handle("GET /api/v1/users/me", a.chain(true, http.HandlerFunc(a.handleMe)))
	mux.Handle("PATCH /api/v1/users/me", a.chain(true, http.HandlerFunc(a.handleUpdateMe)))
	mux.Handle("PATCH /api/v1/users/me/password", a.chain(true, http.HandlerFunc(a.handlePatchPassword)))
	mux.Handle("GET /api/v1/users/me/preferences", a.chain(true, http.HandlerFunc(a.handleGetPrefs)))
	mux.Handle("PATCH /api/v1/users/me/preferences", a.chain(true, http.HandlerFunc(a.handlePatchPrefs)))
	mux.Handle("GET /api/v1/users/me/notifications", a.chain(true, http.HandlerFunc(a.handleListNotifModes)))
	mux.Handle("POST /api/v1/users/me/notifications", a.chain(true, http.HandlerFunc(a.handleSetNotifMode)))
	mux.Handle("GET /api/v1/users/search", a.chain(true, http.HandlerFunc(a.handleSearchUsers)))
	mux.Handle("GET /api/v1/users/{userId}", a.chain(true, http.HandlerFunc(a.handleGetUser)))

	// contacts
	mux.Handle("GET /api/v1/contacts", a.chain(true, http.HandlerFunc(a.handleListContacts)))
	mux.Handle("POST /api/v1/contacts", a.chain(true, http.HandlerFunc(a.handleAddContact)))
	mux.Handle("DELETE /api/v1/contacts/{contactId}", a.chain(true, http.HandlerFunc(a.handleRemoveContact)))

	// rooms
	mux.Handle("GET /api/v1/rooms", a.chain(true, http.HandlerFunc(a.handleListRooms)))
	mux.Handle("POST /api/v1/rooms", a.chain(true, http.HandlerFunc(a.handleCreateRoom)))
	mux.Handle("GET /api/v1/rooms/{roomId}", a.chain(true, http.HandlerFunc(a.handleGetRoom)))
	mux.Handle("PATCH /api/v1/rooms/{roomId}", a.chain(true, http.HandlerFunc(a.handleUpdateRoom)))
	mux.Handle("DELETE /api/v1/rooms/{roomId}", a.chain(true, http.HandlerFunc(a.handleArchiveRoom)))
	mux.Handle("GET /api/v1/rooms/{roomId}/members", a.chain(true, http.HandlerFunc(a.handleRoomMembers)))
	mux.Handle("POST /api/v1/rooms/{roomId}/members", a.chain(true, http.HandlerFunc(a.handleAddMember)))
	mux.Handle("DELETE /api/v1/rooms/{roomId}/members/{userId}", a.chain(true, http.HandlerFunc(a.handleRemoveMember)))
	mux.Handle("PUT /api/v1/rooms/{roomId}/pin", a.chain(true, http.HandlerFunc(a.handlePin)))
	mux.Handle("DELETE /api/v1/rooms/{roomId}/pin", a.chain(true, http.HandlerFunc(a.handleUnpin)))
	mux.Handle("PUT /api/v1/rooms/{roomId}/archived", a.chain(true, http.HandlerFunc(a.handleMemberArchive)))
	mux.Handle("POST /api/v1/rooms/{roomId}/read", a.chain(true, http.HandlerFunc(a.handleRead)))

	// invites
	mux.Handle("GET /api/v1/invites", a.chain(true, http.HandlerFunc(a.handleListInvites)))
	mux.Handle("POST /api/v1/invites", a.chain(true, http.HandlerFunc(a.handleCreateInvite)))
	mux.Handle("DELETE /api/v1/invites/{inviteId}", a.chain(true, http.HandlerFunc(a.handleDeclineInvite)))
	mux.Handle("POST /api/v1/invites/{inviteId}/accept", a.chain(true, http.HandlerFunc(a.handleAcceptInvite)))
	mux.Handle("POST /api/v1/invites/{inviteId}/decline", a.chain(true, http.HandlerFunc(a.handleDeclineInvitePath)))

	// messages
	mux.Handle("GET /api/v1/rooms/{roomId}/messages", a.chain(true, http.HandlerFunc(a.handleListMessages)))
	mux.Handle("POST /api/v1/rooms/{roomId}/messages", a.chain(true, http.HandlerFunc(a.handleCreateMessage)))
	mux.Handle("GET /api/v1/messages/{messageId}", a.chain(true, http.HandlerFunc(a.handleGetMessage)))
	mux.Handle("PATCH /api/v1/messages/{messageId}", a.chain(true, http.HandlerFunc(a.handleEditMessage)))
	mux.Handle("DELETE /api/v1/messages/{messageId}", a.chain(true, http.HandlerFunc(a.handleDeleteMessage)))
	mux.Handle("GET /api/v1/messages/{messageId}/reactions", a.chain(true, http.HandlerFunc(a.handleListReactions)))
	mux.Handle("POST /api/v1/messages/{messageId}/reactions", a.chain(true, http.HandlerFunc(a.handleReact)))
	mux.Handle("DELETE /api/v1/messages/{messageId}/reactions/{emoji}", a.chain(true, http.HandlerFunc(a.handleUnreact)))

	// attachments
	mux.Handle("POST /api/v1/attachments", a.chain(true, http.HandlerFunc(a.handleCreateUpload)))
	mux.Handle("GET /api/v1/attachments/{attachmentId}", a.chain(true, http.HandlerFunc(a.handleGetAttachment)))
	mux.Handle("DELETE /api/v1/attachments/{attachmentId}", a.chain(true, http.HandlerFunc(a.handleDeleteAttachment)))
	mux.Handle("POST /api/v1/attachments/{attachmentId}/complete", a.chain(true, http.HandlerFunc(a.handleCompleteUpload)))

	// push devices
	mux.Handle("POST /api/v1/devices", a.chain(true, http.HandlerFunc(a.handleRegisterDevice)))
	mux.Handle("POST /api/v1/devices/{deviceId}/validate", a.chain(true, http.HandlerFunc(a.handleValidateDevice)))
	mux.Handle("PATCH /api/v1/devices/{deviceId}", a.chain(true, http.HandlerFunc(a.handleRenewDevice)))
	mux.Handle("DELETE /api/v1/devices/{deviceId}", a.chain(true, http.HandlerFunc(a.handleDeleteDevice)))
	mux.Handle("GET /api/v1/push/vapid", a.chain(true, http.HandlerFunc(a.handleVapidKey)))

	// websocket
	mux.Handle("GET /api/v1/ws", a.chain(true, http.HandlerFunc(a.handleWS)))

	a.mux = mux
}

// chain applies the middleware. auth requires authentication.
func (a *API) chain(auth bool, h http.Handler) http.Handler {
	h = a.withRate(h)
	if auth {
		h = a.withAuth(h)
	}
	h = a.withOrigin(h)
	h = limitBody(1<<20, h) // JSON bodies are small; uploads are presigned
	h = withSecurityHeaders(h)
	h = withRequestID(h)
	h = withLogging(a.log, h)
	h = withRecover(a.log, h)
	return h
}

// withAuth resolves cookie or bearer credentials.
func (a *API) withAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if tok := bearerOf(r); tok != "" {
			p, err := a.svc.Auth.ResolveBearer(r.Context(), tok)
			if err != nil {
				a.log.DebugContext(r.Context(), "bearer rejected", "err", err)
				writeError(w, 401, "unauthorized", "invalid token")
				return
			}
			// liveness writes happen inside ResolveBearer, throttled
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxPrincipal, p)))
			return
		}
		if c, err := r.Cookie(a.cookieName); err == nil {
			p, err := a.svc.Auth.ResolveSession(r.Context(), c.Value)
			if err != nil {
				a.log.DebugContext(r.Context(), "session rejected", "cookie", a.cookieName, "err", err)
				writeError(w, 401, "unauthorized", "invalid session")
				return
			}
			// liveness writes happen inside ResolveSession, throttled
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxPrincipal, p)))
			return
		}
		writeError(w, 401, "unauthorized", "authentication required")
	})
}

// withOrigin enforces same-origin on state-changing requests that arrived
// with credentials (ADR-015). Bearer requests are exempt (no ambient auth).
func (a *API) withOrigin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet || r.Method == http.MethodHead || r.Method == http.MethodOptions {
			next.ServeHTTP(w, r)
			return
		}
		if a.svc.BaseOrigin == "" {
			next.ServeHTTP(w, r)
			return
		}
		origin := r.Header.Get("Origin")
		if origin == "" || strings.EqualFold(origin, a.svc.BaseOrigin) {
			next.ServeHTTP(w, r)
			return
		}
		writeError(w, 403, "cross_origin", "cross-origin state changes are rejected (ADR-016)")
	})
}

func bearerOf(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if v, ok := strings.CutPrefix(h, "Bearer "); ok {
		return strings.TrimSpace(v)
	}
	return ""
}

// --- shared helpers ---

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]string{"code": code, "message": message},
	})
}

func decodeJSON(r *http.Request, dst any) error {
	dec := json.NewDecoder(r.Body) // body size already capped by middleware
	if err := dec.Decode(dst); err != nil {
		return fmt.Errorf("decode body: %w", err)
	}
	return nil
}

// mapError converts service errors to the API envelope; unknown errors are
// logged with context (never leaked to clients).
func (a *API) mapError(w http.ResponseWriter, err error) {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "22P02" {
		// malformed uuid in path/body params: resource cannot exist → 404
		// (paths that already fold every lookup error into ErrNotFound do
		// this implicitly; this keeps invites/accept and friends consistent)
		writeError(w, 404, "not_found", "not found")
		return
	}
	switch {
	case errors.Is(err, service.ErrUnauthorized):
		writeError(w, 401, "unauthorized", "invalid credentials")
	case errors.Is(err, service.ErrForbidden):
		writeError(w, 403, "forbidden", "not allowed")
	case errors.Is(err, service.ErrNotFound):
		writeError(w, 404, "not_found", "not found")
	case errors.Is(err, service.ErrConflict):
		writeError(w, 409, "conflict", "duplicate or closed")
	default:
		var ve *service.ValidationError
		if errors.As(err, &ve) {
			writeError(w, 400, "validation", ve.Reason)
			return
		}
		a.log.ErrorContext(context.Background(), "handler error", "err", err)
		writeError(w, 500, "internal_error", "internal error")
	}
}

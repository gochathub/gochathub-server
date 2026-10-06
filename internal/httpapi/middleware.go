// Package httpapi is the transport: routing, middleware, JSON handlers. All
// business logic lives in internal/service.
package httpapi

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gochathub/gochathub-server/internal/service"
)

type ctxKey int

const (
	ctxPrincipal ctxKey = iota
	ctxRequestID
)

func principalFrom(ctx context.Context) (service.Principal, bool) {
	if p, ok := ctx.Value(ctxPrincipal).(service.Principal); ok {
		return p, true
	}
	return service.Principal{}, false
}

// withRecover turns panics into 500s.
func withRecover(log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				log.ErrorContext(r.Context(), "panic recovered", "panic", rec, "path", r.URL.Path)
				writeError(w, 500, "internal_error", "internal error")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// withRequestID adds X-Request-ID (echoing inbound when sane).
func withRequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-ID")
		if id == "" || len(id) > 64 || strings.ContainsAny(id, " \t\r\n") {
			b := make([]byte, 8)
			_, _ = rand.Read(b)
			id = hex.EncodeToString(b)
		}
		w.Header().Set("X-Request-ID", id)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxRequestID, id)))
	})
}

// withLogging logs method/path/status/duration.
func withLogging(log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: 200}
		next.ServeHTTP(sw, r)
		log.InfoContext(r.Context(), "request",
			"method", r.Method, "path", r.URL.Path, "status", sw.status,
			"duration_ms", time.Since(start).Milliseconds())
	})
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

// Flush lets handlers flush; harmless otherwise.
func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Hijack forwards so websocket upgrades pass through the wrapper
// (coder/websocket Accept requires http.Hijacker).
func (w *statusWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return http.NewResponseController(w.ResponseWriter).Hijack()
}

func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// withSecurityHeaders sets basic hardening headers (docs: secure headers).
func withSecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "strict-origin-when-cross-origin")
		h.Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'; base-uri 'none'")
		next.ServeHTTP(w, r)
	})
}

// limitBody caps request size per handler class (docs: request size limits).
func limitBody(max int64, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, max)
		next.ServeHTTP(w, r)
	})
}

// --- rate limiting: per-visitor (by IP) token bucket ---

type limiter struct {
	mu        sync.Mutex
	buckets   map[string]*tbucket
	rpm       int
	lastSweep time.Time
}

type tbucket struct {
	tokens float64
	last   time.Time
}

func newLimiter(rpm int) *limiter {
	return &limiter{buckets: map[string]*tbucket{}, rpm: rpm, lastSweep: time.Now()}
}

// allow consumes one token; refill rate = rpm/60 per second, burst = rpm.
func (l *limiter) allow(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	// cheap sweep to avoid unbounded map growth
	if now.Sub(l.lastSweep) > time.Minute && len(l.buckets) > 4096 {
		for k, b := range l.buckets {
			if now.Sub(b.last) > 10*time.Minute {
				delete(l.buckets, k)
			}
		}
		l.lastSweep = now
	}
	b, ok := l.buckets[ip]
	if !ok {
		b = &tbucket{tokens: float64(l.rpm), last: now}
		l.buckets[ip] = b
	}
	elapsed := now.Sub(b.last).Seconds()
	b.last = now
	b.tokens += elapsed * float64(l.rpm) / 60
	if b.tokens > float64(l.rpm) {
		b.tokens = float64(l.rpm)
	}
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

func visitorIP(r *http.Request, trustProxy bool) string {
	if trustProxy {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			// first entry; a proxy we front validates this
			first := strings.TrimSpace(strings.Split(xff, ",")[0])
			if ip := net.ParseIP(first); ip != nil {
				return ip.String()
			}
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	return host
}

// withRate caps requests per visitor IP on the wrapped routes.
func (a *API) withRate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !a.rate.allow(visitorIP(r, a.trustProxy)) {
			w.Header().Set("Retry-After", "5")
			writeError(w, 429, "rate_limited", "too many requests")
			return
		}
		next.ServeHTTP(w, r)
	})
}

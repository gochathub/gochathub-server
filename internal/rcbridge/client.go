package rcbridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/gochathub/gochathub-server/internal/push"
)

// Rocket.Chat REST client surface for the bridge. All calls carry the linked
// user's PAT as X-Auth-Token / X-User-Id (docs/ROCKETCHAT-INTEGRATION.md §2).

// RateLimitError carries the Reset wait for the sync loop to honor.
type RateLimitError struct{ RetryAfter time.Duration }

func (e RateLimitError) Error() string {
	return "rocket.chat rate limited, reset in " + e.RetryAfter.String()
}

// AuthError marks a dead PAT (regenerated/revoked user-side): the link keeps
// existing but every call fails until the user re-links.
type AuthError struct{ Msg string }

func (e AuthError) Error() string { return "rocket.chat auth rejected: " + e.Msg }

// rcDate is Rocket.Chat's timestamp encoding: REST returns RFC3339 strings,
// some payloads use {"$date": epochMillis} or bare epoch millis.
type rcDate struct{ time.Time }

func (d *rcDate) UnmarshalJSON(b []byte) error {
	s := strings.Trim(string(b), `"`)
	if s == "null" || s == "" {
		return nil
	}
	// {"$date": 1717200000000} shape
	if strings.HasPrefix(s, "{") {
		var v struct {
			Date int64 `json:"$date"`
		}
		if json.Unmarshal(b, &v) == nil && v.Date != 0 {
			d.Time = time.UnixMilli(v.Date).UTC()
			return nil
		}
		return fmt.Errorf("unsupported date object %s", s)
	}
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		if n > 1e12 { // epoch millis
			d.Time = time.UnixMilli(n).UTC()
		} else {
			d.Time = time.Unix(n, 0).UTC()
		}
		return nil
	}
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02T15:04:05.000Z", "2006-01-02T15:04:05Z07:00"} {
		if t, err := time.Parse(layout, s); err == nil {
			d.Time = t
			return nil
		}
	}
	return fmt.Errorf("unsupported date %q", s)
}

type rcUser struct {
	ID       string `json:"_id"`
	Username string `json:"username"`
	Name     string `json:"name"`
}

// rcSub mirrors one subscription entry: the user's pointer into a room.
type rcSub struct {
	RID       string `json:"rid"`
	T         string `json:"t"` // c channel, p private group, d direct, l livechat
	Name      string `json:"name"`
	Open      bool   `json:"open"`
	Unread    int    `json:"unread"`
	TS        rcDate `json:"ts"` // room creation time
	UpdatedAt rcDate `json:"_updatedAt"`
}

type rcMsg struct {
	ID       string  `json:"_id"`
	RID      string  `json:"rid"`
	Msg      string  `json:"msg"`
	TS       rcDate  `json:"ts"`
	U        rcUser  `json:"u"`
	T        string  `json:"t"`    // non-empty = system message
	TMID     string  `json:"tmid"` // thread root
	EditedAt *rcDate `json:"editedAt"`
}

type rcSubsResp struct {
	Subscriptions []rcSub `json:"subscriptions"`
	Update        []rcSub `json:"update"`
	Remove        []rcSub `json:"remove"`
	Success       bool    `json:"success"`
	Error         string  `json:"error"`
	ErrorType     string  `json:"errorType"`
}

type rcHistResp struct {
	Messages []rcMsg `json:"messages"`
	Success  bool    `json:"success"`
	Error    string  `json:"error"`
}

type rcUsersInfoResp struct {
	User    rcUser `json:"user"`
	Success bool   `json:"success"`
	Error   string `json:"error"`
}

type rcPostResp struct {
	Message rcMsg  `json:"message"`
	Success bool   `json:"success"`
	Error   string `json:"error"`
}

// target is one user's workspace + credentials; every request is addressed to
// it (the server URL is per user, ADR-024).
type target struct{ base, pat, uid string }

// guard rejects a workspace URL that resolves to a private address unless the
// host is allow-listed (same SSRF rule as push endpoints). Checked per request,
// after resolution; ponytail: a DNS rebind between check and dial is the known
// ceiling, a dialer Control hook is the upgrade.
func (b *Bridge) guard(ctx context.Context, t target) error {
	if err := push.CheckHost(ctx, t.base, b.AllowHosts); err != nil {
		return GuardError{err}
	}
	return nil
}

// get issues one authenticated API v1 call and classifies errors.
func (b *Bridge) get(ctx context.Context, t target, path string, q url.Values) ([]byte, error) {
	if err := b.guard(ctx, t); err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, t.base+"/api/v1/"+path, nil)
	if err != nil {
		return nil, fmt.Errorf("rc url: %w", err)
	}
	req.URL.RawQuery = q.Encode()
	req.Header.Set("X-Auth-Token", t.pat)
	req.Header.Set("X-User-Id", t.uid)
	resp, err := b.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("rc get %s: %w", path, err)
	}
	defer func() { _, _ = io.Copy(io.Discard, resp.Body); _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, fmt.Errorf("rc read %s: %w", path, err)
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		r, _ := strconv.ParseFloat(resp.Header.Get("X-RateLimit-Reset"), 64)
		var wait time.Duration
		if r > 0 {
			wait = max(0, time.Until(time.UnixMilli(int64(r))))
		} else {
			wait = 5 * time.Second
		}
		return nil, RateLimitError{wait}
	}
	if resp.StatusCode == http.StatusUnauthorized {
		return nil, AuthError{Msg: "HTTP 401"}
	}
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("rc get %s: HTTP %d", path, resp.StatusCode)
	}
	return body, nil
}

// listSubs returns (changes, removals) since `since` (nil = full first pull).
// Endpoint names: RC renamed subscriptions.get/list roles across versions —
// first success is remembered.
func (b *Bridge) listSubs(ctx context.Context, t target, since *time.Time) ([]rcSub, []rcSub, error) {
	q := url.Values{}
	if since != nil {
		q.Set("updatedSince", since.UTC().Format(time.RFC3339Nano))
	}
	for _, candidate := range b.learned("subs", t.base, "subscriptions.get", "subscriptions.list") {
		body, err := b.get(ctx, t, candidate, q)
		if err != nil {
			// 404-style route misses: try the next name. Auth/rate/cancel/guard abort.
			var rl RateLimitError
			if errors.As(err, &rl) || isAuthErr(err) || errors.Is(err, context.Canceled) || isGuardErr(err) {
				return nil, nil, err
			}
			continue
		}
		var r rcSubsResp
		if json.Unmarshal(body, &r) != nil || !r.Success {
			continue
		}
		b.remember("subs", t.base, candidate)
		if since == nil {
			return r.Subscriptions, nil, nil
		}
		return r.Update, r.Remove, nil
	}
	return nil, nil, errors.New("rc: no working subscriptions endpoint")
}

// remember/learned keep the endpoint name that worked per (kind, workspace):
// Rocket.Chat versions differ per server, so the cache is keyed by base URL.
func (b *Bridge) remember(kind, base, path string) {
	b.mu.Lock()
	b.paths[kind+"|"+base] = path
	b.mu.Unlock()
}

func (b *Bridge) learned(kind, base string, candidates ...string) []string {
	b.mu.Lock()
	known := b.paths[kind+"|"+base]
	b.mu.Unlock()
	if known != "" {
		return []string{known}
	}
	return candidates
}

// history returns messages in (since, now] newest-first, up to maxHist, with
// pagination across full pages. Names follow the im.*→dm.* rename; the first
// working one is remembered. groups.history is stable across versions.
func (b *Bridge) history(ctx context.Context, t target, rid, typ string, oldest *time.Time) ([]rcMsg, error) {
	candidates := []string{"groups.history"}
	if typ == "direct" {
		candidates = b.learned("dm", t.base, "dm.history", "im.history")
	}
	latest := time.Now().UTC()
	var out []rcMsg
	for range 5 { // maxHist/100 pages; hard ceiling, oldest page may be partial
		q := url.Values{
			"roomId":             []string{rid},
			"count":              []string{"100"},
			"sort":               []string{`{"ts":-1}`},
			"showThreadMessages": []string{"true"},
			"latest":             []string{latest.Format(time.RFC3339Nano)},
			"inclusive":          []string{"true"},
		}
		if oldest != nil {
			q.Set("oldest", oldest.UTC().Format(time.RFC3339Nano))
		}
		lastErr := error(nil)
		var body []byte
		used := ""
		for _, candidate := range candidates {
			var err error
			body, err = b.get(ctx, t, candidate, q)
			if err == nil {
				lastErr, used = nil, candidate
				break
			}
			var rl RateLimitError
			if errors.As(err, &rl) || isAuthErr(err) || isGuardErr(err) {
				return nil, err
			}
			lastErr = err
		}
		if lastErr != nil {
			return nil, lastErr
		}
		var h rcHistResp
		if err := json.Unmarshal(body, &h); err != nil || !h.Success {
			return nil, fmt.Errorf("rc history %s: bad payload", rid)
		}
		if typ == "direct" {
			b.remember("dm", t.base, used)
		}
		out = append(out, h.Messages...)
		if len(h.Messages) < 100 || len(out) >= maxHist {
			return out, nil
		}
		// next page starts older than the oldest message just fetched
		oldestThis := h.Messages[len(h.Messages)-1].TS.Time
		if !oldestThis.Before(latest) || (oldest != nil && !oldestThis.After(*oldest)) {
			return out, nil
		}
		latest = oldestThis
	}
	return out, nil
}

// maxHist caps a single pull window: backfill or delta burst.
const maxHist = 500

// postMessage sends text as the PAT owner.
func (b *Bridge) postMessage(ctx context.Context, t target, rid, text string) (rcMsg, error) {
	payload := map[string]any{"roomId": rid, "text": text}
	return b.callPost(ctx, t, "chat.postMessage", payload)
}

func (b *Bridge) callPost(ctx context.Context, t target, path string, payload map[string]any) (rcMsg, error) {
	if err := b.guard(ctx, t); err != nil {
		return rcMsg{}, err
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return rcMsg{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.base+"/api/v1/"+path, strings.NewReader(string(raw)))
	if err != nil {
		return rcMsg{}, err
	}
	req.Header.Set("X-Auth-Token", t.pat)
	req.Header.Set("X-User-Id", t.uid)
	req.Header.Set("Content-Type", "application/json")
	resp, err := b.HTTP.Do(req)
	if err != nil {
		return rcMsg{}, fmt.Errorf("rc post %s: %w", path, err)
	}
	defer func() { _, _ = io.Copy(io.Discard, resp.Body); _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return rcMsg{}, err
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		r, _ := strconv.ParseFloat(resp.Header.Get("X-RateLimit-Reset"), 64)
		wait := 5 * time.Second
		if r > 0 {
			wait = max(0, time.Until(time.UnixMilli(int64(r))))
		}
		return rcMsg{}, RateLimitError{wait}
	}
	if resp.StatusCode == http.StatusUnauthorized {
		return rcMsg{}, AuthError{Msg: "HTTP 401"}
	}
	var p rcPostResp
	if json.Unmarshal(body, &p) != nil {
		return rcMsg{}, fmt.Errorf("rc post %s: bad payload (HTTP %d)", path, resp.StatusCode)
	}
	if !p.Success {
		return rcMsg{}, fmt.Errorf("rc post %s: %s", path, p.Error)
	}
	return p.Message, nil
}

// whoami validates credentials: users.info by userId, falling back to
// username (the user may paste either; users.info supports both params).
func (b *Bridge) whoami(ctx context.Context, t target) (rcUser, error) {
	q := url.Values{"userId": []string{t.uid}}
	body, err := b.get(ctx, t, "users.info", q)
	if err == nil {
		var r rcUsersInfoResp
		if err := json.Unmarshal(body, &r); err == nil && r.Success && r.User.ID != "" {
			return r.User, nil
		}
	}
	if isGuardErr(err) {
		return rcUser{}, err
	}
	// fall back to treating the pasted string as username
	q = url.Values{"username": []string{t.uid}}
	body, err = b.get(ctx, t, "users.info", q)
	if err != nil {
		var rl RateLimitError
		var ae AuthError
		if errors.As(err, &rl) || errors.As(err, &ae) || errors.Is(err, context.Canceled) || isGuardErr(err) {
			return rcUser{}, err
		}
		return rcUser{}, fmt.Errorf("rocket.chat unreachable, link failed (%s)", err)
	}
	var r rcUsersInfoResp
	if err := json.Unmarshal(body, &r); err != nil || !r.Success || r.User.ID == "" {
		return rcUser{}, fmt.Errorf("rocket.chat rejected the credentials")
	}
	return r.User, nil
}

func isAuthErr(err error) bool {
	var ae AuthError
	return errors.As(err, &ae)
}

// GuardError: the workspace URL failed the SSRF check (private address,
// unresolvable host, bad scheme).
type GuardError struct{ Err error }

func (e GuardError) Error() string { return "workspace url rejected: " + e.Err.Error() }
func (e GuardError) Unwrap() error { return e.Err }

func isGuardErr(err error) bool {
	var ge GuardError
	return errors.As(err, &ge)
}

package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const turnstileURL = "https://challenges.cloudflare.com/turnstile/v0/siteverify"

// ErrCaptcha is returned when the Turnstile token is missing or rejected.
var ErrCaptcha = errors.New("captcha verification failed")

// Turnstile validates Cloudflare Turnstile tokens against siteverify.
// Tokens are single-use, so every login attempt needs a fresh one.
type Turnstile struct {
	Secret string
	// Hostname, when set, must match the hostname Cloudflare reports for the
	// widget (pins tokens to this deployment).
	Hostname string
	URL      string // siteverify endpoint; empty = Cloudflare's
}

// Verify fails closed: any network, decode or non-success outcome rejects.
func (t *Turnstile) Verify(ctx context.Context, token string, ip *string) error {
	if token == "" {
		return ErrCaptcha
	}
	endpoint := t.URL
	if endpoint == "" {
		endpoint = turnstileURL
	}
	form := url.Values{"secret": {t.Secret}, "response": {token}}
	if ip != nil {
		form.Set("remoteip", *ip)
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("turnstile siteverify: %w", err)
	}
	defer res.Body.Close()
	var out struct {
		Success  bool   `json:"success"`
		Hostname string `json:"hostname"`
		Action   string `json:"action"`
	}
	if res.StatusCode != http.StatusOK || json.NewDecoder(res.Body).Decode(&out) != nil {
		return fmt.Errorf("turnstile siteverify: status %d", res.StatusCode)
	}
	// ponytail: action is only checked when the widget set one; the UI sets "login"
	if !out.Success || (out.Action != "" && out.Action != "login") ||
		(t.Hostname != "" && out.Hostname != t.Hostname) {
		return ErrCaptcha
	}
	return nil
}

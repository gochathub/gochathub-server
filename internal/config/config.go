// Package config loads all runtime configuration from environment variables.
package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config is the full runtime configuration of the server binary.
type Config struct {
	ListenAddr  string
	DatabaseURL string
	SessionTTL  time.Duration
	LogLevel    string

	// CookieSecure forces the __Host- Secure cookie; set false only for
	// local development over plain HTTP.
	CookieSecure bool

	// Object storage for attachments.
	S3Endpoint   string
	S3Region     string
	S3Bucket     string
	S3AccessKey  string
	S3SecretKey  string
	S3UseTLS     bool
	MaxUpload    int64
	AllowUploads bool

	// VAPID keys for Web Push (RFC 8292). If empty the server generates a
	// key pair on first boot and persists it in the database (app_config).
	VAPIDPrivateKey string
	VAPIDPublicKey  string
	// VAPIDSubscriber is the VAPID sub claim (mailto: or https: URI).
	VAPIDSubscriber string
	// NtfyQueryFlag appends e.g. "up" as ?up=1 on push sends (ntfy
	// UnifiedPush flag, docs.ntfy.sh publish). Empty disables it.
	NtfyQueryFlag string

	// PushAllowHosts exempts named hosts from the private-address SSRF
	// rejection when sending push messages (self-hosted ntfy, ADR:
	// docs/UNIFIEDPUSH.md §3.4).
	PushAllowHosts []string

	// RateLimitRPM is the per-visitor token refill for capped endpoints.
	RateLimitRPM int

	// TrustProxy treats X-Forwarded-For as the visitor IP (reverse proxy in
	// front — this deployment is always proxied per docs/ARCHITECTURE.md).
	TrustProxy bool

	// BaseOrigin is the deployment Origin for same-origin enforcement
	// (ADR-015/016), e.g. https://chat.example.com. Empty disables checks.
	BaseOrigin string
}

func Load() (*Config, error) {
	c := &Config{
		ListenAddr:      ":8080",
		SessionTTL:      30 * 24 * time.Hour,
		LogLevel:        "info",
		CookieSecure:    true,
		S3Region:        "us-east-1",
		S3UseTLS:        true,
		MaxUpload:       25 << 20,
		AllowUploads:    true,
		RateLimitRPM:    60,
		VAPIDSubscriber: "https://chatserver.invalid",
		NtfyQueryFlag:   "up",
	}
	var err error
	c.DatabaseURL, err = required("DATABASE_URL")
	if err != nil {
		return nil, err
	}
	str(&c.ListenAddr, "LISTEN_ADDR")
	str(&c.LogLevel, "LOG_LEVEL")
	str(&c.BaseOrigin, "ORIGIN")
	dur := 30 * 24 * time.Hour
	durVar(&dur, "SESSION_TTL")
	c.SessionTTL = dur
	boolVar(&c.CookieSecure, "COOKIE_SECURE", true)
	str(&c.S3Endpoint, "S3_ENDPOINT")
	str(&c.S3Region, "S3_REGION")
	str(&c.S3Bucket, "S3_BUCKET")
	str(&c.S3AccessKey, "S3_ACCESS_KEY")
	str(&c.S3SecretKey, "S3_SECRET_KEY")
	boolVar(&c.S3UseTLS, "S3_USE_TLS", true)
	sizeVar(&c.MaxUpload, "MAX_UPLOAD_BYTES")
	boolVar(&c.AllowUploads, "ALLOW_UPLOADS", true)
	str(&c.VAPIDPrivateKey, "VAPID_PRIVATE_KEY")
	str(&c.VAPIDPublicKey, "VAPID_PUBLIC_KEY")
	str(&c.VAPIDSubscriber, "VAPID_SUBSCRIBER")
	str(&c.NtfyQueryFlag, "PUSH_NTFY_QUERY")
	c.PushAllowHosts = splitList("PUSH_ALLOW_HOSTS")
	intVar(&c.RateLimitRPM, "RATE_LIMIT_RPM")
	boolVar(&c.TrustProxy, "TRUST_PROXY", true)
	if (c.S3Endpoint == "") != (c.S3Bucket == "") || (c.S3AccessKey == "") != (c.S3SecretKey == "") {
		return nil, errors.New("attachment storage requires S3_ENDPOINT, S3_BUCKET, S3_ACCESS_KEY and S3_SECRET_KEY together")
	}
	return c, nil
}

func required(name string) (string, error) {
	v := os.Getenv(name)
	if v == "" {
		return "", fmt.Errorf("missing required environment variable %s", name)
	}
	return v, nil
}

func str(dst *string, name string) {
	if v := os.Getenv(name); v != "" {
		*dst = v
	}
}

func boolVar(dst *bool, name string, def bool) {
	if v := os.Getenv(name); v != "" {
		def, _ = strconv.ParseBool(v)
	}
	*dst = def
}

func durVar(dst *time.Duration, name string) {
	if v := os.Getenv(name); v != "" {
		d, err := time.ParseDuration(v)
		if err == nil {
			*dst = d
		}
	}
}

func sizeVar(dst *int64, name string) {
	if v := os.Getenv(name); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			*dst = n
		}
	}
}

func intVar(dst *int, name string) {
	if v := os.Getenv(name); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			*dst = n
		}
	}
}

func splitList(name string) []string {
	v := os.Getenv(name)
	if v == "" {
		return nil
	}
	var out []string
	for _, s := range strings.Split(v, ",") {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

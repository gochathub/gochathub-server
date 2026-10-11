package service

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/mail"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/gochathub/gochathub-server/internal/push"
)

const mailConfigKey = "mail.smtp"

// SMTPConfig is the decrypted outbound mail configuration.
type SMTPConfig struct{ Host, Port, User, Pass, TLS, From string }

// MailSettings stores the SMTP configuration in app_config (password sealed
// with SECRETS_KEY) and is itself the Mailer signup invites send through, so
// admin edits apply without a restart. Not configured = Enabled() false.
type MailSettings struct {
	App        *App
	Key        *[32]byte // nil: SECRETS_KEY unset, the feature stays off
	AllowHosts []string  // exempt from the private-address rejection (MAIL_ALLOW_HOSTS)
	// NewSender builds the transport; wired in app.Assemble (service must not import internal/mail).
	NewSender func(SMTPConfig) Mailer
}

type storedSMTP struct {
	Host    string `json:"host"`
	Port    string `json:"port"`
	User    string `json:"user"`
	PassEnc string `json:"pass_enc"` // base64(nonce|ciphertext)
	TLS     string `json:"tls"`
	From    string `json:"from"`
}

func (m *MailSettings) seal(plain string) (string, error) {
	aead, err := m.aead()
	if err != nil {
		return "", err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(aead.Seal(nonce, nonce, []byte(plain), nil)), nil
}

func (m *MailSettings) open(enc string) (string, error) {
	if enc == "" {
		return "", nil
	}
	raw, err := base64.StdEncoding.DecodeString(enc)
	if err != nil {
		return "", err
	}
	aead, err := m.aead()
	if err != nil {
		return "", err
	}
	if len(raw) < aead.NonceSize() {
		return "", errors.New("smtp password: ciphertext too short")
	}
	plain, err := aead.Open(nil, raw[:aead.NonceSize()], raw[aead.NonceSize():], nil)
	return string(plain), err
}

func (m *MailSettings) aead() (cipher.AEAD, error) {
	if m.Key == nil {
		return nil, errors.New("SECRETS_KEY is not set")
	}
	b, err := aes.NewCipher(m.Key[:])
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(b)
}

// NewSecretsKey derives the 32-byte key from the SECRETS_KEY env value.
func NewSecretsKey(secret string) *[32]byte {
	if secret == "" {
		return nil
	}
	k := sha256.Sum256([]byte(secret))
	return &k
}

// load returns the stored config; ok=false when nothing is configured.
func (m *MailSettings) load(ctx context.Context) (stored storedSMTP, ok bool, err error) {
	raw, err := m.App.Store.GetConfigValue(ctx, mailConfigKey)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && raw == "") {
		return stored, false, nil
	}
	if err != nil {
		return stored, false, fmt.Errorf("load mail settings: %w", err)
	}
	if err := json.Unmarshal([]byte(raw), &stored); err != nil {
		return stored, false, fmt.Errorf("decode mail settings: %w", err)
	}
	return stored, stored.Host != "", nil
}

// Enabled: transport wired, key present and a host saved.
func (m *MailSettings) Enabled(ctx context.Context) bool {
	if m.Key == nil || m.NewSender == nil {
		return false
	}
	_, ok, err := m.load(ctx)
	return ok && err == nil
}

func (m *MailSettings) sender(ctx context.Context) (Mailer, error) {
	st, ok, err := m.load(ctx)
	if err != nil {
		return nil, err
	}
	if !ok || m.Key == nil || m.NewSender == nil {
		return nil, errors.New("mail is not configured")
	}
	pass, err := m.open(st.PassEnc)
	if err != nil {
		return nil, fmt.Errorf("open smtp password: %w", err)
	}
	// ponytail: checked per send (resolution can change); a DNS-rebinding race between check and dial remains, admin-only surface
	if err := push.CheckHost(ctx, "https://"+st.Host, m.AllowHosts); err != nil {
		return nil, fmt.Errorf("smtp host rejected: %w", err)
	}
	return m.NewSender(SMTPConfig{Host: st.Host, Port: st.Port, User: st.User, Pass: pass, TLS: st.TLS, From: st.From}), nil
}

// Send delivers through the currently saved configuration.
func (m *MailSettings) Send(ctx context.Context, msg Mail) error {
	s, err := m.sender(ctx)
	if err != nil {
		return err
	}
	return s.Send(ctx, msg)
}

// MailSettingsView never carries the password.
type MailSettingsView struct {
	Host                string `json:"host"`
	Port                string `json:"port"`
	User                string `json:"user"`
	TLS                 string `json:"tls"`
	From                string `json:"from"`
	HasPassword         bool   `json:"has_password"`
	Configured          bool   `json:"configured"`
	EncryptionAvailable bool   `json:"encryption_available"`
}

func (m *MailSettings) Get(ctx context.Context, p Principal) (MailSettingsView, error) {
	if !p.IsAdmin() {
		return MailSettingsView{}, ErrForbidden
	}
	st, ok, err := m.load(ctx)
	if err != nil {
		return MailSettingsView{}, err
	}
	v := MailSettingsView{EncryptionAvailable: m.Key != nil}
	if ok {
		v = MailSettingsView{Host: st.Host, Port: st.Port, User: st.User, TLS: st.TLS, From: st.From,
			HasPassword: st.PassEnc != "", Configured: m.Enabled(ctx), EncryptionAvailable: v.EncryptionAvailable}
	}
	return v, nil
}

// MailSettingsInput: Password nil keeps the stored one; an empty Host clears
// the configuration (feature off).
type MailSettingsInput struct {
	Host, Port, User, TLS, From string
	Password                    *string
}

func (m *MailSettings) Save(ctx context.Context, p Principal, in MailSettingsInput) (MailSettingsView, error) {
	if !p.IsAdmin() {
		return MailSettingsView{}, ErrForbidden
	}
	if m.Key == nil {
		return MailSettingsView{}, bad("SECRETS_KEY is not set on the server; it encrypts the SMTP password")
	}
	in.Host = strings.TrimSpace(in.Host)
	if in.Host == "" { // clear
		if err := m.App.Store.SetConfigValue(ctx, mailConfigKey, ""); err != nil {
			return MailSettingsView{}, fmt.Errorf("clear mail settings: %w", err)
		}
		return MailSettingsView{EncryptionAvailable: true}, m.App.Store.Audit(ctx, p.UserID, "mail_settings.clear", "app_config", mailConfigKey, []byte(`{}`), nil)
	}
	if strings.ContainsAny(in.Host, " \t\r\n/@:") {
		return MailSettingsView{}, bad("host must be a bare hostname or IP")
	}
	if in.Port == "" {
		in.Port = "587"
	}
	if n, err := strconv.Atoi(in.Port); err != nil || n < 1 || n > 65535 {
		return MailSettingsView{}, bad("port must be 1-65535")
	}
	switch in.TLS {
	case "starttls", "implicit", "none":
	default:
		return MailSettingsView{}, bad("tls must be starttls, implicit or none")
	}
	in.From = strings.TrimSpace(in.From)
	if a, err := mail.ParseAddress(in.From); err != nil || strings.ContainsAny(in.From, "\r\n") || a.Address == "" {
		return MailSettingsView{}, bad("from must be a valid email address")
	}
	if strings.ContainsAny(in.User, "\r\n") {
		return MailSettingsView{}, bad("user contains a line break")
	}
	if in.User != "" && in.TLS == "none" {
		return MailSettingsView{}, bad("SMTP authentication over plaintext is refused; choose starttls or implicit")
	}
	st := storedSMTP{Host: in.Host, Port: in.Port, User: in.User, TLS: in.TLS, From: in.From}
	if in.User != "" { // password only meaningful with a user
		if in.Password != nil {
			enc, err := m.seal(*in.Password)
			if err != nil {
				return MailSettingsView{}, err
			}
			st.PassEnc = enc
		} else if old, _, err := m.load(ctx); err != nil {
			return MailSettingsView{}, err
		} else {
			st.PassEnc = old.PassEnc
		}
	}
	raw, err := json.Marshal(st)
	if err != nil {
		return MailSettingsView{}, err
	}
	if err := m.App.Store.SetConfigValue(ctx, mailConfigKey, string(raw)); err != nil {
		return MailSettingsView{}, fmt.Errorf("save mail settings: %w", err)
	}
	if err := m.App.Store.Audit(ctx, p.UserID, "mail_settings.update", "app_config", mailConfigKey, []byte(`{}`), nil); err != nil {
		return MailSettingsView{}, err
	}
	return m.Get(ctx, p)
}

// Test sends one message through the saved configuration so an admin can
// verify it; the SMTP error is returned to the (admin) caller.
func (m *MailSettings) Test(ctx context.Context, p Principal, to string) error {
	if !p.IsAdmin() {
		return ErrForbidden
	}
	to = strings.ToLower(strings.TrimSpace(to))
	if len(to) > 254 || strings.ContainsAny(to, " \r\n<>,;") || strings.Index(to, "@") < 1 {
		return bad("to must be a valid address")
	}
	if !m.Enabled(ctx) {
		return bad("mail is not configured (save settings first; SECRETS_KEY must be set)")
	}
	if err := m.Send(ctx, Mail{To: to, Subject: "goChatHub mail test", Body: "This is a test message from your goChatHub server. Mail delivery works.\n"}); err != nil {
		return bad("test send failed: %v", err)
	}
	return m.App.Store.Audit(ctx, p.UserID, "mail_settings.test", "app_config", mailConfigKey, []byte(`{}`), nil)
}

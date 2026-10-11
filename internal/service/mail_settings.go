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
	"regexp"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/gochathub/gochathub-server/internal/push"
)

const (
	mailConfigKey       = "mail.settings"
	legacyMailConfigKey = "mail.smtp" // single-provider SMTP config (first ADR-026 cut)
)

// MailField describes one input of a provider form. The webui renders the form
// from this, so provider knowledge lives only here and in internal/mail.
type MailField struct {
	Name     string   `json:"name"`
	Label    string   `json:"label"`
	Help     string   `json:"help,omitempty"`
	Secret   bool     `json:"secret"`
	Required bool     `json:"required"`
	Default  string   `json:"default,omitempty"`
	Options  []string `json:"options,omitempty"` // non-empty = a select
}

// MailProvider is one entry of the registry.
type MailProvider struct {
	ID     string      `json:"id"`
	Label  string      `json:"label"`
	Fields []MailField `json:"fields"`
}

var fromField = MailField{Name: "from", Label: "From address", Help: "e.g. goChatHub <chat@example.com>; must be a verified sender at the provider", Required: true}

// MailProviders is the provider registry (order = UI order).
var MailProviders = []MailProvider{
	{ID: "postmark", Label: "Postmark", Fields: []MailField{
		{Name: "server_token", Label: "Server API token", Secret: true, Required: true},
		{Name: "message_stream", Label: "Message stream", Help: "Optional. Leave blank for the default transactional stream (outbound). Use a transactional stream, not a broadcast one"},
		fromField,
	}},
	{ID: "ses", Label: "Amazon SES", Fields: []MailField{
		{Name: "region", Label: "AWS region", Required: true, Default: "us-east-1", Help: "e.g. us-east-1"},
		{Name: "access_key_id", Label: "Access key ID", Required: true, Help: "Use a dedicated IAM user limited to ses:SendEmail"},
		{Name: "secret_access_key", Label: "Secret access key", Secret: true, Required: true},
		fromField,
	}},
	{ID: "mailgun", Label: "Mailgun", Fields: []MailField{
		{Name: "api_key", Label: "API key", Secret: true, Required: true, Help: "A domain sending key is enough"},
		{Name: "domain", Label: "Sending domain", Required: true, Help: "e.g. mg.example.com"},
		{Name: "region", Label: "Region", Required: true, Default: "us", Options: []string{"us", "eu"}, Help: "Fixed when the domain was created"},
		fromField,
	}},
	{ID: "sendgrid", Label: "SendGrid", Fields: []MailField{
		{Name: "api_key", Label: "API key", Secret: true, Required: true, Help: "Needs Mail Send permission"},
		fromField,
	}},
	{ID: "resend", Label: "Resend", Fields: []MailField{
		{Name: "api_key", Label: "API key", Secret: true, Required: true},
		fromField,
	}},
	{ID: "brevo", Label: "Brevo", Fields: []MailField{
		{Name: "api_key", Label: "API key", Secret: true, Required: true},
		fromField,
	}},
	{ID: "smtp", Label: "SMTP server", Fields: []MailField{
		{Name: "host", Label: "Host", Required: true, Help: "e.g. smtp.example.com"},
		{Name: "port", Label: "Port", Required: true, Default: "587"},
		{Name: "tls", Label: "Encryption", Required: true, Default: "starttls", Options: []string{"starttls", "implicit", "none"}, Help: "none refuses login"},
		{Name: "user", Label: "Username", Help: "Optional"},
		{Name: "password", Label: "Password", Secret: true},
		fromField,
	}},
}

func providerByID(id string) (MailProvider, bool) {
	for _, p := range MailProviders {
		if p.ID == id {
			return p, true
		}
	}
	return MailProvider{}, false
}

// MailSettings keeps one saved configuration per provider plus the active
// one, in app_config. Secret fields are sealed with SECRETS_KEY; it is itself
// the Mailer signup invites send through, so admin edits apply immediately.
type MailSettings struct {
	App        *App
	Key        *[32]byte // nil: SECRETS_KEY unset, the feature stays off
	AllowHosts []string  // SMTP hosts exempt from the private-address rejection (MAIL_ALLOW_HOSTS)
	// NewSender builds the transport for a provider; wired in app.Assemble
	// (service must not import internal/mail).
	NewSender func(provider string, values map[string]string) (Mailer, error)
}

// stored is the app_config value: secret fields hold base64(nonce|ciphertext).
type storedMail struct {
	Active    string                       `json:"active"`
	Providers map[string]map[string]string `json:"providers"`
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
	raw, err := base64.StdEncoding.DecodeString(enc)
	if err != nil {
		return "", err
	}
	aead, err := m.aead()
	if err != nil {
		return "", err
	}
	if len(raw) < aead.NonceSize() {
		return "", errors.New("mail secret: ciphertext too short")
	}
	plain, err := aead.Open(nil, raw[:aead.NonceSize()], raw[aead.NonceSize():], nil)
	return string(plain), err
}

// NewSecretsKey derives the 32-byte key from the SECRETS_KEY env value.
func NewSecretsKey(secret string) *[32]byte {
	if secret == "" {
		return nil
	}
	k := sha256.Sum256([]byte(secret))
	return &k
}

// load reads the stored settings, upgrading the single-provider SMTP value
// of the first ADR-026 cut in memory (the next save writes the new key).
func (m *MailSettings) load(ctx context.Context) (storedMail, error) {
	st := storedMail{Providers: map[string]map[string]string{}}
	raw, err := m.App.Store.GetConfigValue(ctx, mailConfigKey)
	switch {
	case err == nil && raw != "":
		if err := json.Unmarshal([]byte(raw), &st); err != nil {
			return st, fmt.Errorf("decode mail settings: %w", err)
		}
		if st.Providers == nil {
			st.Providers = map[string]map[string]string{}
		}
		return st, nil
	case err != nil && !errors.Is(err, pgx.ErrNoRows):
		return st, fmt.Errorf("load mail settings: %w", err)
	}
	old, err := m.App.Store.GetConfigValue(ctx, legacyMailConfigKey)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && old == "") {
		return st, nil
	}
	if err != nil {
		return st, fmt.Errorf("load legacy mail settings: %w", err)
	}
	var l struct {
		Host, Port, User, TLS, From string
		PassEnc                     string `json:"pass_enc"`
	}
	if err := json.Unmarshal([]byte(old), &l); err != nil {
		return st, fmt.Errorf("decode legacy mail settings: %w", err)
	}
	if l.Host != "" {
		st.Providers["smtp"] = map[string]string{"host": l.Host, "port": l.Port, "tls": l.TLS, "user": l.User, "password": l.PassEnc, "from": l.From}
		st.Active = "smtp"
	}
	return st, nil
}

func (m *MailSettings) store(ctx context.Context, st storedMail) error {
	raw, err := json.Marshal(st)
	if err != nil {
		return err
	}
	return m.App.Store.SetConfigValue(ctx, mailConfigKey, string(raw))
}

// complete reports whether every required field of the provider is stored.
func complete(p MailProvider, vals map[string]string) bool {
	for _, f := range p.Fields {
		if f.Required && vals[f.Name] == "" {
			return false
		}
	}
	return len(vals) > 0
}

// Enabled: transport wired, key present and an active provider fully configured.
func (m *MailSettings) Enabled(ctx context.Context) bool {
	if m.Key == nil || m.NewSender == nil {
		return false
	}
	st, err := m.load(ctx)
	if err != nil || st.Active == "" {
		return false
	}
	p, ok := providerByID(st.Active)
	return ok && complete(p, st.Providers[st.Active])
}

// values returns the decrypted field values of a provider.
func (m *MailSettings) values(p MailProvider, stored map[string]string) (map[string]string, error) {
	out := make(map[string]string, len(p.Fields))
	for _, f := range p.Fields {
		v := stored[f.Name]
		if f.Secret && v != "" {
			var err error
			if v, err = m.open(v); err != nil {
				return nil, fmt.Errorf("open %s: %w", f.Name, err)
			}
		}
		out[f.Name] = v
	}
	return out, nil
}

func (m *MailSettings) sender(ctx context.Context) (Mailer, error) {
	st, err := m.load(ctx)
	if err != nil {
		return nil, err
	}
	p, ok := providerByID(st.Active)
	if !ok || m.Key == nil || m.NewSender == nil || !complete(p, st.Providers[st.Active]) {
		return nil, errors.New("mail is not configured")
	}
	vals, err := m.values(p, st.Providers[st.Active])
	if err != nil {
		return nil, err
	}
	if p.ID == "smtp" {
		// ponytail: checked per send (resolution can change); a DNS-rebinding race between check and dial remains, admin-only surface
		if err := push.CheckHost(ctx, "https://"+vals["host"], m.AllowHosts); err != nil {
			return nil, fmt.Errorf("smtp host rejected: %w", err)
		}
	}
	return m.NewSender(p.ID, vals)
}

// Send delivers through the active provider.
func (m *MailSettings) Send(ctx context.Context, msg Mail) error {
	s, err := m.sender(ctx)
	if err != nil {
		return err
	}
	return s.Send(ctx, msg)
}

// MailProviderView is one provider's form plus its saved state; secret values
// are never included, only has_secret flags.
type MailProviderView struct {
	MailProvider
	Values     map[string]string `json:"values"` // non-secret fields (defaults filled in)
	HasSecret  map[string]bool   `json:"has_secret"`
	Configured bool              `json:"configured"`
}

type MailSettingsView struct {
	Active              string             `json:"active"` // "" = mail off
	Providers           []MailProviderView `json:"providers"`
	EncryptionAvailable bool               `json:"encryption_available"`
}

func (m *MailSettings) Get(ctx context.Context, p Principal) (MailSettingsView, error) {
	if !p.IsAdmin() {
		return MailSettingsView{}, ErrForbidden
	}
	st, err := m.load(ctx)
	if err != nil {
		return MailSettingsView{}, err
	}
	v := MailSettingsView{Active: st.Active, EncryptionAvailable: m.Key != nil, Providers: make([]MailProviderView, 0, len(MailProviders))}
	for _, def := range MailProviders {
		saved := st.Providers[def.ID]
		pv := MailProviderView{MailProvider: def, Values: map[string]string{}, HasSecret: map[string]bool{}, Configured: complete(def, saved)}
		for _, f := range def.Fields {
			switch {
			case f.Secret:
				pv.HasSecret[f.Name] = saved[f.Name] != ""
			case saved[f.Name] != "":
				pv.Values[f.Name] = saved[f.Name]
			default:
				pv.Values[f.Name] = f.Default
			}
		}
		v.Providers = append(v.Providers, pv)
	}
	return v, nil
}

var (
	sesRegionRe   = regexp.MustCompile(`^[a-z]{2}(-[a-z]+)+-[0-9]$`)
	mailDomainRe  = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9.-]*[A-Za-z0-9])?$`)
	smtpHostBadRe = regexp.MustCompile(`[\s/@:]`)
)

// validate checks one provider's merged field values (secrets as stored/new).
func validate(def MailProvider, vals map[string]string) error {
	for _, f := range def.Fields {
		v := vals[f.Name]
		if len(v) > 512 || strings.ContainsAny(v, "\r\n") {
			return bad("%s: invalid value", f.Label)
		}
		if f.Required && v == "" {
			return bad("%s is required", f.Label)
		}
		if len(f.Options) > 0 && v != "" {
			ok := false
			for _, o := range f.Options {
				ok = ok || o == v
			}
			if !ok {
				return bad("%s must be one of %s", f.Label, strings.Join(f.Options, ", "))
			}
		}
	}
	if a, err := mail.ParseAddress(vals["from"]); err != nil || a.Address == "" {
		return bad("From address must be a valid email address")
	}
	switch def.ID {
	case "ses":
		if !sesRegionRe.MatchString(vals["region"]) {
			return bad("AWS region looks wrong (e.g. us-east-1)")
		}
	case "mailgun":
		if !mailDomainRe.MatchString(vals["domain"]) {
			return bad("Sending domain looks wrong")
		}
	case "smtp":
		if smtpHostBadRe.MatchString(vals["host"]) {
			return bad("Host must be a bare hostname or IP")
		}
		if n, err := strconv.Atoi(vals["port"]); err != nil || n < 1 || n > 65535 {
			return bad("Port must be 1-65535")
		}
		if vals["user"] != "" && vals["tls"] == "none" {
			return bad("SMTP login over plaintext is refused; choose starttls or implicit")
		}
	}
	return nil
}

// Save stores one provider's configuration. Secret fields left empty keep the
// stored value; activate makes it the active provider.
func (m *MailSettings) Save(ctx context.Context, p Principal, providerID string, in map[string]string, activate bool) (MailSettingsView, error) {
	if !p.IsAdmin() {
		return MailSettingsView{}, ErrForbidden
	}
	def, ok := providerByID(providerID)
	if !ok {
		return MailSettingsView{}, ErrNotFound
	}
	if m.Key == nil {
		return MailSettingsView{}, bad("SECRETS_KEY is not set on the server; it encrypts provider credentials")
	}
	st, err := m.load(ctx)
	if err != nil {
		return MailSettingsView{}, err
	}
	old := st.Providers[providerID]
	merged := make(map[string]string, len(def.Fields)) // plaintext for validation
	next := make(map[string]string, len(def.Fields))   // what gets stored
	for _, f := range def.Fields {
		v := strings.TrimSpace(in[f.Name])
		switch {
		case f.Secret && v == "": // keep
			next[f.Name] = old[f.Name]
			merged[f.Name] = old[f.Name]
		case f.Secret:
			enc, err := m.seal(v)
			if err != nil {
				return MailSettingsView{}, err
			}
			next[f.Name], merged[f.Name] = enc, v
		default:
			if v == "" {
				v = f.Default
			}
			next[f.Name], merged[f.Name] = v, v
		}
	}
	if err := validate(def, merged); err != nil {
		return MailSettingsView{}, err
	}
	st.Providers[providerID] = next
	if activate {
		st.Active = providerID
	}
	if err := m.store(ctx, st); err != nil {
		return MailSettingsView{}, fmt.Errorf("save mail settings: %w", err)
	}
	action := "mail_settings.update"
	if activate {
		action = "mail_settings.activate"
	}
	if err := m.App.Store.Audit(ctx, p.UserID, action, "mail_provider", providerID, []byte(`{}`), nil); err != nil {
		return MailSettingsView{}, err
	}
	return m.Get(ctx, p)
}

// SetActive switches the active provider; "" turns mail off. The provider
// must already be fully configured.
func (m *MailSettings) SetActive(ctx context.Context, p Principal, providerID string) (MailSettingsView, error) {
	if !p.IsAdmin() {
		return MailSettingsView{}, ErrForbidden
	}
	st, err := m.load(ctx)
	if err != nil {
		return MailSettingsView{}, err
	}
	if providerID != "" {
		def, ok := providerByID(providerID)
		if !ok {
			return MailSettingsView{}, ErrNotFound
		}
		if !complete(def, st.Providers[providerID]) {
			return MailSettingsView{}, bad("%s is not configured yet; save its settings first", def.Label)
		}
	}
	st.Active = providerID
	if err := m.store(ctx, st); err != nil {
		return MailSettingsView{}, fmt.Errorf("save mail settings: %w", err)
	}
	if err := m.App.Store.Audit(ctx, p.UserID, "mail_settings.activate", "mail_provider", providerID, []byte(`{}`), nil); err != nil {
		return MailSettingsView{}, err
	}
	return m.Get(ctx, p)
}

// Clear wipes one provider's stored credentials (and deactivates it).
func (m *MailSettings) Clear(ctx context.Context, p Principal, providerID string) (MailSettingsView, error) {
	if !p.IsAdmin() {
		return MailSettingsView{}, ErrForbidden
	}
	if _, ok := providerByID(providerID); !ok {
		return MailSettingsView{}, ErrNotFound
	}
	st, err := m.load(ctx)
	if err != nil {
		return MailSettingsView{}, err
	}
	delete(st.Providers, providerID)
	if st.Active == providerID {
		st.Active = ""
	}
	if err := m.store(ctx, st); err != nil {
		return MailSettingsView{}, fmt.Errorf("save mail settings: %w", err)
	}
	if err := m.App.Store.Audit(ctx, p.UserID, "mail_settings.clear", "mail_provider", providerID, []byte(`{}`), nil); err != nil {
		return MailSettingsView{}, err
	}
	return m.Get(ctx, p)
}

// Test sends one message through the active provider so an admin can verify
// it; the provider's error is returned to the (admin) caller.
func (m *MailSettings) Test(ctx context.Context, p Principal, to string) error {
	if !p.IsAdmin() {
		return ErrForbidden
	}
	to = strings.ToLower(strings.TrimSpace(to))
	if len(to) > 254 || strings.ContainsAny(to, " \r\n<>,;") || strings.Index(to, "@") < 1 {
		return bad("to must be a valid address")
	}
	if !m.Enabled(ctx) {
		return bad("no active mail provider (save and activate one first; SECRETS_KEY must be set)")
	}
	if err := m.Send(ctx, Mail{To: to, Subject: "goChatHub mail test", Body: "This is a test message from your goChatHub server. Mail delivery works.\n"}); err != nil {
		return bad("test send failed: %v", err)
	}
	return m.App.Store.Audit(ctx, p.UserID, "mail_settings.test", "app_config", mailConfigKey, []byte(`{}`), nil)
}

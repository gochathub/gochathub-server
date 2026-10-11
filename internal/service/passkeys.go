package service

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"
	"github.com/google/uuid"

	"github.com/gochathub/gochathub-server/internal/id"
	"github.com/gochathub/gochathub-server/internal/pwd"
	"github.com/gochathub/gochathub-server/internal/store"
)

const (
	maxPasskeys    = 10
	handoffTTL     = 60 * time.Second
	maxPasskeyName = 100
)

// ErrPasskeysDisabled: WA is nil (no ORIGIN configured).
var ErrPasskeysDisabled = errors.New("passkeys disabled")

// NewWebAuthn builds the relying party from the deployment origin; the RP ID
// is its host. Returns nil when origin is empty (feature off).
func NewWebAuthn(origin string) (*webauthn.WebAuthn, error) {
	if origin == "" {
		return nil, nil
	}
	host := strings.TrimPrefix(strings.TrimPrefix(origin, "https://"), "http://")
	host, _, _ = strings.Cut(host, ":")
	return webauthn.New(&webauthn.Config{RPID: host, RPDisplayName: "goChatHub", RPOrigins: []string{origin}})
}

// PasskeyView is the API shape; the credential itself never leaves the server.
type PasskeyView struct {
	ID             string
	Name           string
	CreatedAt      time.Time
	LastUsedAt     *time.Time
	BackupEligible bool
	Transports     []string
}

func viewOf(p store.PasskeyRow) PasskeyView {
	v := PasskeyView{ID: p.ID, Name: p.Name, CreatedAt: p.CreatedAt, LastUsedAt: p.LastUsedAt, Transports: []string{}}
	if c, err := decodeCred(p); err == nil {
		v.BackupEligible = c.Flags.BackupEligible
		for _, t := range c.Transport {
			v.Transports = append(v.Transports, string(t))
		}
	}
	return v
}

func decodeCred(p store.PasskeyRow) (webauthn.Credential, error) {
	var c webauthn.Credential
	return c, json.Unmarshal(p.Credential, &c)
}

// waUser adapts a user + passkeys to webauthn.User. The handle is the user's
// UUID bytes: stable and not PII.
type waUser struct {
	row   store.UserRow
	creds []webauthn.Credential
}

func (u waUser) WebAuthnID() []byte {
	b, _ := uuid.MustParse(u.row.ID).MarshalBinary()
	return b
}
func (u waUser) WebAuthnName() string                       { return u.row.Username }
func (u waUser) WebAuthnDisplayName() string                { return u.row.DisplayName }
func (u waUser) WebAuthnCredentials() []webauthn.Credential { return u.creds }

func (a *AuthService) loadWAUser(ctx context.Context, row store.UserRow) (waUser, error) {
	rows, err := a.App.Store.PasskeysByUser(ctx, row.ID)
	if err != nil {
		return waUser{}, err
	}
	u := waUser{row: row}
	for _, r := range rows {
		c, err := decodeCred(r)
		if err != nil {
			return waUser{}, fmt.Errorf("decode passkey %s: %w", r.ID, err)
		}
		u.creds = append(u.creds, c)
	}
	return u, nil
}

func (a *AuthService) storeCeremony(ctx context.Context, kind string, userID *string, sd *webauthn.SessionData) (string, error) {
	raw, err := json.Marshal(sd)
	if err != nil {
		return "", err
	}
	tok := id.NewToken()
	if err := a.App.Store.CreateCeremony(ctx, id.HashToken(tok), kind, userID, raw, time.Now().Add(challengeTTL)); err != nil {
		return "", fmt.Errorf("create ceremony: %w", err)
	}
	return tok, nil
}

func (a *AuthService) takeCeremony(ctx context.Context, tok, kind string) (*string, webauthn.SessionData, error) {
	var sd webauthn.SessionData
	uid, raw, err := a.App.Store.TakeCeremony(ctx, id.HashToken(tok), kind)
	if errors.Is(err, store.ErrNotFound) {
		return nil, sd, ErrUnauthorized
	}
	if err != nil {
		return nil, sd, fmt.Errorf("take ceremony: %w", err)
	}
	return uid, sd, json.Unmarshal(raw, &sd)
}

// BeginPasskeyRegistration re-authenticates (password, plus a TOTP/backup code
// when TOTP is on) and starts the ceremony. Sessions only; shadow accounts out.
func (a *AuthService) BeginPasskeyRegistration(ctx context.Context, p Principal, password, code string) (string, *protocol.CredentialCreation, error) {
	if a.WA == nil {
		return "", nil, ErrPasskeysDisabled
	}
	if p.Kind != "session" {
		return "", nil, ErrForbidden
	}
	row, err := a.App.Store.UserByID(ctx, p.UserID)
	if err != nil {
		return "", nil, ErrNotFound
	}
	if row.PasswordHash == "!" {
		return "", nil, ErrForbidden
	}
	if ok, err := pwd.Verify(row.PasswordHash, password); err != nil || !ok {
		return "", nil, ErrUnauthorized
	}
	if t, err := a.App.Store.TOTPByUser(ctx, p.UserID); err == nil && t.Enabled {
		if ok, err := a.checkSecondFactor(ctx, p.UserID, t, code); err != nil {
			return "", nil, err
		} else if !ok {
			return "", nil, ErrUnauthorized
		}
	} else if err != nil && !errors.Is(err, store.ErrNotFound) {
		return "", nil, err
	}
	u, err := a.loadWAUser(ctx, row)
	if err != nil {
		return "", nil, err
	}
	if len(u.creds) >= maxPasskeys {
		return "", nil, bad("passkey limit reached")
	}
	excl := make([]protocol.CredentialDescriptor, 0, len(u.creds))
	for _, c := range u.creds {
		excl = append(excl, c.Descriptor())
	}
	creation, sd, err := a.WA.BeginRegistration(u,
		webauthn.WithResidentKeyRequirement(protocol.ResidentKeyRequirementRequired),
		webauthn.WithAuthenticatorSelection(protocol.AuthenticatorSelection{
			ResidentKey:      protocol.ResidentKeyRequirementRequired,
			UserVerification: protocol.VerificationRequired,
		}),
		webauthn.WithExclusions(excl))
	if err != nil {
		return "", nil, fmt.Errorf("begin registration: %w", err)
	}
	tok, err := a.storeCeremony(ctx, "register", &row.ID, sd)
	return tok, creation, err
}

// FinishPasskeyRegistration verifies the attestation (credential is the raw
// RegistrationResponseJSON) and stores the passkey.
func (a *AuthService) FinishPasskeyRegistration(ctx context.Context, p Principal, ceremony, name string, credential []byte) (PasskeyView, error) {
	if a.WA == nil {
		return PasskeyView{}, ErrPasskeysDisabled
	}
	name = strings.TrimSpace(name)
	if name == "" {
		name = "passkey"
	}
	if len(name) > maxPasskeyName {
		return PasskeyView{}, bad("name too long")
	}
	uid, sd, err := a.takeCeremony(ctx, ceremony, "register")
	if err != nil {
		return PasskeyView{}, err
	}
	if uid == nil || *uid != p.UserID {
		return PasskeyView{}, ErrUnauthorized
	}
	row, err := a.App.Store.UserByID(ctx, p.UserID)
	if err != nil {
		return PasskeyView{}, ErrNotFound
	}
	u, err := a.loadWAUser(ctx, row)
	if err != nil {
		return PasskeyView{}, err
	}
	parsed, err := protocol.ParseCredentialCreationResponseBytes(credential)
	if err != nil {
		return PasskeyView{}, bad("invalid credential")
	}
	cred, err := a.WA.CreateCredential(u, sd, parsed)
	if err != nil {
		return PasskeyView{}, bad("passkey registration failed")
	}
	raw, err := json.Marshal(cred)
	if err != nil {
		return PasskeyView{}, err
	}
	pk := store.PasskeyRow{ID: id.NewID(), UserID: p.UserID, CredentialID: cred.ID, Credential: raw, Name: name}
	if err := a.App.Store.CreatePasskey(ctx, &pk); err != nil {
		return PasskeyView{}, fmt.Errorf("create passkey: %w", err)
	}
	_ = a.App.Store.Audit(ctx, p.UserID, "passkey.register", "passkey", pk.ID, []byte(`{}`), nil)
	return viewOf(pk), nil
}

// BeginPasskeyLogin starts a discoverable (usernameless) login.
func (a *AuthService) BeginPasskeyLogin(ctx context.Context) (string, *protocol.CredentialAssertion, error) {
	if a.WA == nil {
		return "", nil, ErrPasskeysDisabled
	}
	as, sd, err := a.WA.BeginDiscoverableLogin(webauthn.WithUserVerification(protocol.VerificationRequired))
	if err != nil {
		return "", nil, fmt.Errorf("begin login: %w", err)
	}
	tok, err := a.storeCeremony(ctx, "login", nil, sd)
	return tok, as, err
}

// FinishPasskeyLogin verifies the assertion (credential is the raw
// AuthenticationResponseJSON) and issues a session. A passkey is full MFA, so
// no TOTP challenge. Every failure is ErrUnauthorized.
func (a *AuthService) FinishPasskeyLogin(ctx context.Context, ceremony string, credential []byte, userAgent string, ip *string) (string, store.UserRow, error) {
	if a.WA == nil {
		return "", store.UserRow{}, ErrPasskeysDisabled
	}
	_, sd, err := a.takeCeremony(ctx, ceremony, "login")
	if err != nil {
		return "", store.UserRow{}, err
	}
	parsed, err := protocol.ParseCredentialRequestResponseBytes(credential)
	if err != nil {
		return "", store.UserRow{}, ErrUnauthorized
	}
	var row store.UserRow
	handler := func(rawID, handle []byte) (webauthn.User, error) {
		uid, err := uuid.FromBytes(handle)
		if err != nil {
			return nil, ErrUnauthorized
		}
		if row, err = a.App.Store.UserByID(ctx, uid.String()); err != nil {
			return nil, ErrUnauthorized
		}
		if !row.Enabled || row.PasswordHash == "!" {
			return nil, ErrUnauthorized
		}
		return a.loadWAUser(ctx, row)
	}
	_, cred, err := a.WA.ValidatePasskeyLogin(handler, sd, parsed)
	if err != nil {
		if row.ID != "" {
			a.auditLogin(ctx, row.ID, ip, false)
		}
		return "", store.UserRow{}, ErrUnauthorized
	}
	if cred.Authenticator.CloneWarning {
		_ = a.App.Store.Audit(ctx, row.ID, "auth.passkey_clone_warning", "user", row.ID, []byte(`{}`), ip)
		a.auditLogin(ctx, row.ID, ip, false)
		return "", store.UserRow{}, ErrUnauthorized
	}
	pk, err := a.App.Store.PasskeyByCredentialID(ctx, cred.ID)
	if err != nil {
		return "", store.UserRow{}, ErrUnauthorized
	}
	raw, err := json.Marshal(cred)
	if err != nil {
		return "", store.UserRow{}, err
	}
	if err := a.App.Store.UpdatePasskeyCredential(ctx, pk.ID, raw); err != nil {
		return "", store.UserRow{}, fmt.Errorf("update passkey: %w", err)
	}
	return a.issueSession(ctx, row, userAgent, ip)
}

func (a *AuthService) ListPasskeys(ctx context.Context, p Principal) ([]PasskeyView, error) {
	rows, err := a.App.Store.PasskeysByUser(ctx, p.UserID)
	if err != nil {
		return nil, err
	}
	out := make([]PasskeyView, 0, len(rows))
	for _, r := range rows {
		out = append(out, viewOf(r))
	}
	return out, nil
}

func (a *AuthService) RenamePasskey(ctx context.Context, p Principal, passkeyID, name string) (PasskeyView, error) {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > maxPasskeyName {
		return PasskeyView{}, bad("invalid name")
	}
	r, err := a.App.Store.RenamePasskey(ctx, p.UserID, passkeyID, name)
	if err != nil {
		return PasskeyView{}, err
	}
	_ = a.App.Store.Audit(ctx, p.UserID, "passkey.rename", "passkey", passkeyID, []byte(`{}`), nil)
	return viewOf(r), nil
}

// DeletePasskey refuses to remove the last passkey of a user without a usable
// password (it would lock them out).
func (a *AuthService) DeletePasskey(ctx context.Context, p Principal, passkeyID string) error {
	row, err := a.App.Store.UserByID(ctx, p.UserID)
	if err != nil {
		return ErrNotFound
	}
	if row.PasswordHash == "" || row.PasswordHash == "!" {
		if rows, err := a.App.Store.PasskeysByUser(ctx, p.UserID); err != nil {
			return err
		} else if len(rows) <= 1 {
			return ErrConflict
		}
	}
	if err := a.App.Store.DeletePasskey(ctx, p.UserID, passkeyID); err != nil {
		return err
	}
	return a.App.Store.Audit(ctx, p.UserID, "passkey.remove", "passkey", passkeyID, []byte(`{}`), nil)
}

// RemovePasskeys is the lost-device path (CLI): drops every passkey and the
// user's sessions. Returns how many passkeys were removed.
func (a *AuthService) RemovePasskeys(ctx context.Context, username string) (int64, error) {
	row, err := a.App.Store.UserByUsername(ctx, strings.ToLower(username))
	if err != nil {
		return 0, ErrNotFound
	}
	n, err := a.App.Store.DeleteUserPasskeys(ctx, row.ID)
	if err != nil {
		return 0, err
	}
	if err := a.App.Store.RevokeUserSessions(ctx, row.ID); err != nil {
		return 0, err
	}
	return n, a.App.Store.Audit(ctx, "", "passkey.reset", "user", row.ID, []byte(`{}`), nil)
}

// PasskeysForUsername lists a user's passkeys (CLI).
func (a *AuthService) PasskeysForUsername(ctx context.Context, username string) ([]PasskeyView, error) {
	row, err := a.App.Store.UserByUsername(ctx, strings.ToLower(username))
	if err != nil {
		return nil, ErrNotFound
	}
	return a.ListPasskeys(ctx, Principal{UserID: row.ID})
}

// CreateMobileCode mints a one-time code bound to the app's PKCE challenge
// (base64url SHA-256 of its verifier). Sessions only.
func (a *AuthService) CreateMobileCode(ctx context.Context, p Principal, challenge string) (string, error) {
	if p.Kind != "session" {
		return "", ErrForbidden
	}
	if len(challenge) != 43 { // base64url SHA-256, unpadded
		return "", bad("invalid code_challenge")
	}
	code := id.NewToken()
	if err := a.App.Store.CreateHandoff(ctx, id.HashToken(code), p.UserID, challenge, time.Now().Add(handoffTTL)); err != nil {
		return "", fmt.Errorf("create handoff: %w", err)
	}
	return code, nil
}

// ExchangeMobileCode redeems a code with its verifier for a normal API token.
func (a *AuthService) ExchangeMobileCode(ctx context.Context, code, verifier string) (string, error) {
	uid, challenge, err := a.App.Store.TakeHandoff(ctx, id.HashToken(code))
	if errors.Is(err, store.ErrNotFound) {
		return "", ErrUnauthorized
	}
	if err != nil {
		return "", fmt.Errorf("take handoff: %w", err)
	}
	sum := sha256.Sum256([]byte(verifier))
	got := base64.RawURLEncoding.EncodeToString(sum[:])
	if subtle.ConstantTimeCompare([]byte(got), []byte(challenge)) != 1 {
		return "", ErrUnauthorized
	}
	if u, err := a.App.Store.UserByID(ctx, uid); err != nil || !u.Enabled {
		return "", ErrUnauthorized
	}
	raw, _, err := a.App.Users.issueAPIToken(ctx, uid, uid, "mobile")
	return raw, err
}

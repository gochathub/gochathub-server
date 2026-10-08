package service

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/pquerna/otp"
	"github.com/pquerna/otp/totp"

	"github.com/gochathub/gochathub-server/internal/id"
	"github.com/gochathub/gochathub-server/internal/pwd"
	"github.com/gochathub/gochathub-server/internal/store"
)

const (
	totpPeriod        = 30
	challengeTTL      = 5 * time.Minute
	challengeAttempts = 5
	backupCodeCount   = 10
)

// TwoFactorRequired is returned by Login for 2FA users: the password was
// right, no session exists yet, redeem Challenge via Login2FA.
type TwoFactorRequired struct{ Challenge string }

func (*TwoFactorRequired) Error() string { return "two-factor code required" }

var errBadCode = bad("invalid code")

func (a *AuthService) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}

func hashBackup(code string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(code)))
	return hex.EncodeToString(sum[:])
}

// checkTOTP accepts the current step and one either side (clock drift) and
// spends it: each step is usable once per user.
func (a *AuthService) checkTOTP(ctx context.Context, userID, secret, code string) (bool, error) {
	code = strings.TrimSpace(code)
	now := a.now()
	for _, off := range []time.Duration{0, -totpPeriod * time.Second, totpPeriod * time.Second} {
		t := now.Add(off)
		ok, err := totp.ValidateCustom(code, secret, t, totp.ValidateOpts{
			Period: totpPeriod, Digits: otp.DigitsSix, Algorithm: otp.AlgorithmSHA1,
		})
		if errors.Is(err, otp.ErrValidateSecretInvalidBase32) {
			return false, fmt.Errorf("stored totp secret for %s: %w", userID, err)
		}
		if err != nil { // wrong length / non-numeric: not a TOTP code
			return false, nil
		}
		if ok {
			return a.App.Store.ConsumeTOTPStep(ctx, userID, t.Unix()/totpPeriod)
		}
	}
	return false, nil
}

// checkSecondFactor: TOTP code first, then a single-use backup code.
func (a *AuthService) checkSecondFactor(ctx context.Context, userID string, t store.TOTPRow, code string) (bool, error) {
	if ok, err := a.checkTOTP(ctx, userID, t.Secret, code); err != nil || ok {
		return ok, err
	}
	return a.App.Store.UseBackupCode(ctx, userID, hashBackup(code))
}

// Login2FA redeems a Login challenge with a TOTP or backup code.
func (a *AuthService) Login2FA(ctx context.Context, challenge, code, userAgent string, ip *string) (string, store.UserRow, error) {
	h := id.HashToken(challenge)
	uid, err := a.App.Store.TryLoginChallenge(ctx, h, challengeAttempts)
	if errors.Is(err, store.ErrNotFound) {
		return "", store.UserRow{}, ErrUnauthorized
	}
	if err != nil {
		return "", store.UserRow{}, fmt.Errorf("challenge: %w", err)
	}
	t, err := a.App.Store.TOTPByUser(ctx, uid)
	if err != nil || !t.Enabled {
		return "", store.UserRow{}, ErrUnauthorized
	}
	ok, err := a.checkSecondFactor(ctx, uid, t, code)
	if err != nil {
		return "", store.UserRow{}, err
	}
	if !ok {
		a.auditLogin(ctx, uid, ip, false)
		return "", store.UserRow{}, ErrUnauthorized
	}
	row, err := a.App.Store.UserByID(ctx, uid)
	if err != nil || !row.Enabled {
		return "", store.UserRow{}, ErrUnauthorized
	}
	_ = a.App.Store.DeleteLoginChallenge(ctx, h)
	return a.issueSession(ctx, row, userAgent, ip)
}

// SetupTOTP starts enrollment; nothing is enforced until EnableTOTP.
func (a *AuthService) SetupTOTP(ctx context.Context, p Principal) (secret, url string, err error) {
	key, err := totp.Generate(totp.GenerateOpts{Issuer: "goChatHub", AccountName: p.Username})
	if err != nil {
		return "", "", err
	}
	ok, err := a.App.Store.SetPendingTOTP(ctx, p.UserID, key.Secret())
	if err != nil {
		return "", "", err
	}
	if !ok {
		return "", "", ErrConflict
	}
	return key.Secret(), key.URL(), nil
}

// EnableTOTP confirms enrollment with a first code and returns the
// one-time backup codes.
func (a *AuthService) EnableTOTP(ctx context.Context, p Principal, code string) ([]string, error) {
	t, err := a.App.Store.TOTPByUser(ctx, p.UserID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, bad("start setup first")
	}
	if err != nil {
		return nil, err
	}
	if t.Enabled {
		return nil, ErrConflict
	}
	if ok, err := a.checkTOTP(ctx, p.UserID, t.Secret, code); err != nil {
		return nil, err
	} else if !ok {
		return nil, errBadCode
	}
	if err := a.App.Store.EnableTOTP(ctx, p.UserID); err != nil {
		return nil, err
	}
	codes, err := a.newBackupCodes(ctx, p.UserID)
	if err != nil {
		return nil, err
	}
	return codes, a.App.Store.Audit(ctx, p.UserID, "auth.2fa_enabled", "user", p.UserID, []byte(`{}`), nil)
}

// RegenerateBackupCodes replaces all backup codes; needs a live TOTP code.
func (a *AuthService) RegenerateBackupCodes(ctx context.Context, p Principal, code string) ([]string, error) {
	t, err := a.App.Store.TOTPByUser(ctx, p.UserID)
	if err != nil || !t.Enabled {
		return nil, bad("two-factor is not enabled")
	}
	if ok, err := a.checkTOTP(ctx, p.UserID, t.Secret, code); err != nil {
		return nil, err
	} else if !ok {
		return nil, errBadCode
	}
	return a.newBackupCodes(ctx, p.UserID)
}

// DisableTOTP needs the password and a valid code (TOTP or backup).
func (a *AuthService) DisableTOTP(ctx context.Context, p Principal, password, code string) error {
	row, err := a.App.Store.UserByID(ctx, p.UserID)
	if err != nil {
		return ErrNotFound
	}
	if ok, err := pwd.Verify(row.PasswordHash, password); err != nil || !ok {
		return ErrUnauthorized
	}
	t, err := a.App.Store.TOTPByUser(ctx, p.UserID)
	if err != nil || !t.Enabled {
		return bad("two-factor is not enabled")
	}
	if ok, err := a.checkSecondFactor(ctx, p.UserID, t, code); err != nil {
		return err
	} else if !ok {
		return errBadCode
	}
	if err := a.App.Store.DeleteTOTP(ctx, p.UserID); err != nil {
		return err
	}
	return a.App.Store.Audit(ctx, p.UserID, "auth.2fa_disabled", "user", p.UserID, []byte(`{}`), nil)
}

// ResetTOTP is the lost-device path (CLI): drops the factor and sessions.
func (a *AuthService) ResetTOTP(ctx context.Context, username string) error {
	row, err := a.App.Store.UserByUsername(ctx, strings.ToLower(username))
	if err != nil {
		return err
	}
	if err := a.App.Store.DeleteTOTP(ctx, row.ID); err != nil {
		return err
	}
	if err := a.App.Store.RevokeUserSessions(ctx, row.ID); err != nil {
		return err
	}
	return a.App.Store.Audit(ctx, "", "auth.2fa_reset", "user", row.ID, []byte(`{}`), nil)
}

func (a *AuthService) newBackupCodes(ctx context.Context, userID string) ([]string, error) {
	codes := make([]string, backupCodeCount)
	hashes := make([]string, backupCodeCount)
	for i := range codes {
		codes[i] = rand.Text()[:10] // 50 bits, base32 upper-case
		hashes[i] = hashBackup(codes[i])
	}
	return codes, a.App.Store.ReplaceBackupCodes(ctx, userID, hashes)
}

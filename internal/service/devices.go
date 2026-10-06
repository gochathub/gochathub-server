package service

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"

	"github.com/gochathub/gochathub-server/internal/id"
	"github.com/gochathub/gochathub-server/internal/model"
	"github.com/gochathub/gochathub-server/internal/store"
)

// DeviceService registers devices and their UnifiedPush/Web Push targets,
// including the validation-ping round trip (docs/UNIFIEDPUSH.md §3.3).
type DeviceService struct{ App *App }

// RegisterInput is POST /devices.
type RegisterInput struct {
	Platform      string                  `json:"platform"`
	ClientName    string                  `json:"client_name"`
	ClientVersion string                  `json:"client_version"`
	Push          *model.PushRegistration `json:"push_registration"`
}

// RegisterOutput returns the validation state.
type RegisterOutput struct {
	DeviceID           string `json:"device_id"`
	ValidationRequired bool   `json:"validation_required"`
}

// Register creates the device and, when push data arrives, the endpoint plus
// the encrypted validation ping. The client proves receipt by posting the
// token back to /devices/{id}/validate.
func (ds *DeviceService) Register(ctx context.Context, p Principal, in RegisterInput) (RegisterOutput, error) {
	switch in.Platform {
	case "android", "web":
	default:
		return RegisterOutput{}, bad("platform must be android|web")
	}
	if in.ClientName == "" || len(in.ClientName) > 64 {
		return RegisterOutput{}, bad("client_name required (max 64 chars)")
	}
	d := &store.DeviceRow{
		ID:            id.NewID(),
		UserID:        p.UserID,
		Platform:      in.Platform,
		ClientName:    in.ClientName,
		ClientVersion: in.ClientVersion,
	}
	if in.Push == nil {
		if err := ds.App.Store.CreateDevice(ctx, d); err != nil {
			return RegisterOutput{}, fmt.Errorf("create device: %w", err)
		}
		return RegisterOutput{DeviceID: d.ID}, nil
	}
	// validate the three fields before persisting anything (§3.2 contract)
	if len(in.Push.Endpoint) > 1000 || in.Push.Endpoint == "" {
		return RegisterOutput{}, bad("endpoint required, max 1000 bytes")
	}
	if in.Push.PublicKey == "" || in.Push.AuthSecret == "" {
		return RegisterOutput{}, bad("public_key and auth_secret required (connector-generated)")
	}
	e := &store.PushEndpointRow{
		ID:         id.NewID(),
		DeviceID:   d.ID,
		UserID:     p.UserID,
		Endpoint:   in.Push.Endpoint,
		PublicKey:  in.Push.PublicKey,
		AuthSecret: in.Push.AuthSecret,
	}
	token := id.NewToken()
	sum := sha256.Sum256([]byte(token))
	e.ValidateHash = sum[:]
	err := ds.App.Store.WithTx(ctx, func(tx *store.Store) error {
		if err := tx.CreateDevice(ctx, d); err != nil {
			return err
		}
		return tx.CreatePushEndpoint(ctx, e)
	})
	if err != nil {
		return RegisterOutput{}, fmt.Errorf("register device: %w", err)
	}
	out := RegisterOutput{DeviceID: d.ID, ValidationRequired: true}
	if ds.App.Sender == nil {
		// push sending disabled: no ping possible; validation stays open via
		// /validate once a sender is configured
		return out, nil
	}
	if err := ds.App.Sender.PingValidation(ctx, *e, token); err != nil {
		ds.App.Log.WarnContext(ctx, "device validation ping failed", "device", d.ID, "err", err)
	}
	return out, nil
}

// Validate completes the ping round trip.
func (ds *DeviceService) Validate(ctx context.Context, p Principal, deviceID, token string) error {
	if deviceID == "" || token == "" {
		return bad("device id and token required")
	}
	e, err := ds.App.Store.PushEndpointForDeviceByDevice(ctx, deviceID, p.UserID)
	if err != nil {
		return ErrNotFound
	}
	if e.ValidatedAt != nil {
		return nil // already validated
	}
	sum := sha256.Sum256([]byte(token))
	if constEq(sum[:], e.ValidateHash) {
		return ds.App.Store.ValidatePushEndpoint(ctx, e.ID)
	}
	return ErrForbidden
}

// Renew replaces the endpoint for a device (connector may re-register).
func (ds *DeviceService) Renew(ctx context.Context, p Principal, deviceID string, in model.PushRegistration) error {
	if _, err := ds.App.Store.DeviceForUser(ctx, deviceID, p.UserID); err != nil {
		return ErrNotFound
	}
	old, err := ds.App.Store.ValidEndpointForDevice(ctx, deviceID, p.UserID)
	if errors.Is(err, store.ErrNotFound) {
		return ds.registerEndpointForDevice(ctx, p, deviceID, in)
	}
	if err != nil {
		return err
	}
	e := &store.PushEndpointRow{
		ID:         id.NewID(),
		DeviceID:   deviceID,
		UserID:     p.UserID,
		Endpoint:   in.Endpoint,
		PublicKey:  in.PublicKey,
		AuthSecret: in.AuthSecret,
	}
	if err := ds.App.Store.ReplacePushEndpoint(ctx, old.ID, e); err != nil {
		return fmt.Errorf("replace push endpoint: %w", err)
	}
	if ds.App.Sender != nil {
		token := id.NewToken()
		if pingErr := ds.App.Sender.PingValidation(ctx, *e, token); pingErr == nil {
			sum := sha256.Sum256([]byte(token))
			return ds.App.Store.SetValidationHash(ctx, e.ID, sum[:])
		}
	}
	return nil
}

func (ds *DeviceService) registerEndpointForDevice(ctx context.Context, p Principal, deviceID string, in model.PushRegistration) error {
	e := &store.PushEndpointRow{
		ID:         id.NewID(),
		DeviceID:   deviceID,
		UserID:     p.UserID,
		Endpoint:   in.Endpoint,
		PublicKey:  in.PublicKey,
		AuthSecret: in.AuthSecret,
	}
	return ds.App.Store.CreatePushEndpoint(ctx, e)
}

// Delete removes the device and its push registration (cascade).
func (ds *DeviceService) Delete(ctx context.Context, p Principal, deviceID string) error {
	tag, err := ds.App.Store.DeviceForUser(ctx, deviceID, p.UserID)
	if err != nil {
		return ErrNotFound
	}
	_ = tag
	return ds.App.Store.DeleteDevice(ctx, deviceID, p.UserID)
}

// constEq is constant-time []byte equality.
func constEq(a, b []byte) bool {
	if len(a) != len(b) || len(a) == 0 {
		return len(a) == len(b)
	}
	var v byte
	for i := range a {
		v |= a[i] ^ b[i]
	}
	return v == 0
}

package store

import (
	"context"
	"time"
)

// DeviceRow is the devices shape.
type DeviceRow struct {
	ID            string
	UserID        string
	Platform      string
	ClientName    string
	ClientVersion string
	CreatedAt     time.Time
	LastSeenAt    time.Time
}

func (s *Store) CreateDevice(ctx context.Context, d *DeviceRow) error {
	return s.Q.QueryRow(ctx, `
		INSERT INTO devices (id, user_id, platform, client_name, client_version)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING created_at, last_seen_at`,
		d.ID, d.UserID, d.Platform, d.ClientName, d.ClientVersion,
	).Scan(&d.CreatedAt, &d.LastSeenAt)
}

func (s *Store) DeviceForUser(ctx context.Context, deviceID, userID string) (DeviceRow, error) {
	var d DeviceRow
	err := s.Q.QueryRow(ctx, `
		SELECT id::text, user_id::text, platform, client_name, client_version, created_at, last_seen_at
		FROM devices WHERE id = $1 AND user_id = $2`,
		deviceID, userID).Scan(&d.ID, &d.UserID, &d.Platform, &d.ClientName,
		&d.ClientVersion, &d.CreatedAt, &d.LastSeenAt)
	return d, err
}

func (s *Store) TouchDevice(ctx context.Context, id string) error {
	_, err := s.Q.Exec(ctx, `UPDATE devices SET last_seen_at = now() WHERE id = $1`, id)
	return err
}

// PushEndpointRow is the push_endpoints shape.
type PushEndpointRow struct {
	ID           string
	DeviceID     string
	UserID       string
	Endpoint     string
	PublicKey    string
	AuthSecret   string
	ValidateHash []byte // sha256 of the ping token; cleared once validated
	ValidatedAt  *time.Time
	CreatedAt    time.Time
	LastSeenAt   time.Time
	RevokedAt    *time.Time
}

// CreatePushEndpoint registers a device's push target with its validation
// ping hash (docs/UNIFIEDPUSH.md §3.3).
func (s *Store) CreatePushEndpoint(ctx context.Context, e *PushEndpointRow) error {
	return s.Q.QueryRow(ctx, `
		INSERT INTO push_endpoints (id, device_id, user_id, endpoint, public_key, auth_secret, validation_token_hash)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		RETURNING created_at, last_seen_at`,
		e.ID, e.DeviceID, e.UserID, e.Endpoint, e.PublicKey, e.AuthSecret, e.ValidateHash,
	).Scan(&e.CreatedAt, &e.LastSeenAt)
}

// ValidatePushEndpoint clears the ping hash and stamps validated_at once the
// client proves receipt of the ping token.
func (s *Store) ValidatePushEndpoint(ctx context.Context, endpointID string) error {
	_, err := s.Q.Exec(ctx, `
		UPDATE push_endpoints SET validation_token_hash = NULL, validated_at = now(), last_seen_at = now()
		WHERE id = $1 AND validated_at IS NULL`, endpointID)
	return err
}

func (s *Store) ValidatedEndpointsForUsers(ctx context.Context, userIDs []string) ([]PushEndpointRow, error) {
	if len(userIDs) == 0 {
		return nil, nil
	}
	rows, err := s.Q.Query(ctx, `
		SELECT id::text, device_id::text, user_id::text, endpoint, public_key, auth_secret,
			validation_token_hash, validated_at, created_at, last_seen_at, revoked_at
		FROM push_endpoints
		WHERE user_id::text = ANY($1) AND revoked_at IS NULL AND validated_at IS NOT NULL`, userIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PushEndpointRow
	for rows.Next() {
		var e PushEndpointRow
		if err := rows.Scan(&e.ID, &e.DeviceID, &e.UserID, &e.Endpoint, &e.PublicKey, &e.AuthSecret,
			&e.ValidateHash, &e.ValidatedAt, &e.CreatedAt, &e.LastSeenAt, &e.RevokedAt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// ReplacePushEndpoint swaps a device's push registration (endpoint renewal).
func (s *Store) ReplacePushEndpoint(ctx context.Context, oldID string, e *PushEndpointRow) error {
	return s.WithTx(ctx, func(tx *Store) error {
		if _, err := tx.Q.Exec(ctx,
			`UPDATE push_endpoints SET revoked_at = now() WHERE id = $1 AND revoked_at IS NULL`, oldID); err != nil {
			return err
		}
		return tx.CreatePushEndpoint(ctx, e)
	})
}

// RevokePushEndpointByID revokes without device checks (dead-endpoint cleanup).
func (s *Store) RevokePushEndpointByID(ctx context.Context, endpointID string) error {
	_, err := s.Q.Exec(ctx, `
		UPDATE push_endpoints SET revoked_at = now() WHERE id = $1 AND revoked_at IS NULL`, endpointID)
	return err
}

// PushEndpointForDeviceByDevice returns the (newest) endpoint for a device
// owned by the user.
func (s *Store) PushEndpointForDeviceByDevice(ctx context.Context, deviceID, userID string) (PushEndpointRow, error) {
	const q = `SELECT id::text, device_id::text, user_id::text, endpoint, public_key, auth_secret,
		validation_token_hash, validated_at, created_at, last_seen_at, revoked_at
		FROM push_endpoints WHERE device_id = $1 AND user_id = $2 AND revoked_at IS NULL
		ORDER BY created_at DESC LIMIT 1`
	var e PushEndpointRow
	err := s.Q.QueryRow(ctx, q, deviceID, userID).Scan(
		&e.ID, &e.DeviceID, &e.UserID, &e.Endpoint, &e.PublicKey, &e.AuthSecret,
		&e.ValidateHash, &e.ValidatedAt, &e.CreatedAt, &e.LastSeenAt, &e.RevokedAt)
	return e, err
}

// ValidEndpointForDevice returns the validated endpoint for a device.
func (s *Store) ValidEndpointForDevice(ctx context.Context, deviceID, userID string) (PushEndpointRow, error) {
	const q = `SELECT id::text, device_id::text, user_id::text, endpoint, public_key, auth_secret,
		validation_token_hash, validated_at, created_at, last_seen_at, revoked_at
		FROM push_endpoints WHERE device_id = $1 AND user_id = $2 AND revoked_at IS NULL
		AND validated_at IS NOT NULL
		ORDER BY created_at DESC LIMIT 1`
	var e PushEndpointRow
	err := s.Q.QueryRow(ctx, q, deviceID, userID).Scan(
		&e.ID, &e.DeviceID, &e.UserID, &e.Endpoint, &e.PublicKey, &e.AuthSecret,
		&e.ValidateHash, &e.ValidatedAt, &e.CreatedAt, &e.LastSeenAt, &e.RevokedAt)
	return e, err
}

// SetValidationHash stores the ping hash on a fresh endpoint (renewal path).
func (s *Store) SetValidationHash(ctx context.Context, endpointID string, hash []byte) error {
	_, err := s.Q.Exec(ctx, `
		UPDATE push_endpoints SET validation_token_hash = $2, validated_at = NULL
		WHERE id = $1`, endpointID, hash)
	return err
}

// DeleteDevice removes the device row; push endpoints cascade.
func (s *Store) DeleteDevice(ctx context.Context, deviceID, userID string) error {
	tag, err := s.Q.Exec(ctx, `DELETE FROM devices WHERE id = $1 AND user_id = $2`, deviceID, userID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// TouchPushEndpoint records liveness after a successful send.
func (s *Store) TouchPushEndpoint(ctx context.Context, endpointID string) error {
	_, err := s.Q.Exec(ctx, `UPDATE push_endpoints SET last_seen_at = now() WHERE id = $1`, endpointID)
	return err
}

// --- app_config (VAPID key persistence, ADR: UNIFIEDPUSH.md §7.1) ---

func (s *Store) GetConfigValue(ctx context.Context, key string) (string, error) {
	var v string
	err := s.Q.QueryRow(ctx, `SELECT value #>> '{}' FROM app_config WHERE key = $1`, key).Scan(&v)
	return v, err
}

func (s *Store) SetConfigValue(ctx context.Context, key, value string) error {
	// to_jsonb(text) stores the scalar string; #>> '{}' reads it back
	_, err := s.Q.Exec(ctx, `
		INSERT INTO app_config (key, value) VALUES ($1, to_jsonb($2::text))
		ON CONFLICT (key) DO UPDATE SET value = to_jsonb($2::text), updated_at = now()`, key, value)
	return err
}

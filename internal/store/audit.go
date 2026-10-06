package store

import (
	"context"
)

// Audit writes a security-relevant action record. actorUserID "" (CLI/system)
// stores NULL.
func (s *Store) Audit(ctx context.Context, actorUserID, action, targetType, targetID string, detailJSON []byte, ip *string) error {
	var actor any
	if actorUserID != "" {
		actor = actorUserID
	}
	_, err := s.Q.Exec(ctx, `
		INSERT INTO audit_log (actor_user_id, action, target_type, target_id, detail, ip_address)
		VALUES ($1, $2, $3, $4, $5::jsonb, $6)`,
		actor, action, targetType, targetID, string(detailJSON), ip)
	return err
}

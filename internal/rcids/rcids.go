// Package rcids holds the deterministic uuidv5 namespace URL strings shared
// by the Rocket.Chat one-shot import (internal/migrate) and the live bridge
// (internal/rcbridge). One identity per RC id, across import and bridge.
// Note: since ADR-024 the live bridge derives per-owner ids instead
// (rcbridge.rcID); only the import uses these now.
package rcids

// Namespace URL fragments; NS* derive uuid.NewSHA1(uuid.NameSpaceURL, []byte(const)).
const (
	NSUser = "https://gochathub.app/migrate/rc/user"
	NSRoom = "https://gochathub.app/migrate/rc/room"
	NSMsg  = "https://gochathub.app/migrate/rc/msg"
)

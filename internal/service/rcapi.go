package service

import (
	"context"
	"fmt"

	"github.com/gochathub/gochathub-server/internal/model"
)

// Cross-server room bridge API types (docs/ROCKETCHAT-INTEGRATION.md). The
// implementer of RCSender lives in internal/rcbridge; the interface exists so
// the message service can route source-marked rooms without a dependency on
// the bridge package.

// LinkRequest is POST /integrations/rocket-chat/link: the user's RC userId
// (or username) plus a Personal Access Token created in the RC workspace.
type LinkRequest struct {
	RCUserID string `json:"rc_user_id"`
	Token    string `json:"token"`
}

// LinkStatus is GET/POST-link — the PAT is never echoed after the link call.
type LinkStatus struct {
	Linked     bool   `json:"linked"`
	RCUserID   string `json:"rc_user_id,omitempty"`
	RCUsername string `json:"rc_username,omitempty"`
}

// Bridge is the cross-server surface implemented by internal/rcbridge.Bridge
// (the interface exists so message routing needs no dependency on the bridge
// package). Nil App.RCBridge = feature disabled.
type RCBridge interface {
	Send(ctx context.Context, p Principal, roomID string, in MessageInput) (model.Message, error)
	Link(ctx context.Context, p Principal, req LinkRequest) (LinkStatus, error)
	Status(ctx context.Context, p Principal) (LinkStatus, error)
	Unlink(ctx context.Context, p Principal) error
}

// Bad is the exported validation-error constructor (bridge callers use it).
func Bad(format string, args ...any) error {
	return &ValidationError{Reason: fmt.Sprintf(format, args...)}
}

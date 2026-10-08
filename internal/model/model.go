// Package model defines the API-facing domain types shared by HTTP, WS, CLI.
package model

import "time"

// Preferences is the stored/GET form with concrete values (defaults false).
type Preferences struct {
	LastSeenVisible      bool `json:"last_seen_visible"`
	ReadReceipts         bool `json:"read_receipts"`
	AllowGroupInvites    bool `json:"allow_group_invites"`
	AllowPrivateMessages bool `json:"allow_private_messages"`
}

// PreferencesPatch is the PATCH /users/me/preferences form (partial).
type PreferencesPatch struct {
	LastSeenVisible      *bool `json:"last_seen_visible,omitempty"`
	ReadReceipts         *bool `json:"read_receipts,omitempty"`
	AllowGroupInvites    *bool `json:"allow_group_invites,omitempty"`
	AllowPrivateMessages *bool `json:"allow_private_messages,omitempty"`
}

// User is the API user. `email` is only populated on self payloads.
type User struct {
	ID          string     `json:"id"`
	Username    string     `json:"username"`
	DisplayName string     `json:"display_name"`
	Role        string     `json:"role"`
	Email       string     `json:"email,omitempty"`
	Timezone    string     `json:"timezone,omitempty"` // IANA name; self payload and contacts
	AvatarURL   string     `json:"avatar_url"`
	LastSeenAt  *time.Time `json:"last_seen_at"`
	// TwoFactorEnabled is present on self payloads only.
	TwoFactorEnabled *bool `json:"two_factor_enabled,omitempty"`
}

// Contact relationships are one-way (ADR-010).
type Contact struct {
	ID        string    `json:"id"` // the contact's user id; used for DELETE
	User      User      `json:"user"`
	CreatedAt time.Time `json:"created_at"`
}

type RoomType string

const (
	RoomPublic   RoomType = "public"
	RoomPrivate  RoomType = "private"
	RoomDirect   RoomType = "direct"
	RoomGroupDir RoomType = "group_direct"
)

type Room struct {
	ID              string     `json:"id"`
	Type            RoomType   `json:"type"`
	Name            *string    `json:"name"`
	Description     string     `json:"description"`
	AvatarURL       string     `json:"avatar_url"`
	PinnedMessageID *string    `json:"pinned_message_id"`
	ArchivedAt      *time.Time `json:"archived_at,omitempty"`
	// Archived is the caller's member-level archive flag (sidebar filter);
	// rooms without membership context report false.
	Archived    bool      `json:"archived,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
	MyRole      string    `json:"my_role,omitempty"` // caller's member role
	UnreadCount int64     `json:"unread_count,omitempty"`
}

type InviteStatus string

const (
	InvitePending  InviteStatus = "pending"
	InviteAccepted InviteStatus = "accepted"
	InviteDeclined InviteStatus = "declined"
	InviteRevoked  InviteStatus = "revoked"
)

type Invite struct {
	ID         string       `json:"id"`
	Room       Room         `json:"room"`
	Inviter    User         `json:"inviter"`
	Invitee    User         `json:"invitee"`
	Status     InviteStatus `json:"status"`
	ExpiresAt  *time.Time   `json:"expires_at"`
	CreatedAt  time.Time    `json:"created_at"`
	AcceptedAt *time.Time   `json:"accepted_at"`
}

type Receipts struct {
	DeliveredAt *time.Time `json:"delivered_at"`
	ReadAt      *time.Time `json:"read_at"`
}

type Message struct {
	ID               string       `json:"id"`
	RoomID           string       `json:"room_id"`
	AuthorID         string       `json:"author_id"`
	Author           *User        `json:"author,omitempty"`
	Body             string       `json:"body"`
	Format           string       `json:"format"`
	ReplyToMessageID *string      `json:"reply_to_message_id"`
	Attachments      []Attachment `json:"attachments,omitempty"`
	Reactions        []Reaction   `json:"reactions,omitempty"`
	Receipts         *Receipts    `json:"receipts,omitempty"`
	CreatedAt        time.Time    `json:"created_at"`
	EditedAt         *time.Time   `json:"edited_at"`
	DeletedAt        *time.Time   `json:"deleted_at"`
}

// Reaction is an aggregated count per emoji (ADR-009-adjacent rendering data).
type Reaction struct {
	Emoji string `json:"emoji"`
	Count int64  `json:"count"`
}

// MessagePage is the cursor-paginated listing form.
type MessagePage struct {
	Items      []Message `json:"items"`
	NextCursor string    `json:"next_cursor"`
}

type Attachment struct {
	ID           string    `json:"id"`
	Filename     string    `json:"filename"`
	MimeType     string    `json:"mime_type"`
	SizeBytes    int64     `json:"size_bytes"`
	URL          string    `json:"url"`
	ThumbnailURL string    `json:"thumbnail_url,omitempty"`
	Width        *int      `json:"width"`
	Height       *int      `json:"height"`
	Status       string    `json:"-"` // pending/ready/failed — not API-visible
	CreatedAt    time.Time `json:"created_at,omitempty"`
}

// ChangePasswordRequest is the PATCH /users/me/password body.
type ChangePasswordRequest struct {
	OldPassword string `json:"old_password"`
	NewPassword string `json:"new_password"`
}

// UpdateUserInput is the PATCH /users/me body; nil = keep, empty string = clear.
type UpdateUserInput struct {
	DisplayName    *string `json:"display_name"`
	Email          *string `json:"email"`
	Timezone       *string `json:"timezone"`
	AvatarAttachID *string `json:"avatar_attachment_id"`
}

type PushRegistration struct {
	Endpoint   string `json:"endpoint"`
	PublicKey  string `json:"public_key"`
	AuthSecret string `json:"auth_secret"`
}

type Device struct {
	ID            string    `json:"id"`
	Platform      string    `json:"platform"`
	ClientName    string    `json:"client_name"`
	ClientVersion string    `json:"client_version"`
	CreatedAt     time.Time `json:"created_at"`
	LastSeenAt    time.Time `json:"last_seen_at"`
	Validated     bool      `json:"validated"`
}

// WSEnvelope is the WebSocket event envelope (docs/WEBSOCKETS.md).
type WSEnvelope struct {
	Type      string         `json:"type"` // e.g. "message.created"
	ID        string         `json:"id"`
	Timestamp time.Time      `json:"timestamp"`
	RoomID    string         `json:"room_id,omitempty"`
	Data      map[string]any `json:"data,omitempty"`
}

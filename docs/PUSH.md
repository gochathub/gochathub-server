# Android Push / UnifiedPush / ntfy

## Goal

Use UnifiedPush from the Android application with the existing self-hosted ntfy server acting as the distributor.

Do NOT add FCM or APNs directly to the chat backend.

## Registration flow

Conceptually:

```text
Android app
    |
    | UnifiedPush registration
    v
ntfy distributor
    |
    | returns Web Push/UnifiedPush registration
    v
Android app
    |
    | POST registration to chat API
    v
Go server
    |
    | persist endpoint/key/auth data
    v
PostgreSQL
```

The registration API and protocol fields are now documented in
`UNIFIEDPUSH.md` (verified against the UnifiedPush spec AND_3.1.0 and ntfy
docs).

## Sending

When a message is created:

```text
message transaction
      |
      +--> WebSocket to active recipients
      |
      +--> determine push recipients
               |
               +--> POST encrypted Web Push notification
                    to registered UnifiedPush endpoint
                         |
                         v
                       ntfy
                         |
                         v
                     Android OS
```

The backend should not need to know that ntfy is ultimately delivering the notification.

## Notification policy

At minimum:

- Do not push when user is actively viewing the room, unless explicitly configured.
- Push direct messages.
- Push mentions.
- Respect muted rooms.
- Respect disabled notifications.
- Avoid duplicate notifications to multiple devices when the policy does not require them.

## Push payload

Keep payload small.

Example conceptual payload:

```json
{
  "type": "chat.message",
  "room_id": "room-id",
  "message_id": "message-id"
}
```

Do not put sensitive full message content in the push payload unless there is a specific product requirement.

The Android app should retrieve authoritative message data from the API.

## Web client (PWA)

The web UI registers as a `platform: web` device: it subscribes through the
browser's PushManager with the key from `GET /push/vapid`, then posts the
subscription to `POST /devices`. Delivery, payload and policy are identical to
Android; browser endpoints (FCM/Mozilla autopush) are plain Web Push, so no
gateway is involved. The service worker answers the validation ping by relaying
the decrypted token to the page, which calls `/devices/{id}/validate`, and
fetches the message with the session cookie to render the notification text.

## Important

Do not make the Android application dependent on the ntfy application's UI or APIs.

UnifiedPush is the integration boundary.

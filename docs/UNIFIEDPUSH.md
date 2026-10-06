# UnifiedPush Reference

Verified against official documentation on 2026-10-06:

- https://unifiedpush.org/developers/spec/definitions/ (spec 3.x terminology)
- https://unifiedpush.org/developers/spec/android/ (Android spec AND_3.1.0)
- https://unifiedpush.org/developers/intro/ (server-side integration rules)
- https://docs.ntfy.sh/publish/ and https://docs.ntfy.sh/config/ (ntfy specifics)
- https://unifiedpush.org/users/gateway/ and https://unifiedpush.org/users/distributors/ntfy/

Items marked **[verify]** are not fully pinned down by those pages and must be
confirmed against the referenced project at implementation time.

**Live verification (2026-10-06, ntfy.example.com):** all three
`[verify]` items were answered via live integration tests
(`internal/push/live_test.go`, `internal/httpapi/integration_test.go`):

1. **TTL/Urgency on ntfy UP topics**: accepted (and otherwise ignored) — the
   RFC 8030 header set posts cleanly to ntfy; sending through webpush-go
   (`Content-Encoding: aes128gcm`, TTL, Urgency, `?up=1`) returns 200.
2. **RFC 8291 through ntfy**: verified end-to-end — our sender's aes128gcm
   body survives ntfy, gets base64 re-encoded for the `up=1` flag, and
   decrypts to the original payload (test roundtrip).
3. **Invalid-topic send semantics**: ntfy returns **2xx for any topic name**
   — never 404/410. Our sender's dead-endpoint revocation cannot fire on
   ntfy status codes; cleanup is registration-driven (endpoint replacement)
   or distributor-driven (30-day no-ack unregister).
4. Additional finding: webpush-go pads to the maximum record, so every push
   is ~4 KiB on the wire regardless of payload size — the SSE/JSON stream
   re-encodes it as `"encoding":"base64"`. Android's connector decrypts the
   padded record identically; payload size limits are unaffected.

Not verified here (needs a device): the ntfy Android distributor's
decryption and notification behavior itself.

## 1. Roles in a UnifiedPush system

| UnifiedPush role | In this project |
| --- | --- |
| Application Server | The Go chat server (this repo) |
| End User Application (User Agent) | Android chat client, web client |
| Connector Library | `android-connector` (official Kotlin/Java library; Flutter/RN wrappers exist) used inside the Android app |
| Push Distributor | The ntfy Android app installed on the user's device |
| Push Server (Provider) | Our self-hosted ntfy server |
| Push Gateway / Rewrite Proxy | Not needed: we speak Web Push directly and can reach ntfy |

Definitions page: the only standardized segment is Application Server ↔ Push
Server. Push Server ↔ Distributor and Distributor ↔ App are push-provider
specific (ntfy's own WebSocket/SSE stream and the UP Android broadcasts).

## 2. Protocol stack

```
Go server  --Web Push (RFC8030 + RFC8291 + RFC8292)-->  ntfy server
ntfy server --ntfy internal protocol (WebSocket/SSE)-->  ntfy Android app
ntfy app  --UP Android broadcasts (spec AND_3.1.0)-->  chat app
```

App Server → Push Server is Web Push:

- RFC 8030 — HTTP protocol (`POST <endpoint>`, TTL/Urgency headers, capability URL endpoint).
- RFC 8291 — message encryption. Payload must have `Content-Encoding: aes128gcm`.
  Beware libraries implementing only the 4th draft of RFC8291 — they are
  incompatible. webpush-go v1.4.0 implements the current RFC (verified in
  source + live roundtrip).
- RFC 8292 — VAPID (ES256 over a P-256 key). The public key format the Android
  side expects: P-256 SEC 1 uncompressed point, base64url (RFC 7515), exactly
  87 bytes encoded.

## 3. What our backend implements

### 3.1 Expose our VAPID public key

A public, authenticated endpoint returning the server's VAPID public key
(base64url, 87 bytes as above). The Android app passes it to the distributor
during registration (`vapid` extra). Some distributors refuse registration
without VAPID (`VAPID_REQUIRED` failure); ntfy does not enforce it, but send it
always — it is what makes the endpoint a Web Push subscription.

### 3.2 Accept device push registration from the app

The app, after the connector registers it with the distributor, receives:

- `endpoint` — capability URL of the push resource (≤ 1000 bytes, roughly 160
  bits of entropy on well-formed servers).
- `public key` (`p256dh`) — the device's RFC 8291 encryption public key.
- `auth secret` (auth) — RFC 8291 authentication secret.

The connector libraries generate these; the app only relays
endpoint + key + auth to us. Persist them per device/session via a
`POST /devices/{id}/push` style authenticated API. Store keys in the database
like other credentials (secret handling rules apply).

### 3.3 Validate a new registration before trusting it

Per the UnifiedPush server guidance: a channel should be validated before real
notifications flow, otherwise a web push server can be used as a DoS
amplification vector (someone registers arbitrary endpoints against our
account). Method:

1. On registration, send one ping-style push containing a fresh random token
   generated by us.
2. The app reads it via the connector, and sends the token back to us on a
   second authenticated call.
3. Only then mark the registration as validated/active.

### 3.4 Send a push message

On message events (per the policy in `PUSH.md`):

1. Serialize minimal payload (only identifiers and type — see `PUSH.md`).
2. Encrypt with RFC 8291 using the stored `p256dh` key and auth secret.
   Constraints from the Android spec: encrypted payload 1–4096 bytes, so the
   cleartext stays ≤ 3993 bytes. Our payload is well below that.
3. `POST <endpoint>` with `Content-Encoding: aes128gcm` (and a TTL header; the cache behavior is ultimately the push server's). Verified live: ntfy accepts this header set on UP topics.
4. Outbound network policy (SSRF): for every push, resolve the endpoint host
   and reject non-global addresses — at minimum RFC 1918 IPv4 private, RFC 4193
   IPv6 ULA, plus link-local/broadcast recommended. Because our deployments use
   a private ntfy server, we need a configuration allow-list of push hosts that
   exempts them from the private-address rejection. Check against the resolved
   IP on every send, not just at registration.

### 3.5 Failure handling

- Distributor-side registration failures seen by the app: `INTERNAL_ERROR`,
  `NETWORK`, `ACTION_REQUIRED`, `VAPID_REQUIRED`. On any of these the app must
  generate a new token and re-register; treat a stale registration as dead.
- Sending: retry only transient HTTP (5xx/timeouts) briefly; on 404/410-ish
  removal signals from the push server, delete the registration. Verified
  live: ntfy never 404s — any topic accepts and caches — so with ntfy the
  sender cannot detect dead endpoints from status codes (see §7 checklist
  note for what replaces this).
- Long-lived registrations need renewal: the Android side re-registers
  periodically; when the app delivers a new endpoint we replace the old one.

## 4. ntfy specifics (our push server)

- UnifiedPush topics start with `up*` — endpoints handed out by the ntfy
  Android app are ordinary ntfy topic URLs whose topic begins with `up`
  (random). Topic names visible to us: treat the whole endpoint URL opaquely;
  never parse or reuse it beyond POSTing.
- Publishing to a UnifiedPush topic: `POST <endpoint>` with header
  `X-UnifiedPush: 1` (aliases `unifiedpush` or `up`, also usable as a query
  parameter). This disables the Firebase path and makes ntfy auto-base64
  encode binary bodies.
- ACL: the app server needs **anonymous write-only** access to `up*` topics:
  `ntfy access '*' 'up*' write-only` (or per topic). Subscribers must be able
  to read them.
- If `visitor-subscriber-rate-limiting: true` is configured on ntfy, publishing
  to a UnifiedPush topic with no registered rate visitor returns
  **HTTP 507 Insufficient Storage**. This is a deployment config interaction,
  not a code path.
- Message limit: 4096 bytes per message general ntfy limit; matches the UP
  Android spec maximum.
- ntfy also has a built-in Matrix push gateway (`/_matrix/push/v1/notify`) —
  we do not use it; we POST directly to the topic endpoint.
- Our server must not depend on the ntfy web UI or its admin API.

## 5. Android side (reference only — android-connector lives in the mobile repo)

Broadcast protocol AND_3.1.0 summary, useful for debugging and for defining the
app↔server API contract:

- Register: `org.unifiedpush.android.distributor.REGISTER`, extras `token`
  (connector-generated, ≤ 100 bytes, UUIDv4 suggested, must be unguessable),
  optional `vapid`, `message` (registration description ≤ 100 bytes, shown in
  distributor UI). Identity pinning via `FLAG_SHARE_IDENTITY` on API 34+.
- Responses: `NEW_ENDPOINT` (extras `token`, `endpoint`, optional `id` — ack
  with `MESSAGE_ACK` within 30 s or the distributor may drop the endpoint),
  `REGISTRATION_FAILED`, `TEMP_UNAVAILABLE` (retry; may carry a fallback
  `useDistributor`; ignore cyclic fallback chains).
- Message: `org.unifiedpush.android.connector.MESSAGE`, extras `token`,
  `bytesMessage` (RFC 8291 ciphertext, 1–4096 bytes), optional `id`. The
  distributor holds the app in foreground importance ~5 s so it may notify.
  Urgency filtering follows RFC 8030 §5.3 (very-low → high based on power,
  Wi-Fi, battery state).
- Unregister: `org.unifiedpush.android.distributor.UNREGISTER` with `token`;
  app must discard the token regardless of acknowledgement; distributor sends
  `org.unifiedpush.android.connector.UNREGISTERED`.
- Ack/lifetime: no acks for ~30 days after the last `NEW_ENDPOINT` ping lets
  the distributor unregister silently; apps therefore re-register on startup
  and periodically.
- Registration limits per app are allowed; distributors should be willing to
  host > 1000 registrations per app (ours = per user, which trivially satisfies
  this).
- Connector: `android-connector` handles key generation, decryption, and the
  broadcast plumbing; embedded-FCM distributor exists as a Play-services
  fallback but is out of scope for this project (no FCM in backend, and the
  Android app targets F-Droid-style installs).

## 6. Decisions

- We implement plain Web Push (RFC 8030/8291/8292) against the registered
  endpoint — not the Matrix gateway format, not ntfy-specific JSON bodies.
- Payload stays identifiers-only per `PUSH.md`; nothing sensitive rides the
  push channel even though RFC 8291 already encrypts it end-to-end.
- The `p256dh`/`auth` secrets and endpoint URL are database-stored device data,
  not secrets in config.

## 7. Implementation checklist (backend)

Tracked in `docs/IMPLEMENTATION.md` (one-pass checklist). The `[verify]` items
were resolved by the 2026-10-06 live run (see the section at the top). One
consequence lives in code: `push.Sender` keeps the 404/410 revocation branch —
harmless with ntfy, correct against strict RFC 8030 push services.
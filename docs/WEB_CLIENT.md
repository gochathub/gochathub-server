# Web Client Compatibility Requirements

Based on a review of [Avian-Template](https://github.com/daemon-bixia/Avian-Template)
(commit reviewed 2026-10-06) as the intended baseline for the web client.

## What Avian-Template is

Vue 3 + Pinia + vue-router + Tailwind 4 UI template (Vite, yarn). All data is
mocked (`src/store/defaults.ts`); there is no HTTP client, no WebSocket client,
and no auth logic. Every integration path listed here is ours to build.

Its `src/types.ts` model is the reference for what the UI expects from the
backend.

## Decisions made during review

| Avian feature | Decision |
| --- | --- |
| Voice calls (dialer, call list, call modals) | Strip from fork. No WebRTC/signaling in backend scope. |
| Per-message `sent/delivered/read/waiting` state | Implement: per-message receipts (`message_receipts`), replacing the room-cursor-only design sketched in `DATABASE.md`. ADR needed. |
| Pinned message per conversation | Implement server-side: pinned message in API + WS events. |
| `broadcast` conversation type | Map onto existing `group` rooms (announcement-style usage is product convention, not a room type). |
| Contact-centric model | Add a `contacts` table + API (add/remove, sync via WS). This is a new server feature; Android gets it too. |
| Signup and password-reset pages | Strip. Accounts are CLI-managed; no self-registration, no email flows. |
| Avatars | Avatar upload via the attachment flow, with a Gravatar fallback and monogram when no avatar exists. |
| Link previews (`previewData`) | Strip. Client linkifies URLs only; no preview fetching client- or server-side. |
| Privacy toggles (lastSeen, readReceipt, joiningGroups, privateMessages) | Implement as a JSONB `users.preferences` column with `GET/PATCH /users/me/preferences`. |
| Numeric IDs / email login field | Frontend adapts to server: string IDs everywhere, login by username. |

## New server requirements

All are architecture decisions recorded in `DECISIONS.md` — no restatement here:

- ADR-009 — per-message delivery/read receipts (`message_receipts`).
- ADR-010 — contacts table (add/remove/list, WS sync).
- ADR-011 — avatars: attachment upload with server-side Gravatar fallback.
- ADR-013 — privacy toggles as JSONB `users.preferences`.
- Message pinning (`/rooms/{id}/pin`) and `GET /users/search` — already specified in `api/openapi.yaml`.

## Design questions — resolved

All five original design questions are recorded as ADR-015 through ADR-018; room avatars were already ADR-011.
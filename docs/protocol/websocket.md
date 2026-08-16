# SOBH WebSocket Protocol v1

The WebSocket carries everything that has to feel instant: sending and
receiving messages, read receipts, typing indicators, presence and call
signalling. Anything the socket can do, the REST API can also do — the socket
is an optimisation, never the only path.

**Endpoint:** `wss://<host>/ws`
**Protocol version:** `1` (`protocol_version` query parameter)

---

## 1. Handshake

Authentication happens *before* the upgrade, so a bad token produces an
ordinary HTTP error envelope rather than a socket that immediately closes.

The access token may be supplied two ways:

| Method | Form | Use |
|---|---|---|
| Query parameter | `wss://host/ws?token=<access_token>` | Native apps |
| Subprotocol | `Sec-WebSocket-Protocol: sobh.auth.<access_token>` | Browsers, which cannot set headers on a handshake |

```
wss://api.sobh.app/ws?token=eyJhbGci...&protocol_version=1
```

A token that is expired, revoked, or signed with an unknown key is rejected
with `401` and the usual error envelope. If `protocol_version` names a version
this server does not speak, the handshake fails with
`UNSUPPORTED_PROTOCOL_VERSION` — the client must not retry until it upgrades.

---

## 2. Frame format

Every frame in both directions is a single JSON object.

```jsonc
{
  "id": "c7f2…",        // client correlation id; echoed on the response
  "event": "message.send",
  "payload": { },        // event-specific
  "sync_seq": 4821,      // server → client only: your event-log position
  "error": {             // server → client only, on failure
    "code": "CHAT_PERMISSION_DENIED",
    "message": "You cannot post in this chat"
  }
}
```

- `id` is chosen by the client and echoed on the acknowledgement. Server-initiated
  events have no `id`.
- `error.code` is drawn from the same vocabulary as the REST API (§66), so a
  client has one error table to maintain.
- Frames are capped at **256 KiB**. Media never travels over the socket; it is
  uploaded to object storage and referenced by `media_id`.

---

## 3. Connection lifecycle

```
client                                  server
  │  ── handshake (token) ─────────────▶ │
  │  ◀───────────────── connected ────── │   sync_cursor, server_seq
  │                                      │
  │  ── sync.request { cursor } ───────▶ │   only if cursor < server_seq
  │  ◀───────────────── sync.batch ───── │
  │  ── sync.ack { cursor } ───────────▶ │
  │                                      │
  │  ══════════ live traffic ══════════  │
  │                                      │
  │  ◀───────────── ping (control) ───── │   every 25s
  │  ── pong (control) ────────────────▶ │
```

### `connected`

The first frame the server sends. It is the client's cue to decide whether it
needs to resynchronise.

```json
{
  "event": "connected",
  "payload": {
    "user_id": "…", "device_id": "…",
    "protocol_version": 1,
    "sync_cursor": 4780,
    "server_seq": 4821,
    "heartbeat_ms": 25000
  }
}
```

`sync_cursor` is where the server last saw this device; `server_seq` is the
head of its event log. When they differ, the client has missed events — see §5.

### Heartbeat

The server sends a WebSocket **ping** control frame every 25 s and expects the
pong within 60 s. Clients should also send an application-level `ping` when the
platform suspends timers (mobile background), since the OS may pause the
control-frame machinery.

### Close codes

| Code | Meaning | Client action |
|---|---|---|
| 1000 | Normal closure | Reconnect if the app is still foregrounded |
| 4000 | Server shutting down | Reconnect with backoff; another node will accept |
| 4001 | Access token expired | Refresh the token, then reconnect |
| 4002 | Session replaced | Do **not** reconnect; the session was signed out |
| 4003 | Protocol error | Fix the client; do not retry blindly |
| 4004 | Slow consumer | Reconnect and resume from cursor |
| 4005 | Rate limited | Back off before reconnecting |

### Reconnection

Exponential backoff with jitter, starting at 1 s and capped at 60 s:

```
delay = min(60s, 1s × 2^attempt) × random(0.5, 1.5)
```

Reset the attempt counter after a connection survives 30 s. On 4002 the client
must stop and return the user to the sign-in screen.

---

## 4. Client → server events

| Event | Payload | Acknowledged with |
|---|---|---|
| `ping` | `{}` | `pong` |
| `message.send` | `{ chat_id, client_message_id, type, content, entities?, payload?, reply_to_id?, attachments?, mentions?, is_silent? }` | `message.sent` |
| `message.read` | `{ chat_id, seq }` | `ack` |
| `message.edit` | `{ message_id, content, entities? }` | `ack` |
| `message.delete` | `{ message_id }` | `ack` |
| `message.react` | `{ message_id, emoji }` | `ack` |
| `typing.start` / `typing.stop` | `{ chat_id }` | none (fire-and-forget) |
| `chat.subscribe` / `chat.unsubscribe` | `{ chat_id }` | `ack` |
| `sync.request` | `{ cursor, limit? }` | `sync.batch` |
| `sync.ack` | `{ cursor }` | none |

### `message.send` and idempotency

`client_message_id` is a client-generated UUID and is **required**. It is the
idempotency key: retrying a send with the same value returns the original
message instead of creating a second one. This is what makes the offline outbox
safe — a client may retry as often as it likes without risking duplicates.

The acknowledgement is what promotes the client's optimistic row from `PENDING`
to `SENT`:

```json
{
  "id": "c7f2…",
  "event": "message.sent",
  "payload": {
    "client_message_id": "…",
    "message_id": "…",
    "chat_id": "…",
    "seq": 1043,
    "created_at": "2026-08-16T09:14:22.481Z"
  }
}
```

### `chat.subscribe`

Only needed for **large chats** (see §5). Membership is verified before the
subscription opens, so a client cannot listen to a chat by guessing its id.

---

## 5. Server → client events

| Event | Payload | Durable |
|---|---|---|
| `message.new` | `{ chat_id, message }` | yes |
| `message.edited` | `{ chat_id, message }` | yes |
| `message.deleted` | `{ chat_id, message_id, seq }` | yes |
| `message.read` | `{ chat_id, user_id, last_read_seq }` | no |
| `message.reaction` | `{ chat_id, message_id, user_id, emoji, added }` | no |
| `typing.start` / `typing.stop` | `{ chat_id, user_id }` | no |
| `presence.online` / `presence.offline` | `{ user_id, online }` | no |
| `chat.updated` | `{ chat }` | yes |
| `call.incoming` / `call.accepted` / `call.rejected` / `call.ended` | `{ call, … }` | partly |
| `notification.new` | `{ notification }` | yes |

**Durable** events are written to the per-user event log and carry a `sync_seq`.
If the socket drops, they are recovered by cursor. **Non-durable** events —
typing, presence, read receipts — are live signals only: a missed one costs
nothing and is never replayed.

### Two delivery paths

Chats behave differently above and below a fan-out threshold (1000 members):

- **Small chats** (private chats, ordinary groups). Each recipient gets a row
  in their event log and the event is pushed to their user subject. Recovery is
  purely by cursor; no subscription is needed.
- **Large chats** (channels with tens of thousands of subscribers). Writing a
  log row per subscriber per post does not scale, so these are pull-based: the
  post is broadcast on the chat subject for clients that currently have the chat
  open (`chat.subscribe`), and everyone else reconciles by comparing their
  per-chat cursor against `chats.last_seq` from the chat list.

This is the one place where the two paths are visible to a client, and it is
why `chat.subscribe` exists.

---

## 6. Synchronisation

Each device has a `sync_cursor`: the highest event sequence it has confirmed.
The server keeps a monotonic per-user counter, so sequences have no gaps and
strictly increase.

```
device cursor: 4780        server head: 4821
                └──────── 41 missed events ────────┘
```

On reconnect:

1. Read `sync_cursor` and `server_seq` from the `connected` frame.
2. If `sync_cursor < server_seq`, send `sync.request { cursor: sync_cursor }`.
3. Apply the returned `sync.batch` in sequence order.
4. Send `sync.ack { cursor }` with the highest applied sequence.
5. Repeat while `has_more` is true.

The cursor is only advanced by `sync.ack`, so an event is never dropped because
a client crashed mid-apply — it will simply be delivered again. Handlers must
therefore be **idempotent**: apply by `(chat_id, seq)` and ignore anything
already stored.

Events are pruned once every device on the account has acknowledged them, with
a seven-day grace period for devices that have been offline. A device that has
been away longer than that receives a cursor reset and performs a full
resynchronisation from the REST API instead.

---

## 7. Rate limits and backpressure

Message sends are limited per user (default 100/minute) and the socket returns
`RATE_LIMITED` with a `retry_after`. A client whose outbound buffer the server
cannot drain is closed with **4004**: reconnecting and resuming from the cursor
is cheaper than holding memory for a stalled connection, and no data is lost.

---

## 8. Scaling model

Nodes are interchangeable and hold no session state. When a connection opens,
its node subscribes to that user's subject on the message bus; whichever node
handles a send publishes there, and the subscribing node delivers it.

```
 user A ──▶ WS node 1 ──┐                ┌── WS node 3 ──▶ user B
                        ├── NATS ────────┤
 user C ──▶ WS node 2 ──┘                └── WS node 3 ──▶ user D
```

Realtime delivery across the bus is at-most-once by design. The durable record
is the event log in PostgreSQL, and cursor-based recovery is what makes
at-most-once delivery acceptable: a node dying loses frames, never messages.

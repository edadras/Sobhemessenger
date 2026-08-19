# SOBH — Messenger & Media Platform

An independent, self-hosted messenger, media and news platform. Every part of
the system — API, database, object storage, realtime fabric and search — runs on
infrastructure the operator controls. There is no dependency on any third-party
messaging network.

```
Flutter apps (Android · iOS · Web)
        │  HTTPS + WebSocket
        ▼
   SOBH API gateway
        │
        ├── Auth · Users · Messaging · Media · Groups
        ├── Channels · Communities · Stories · Calls
        └── News · Search · Notifications · Admin
                │
                ├── PostgreSQL      durable state
                ├── Redis           presence, rate limits, caches
                ├── NATS JetStream  realtime fan-out + background jobs
                ├── MinIO           media objects
                └── OpenSearch      full-text search
```

---

## Repository layout

```
apps/mobile/        Flutter client (Android, iOS, Web)
apps/admin/         Flutter Web admin panel
backend/            Go modular monolith
database/           Migrations and seeds
protocol/           OpenAPI and WebSocket contracts
infrastructure/     Docker, Kubernetes, nginx, monitoring
docs/               Architecture, API, protocol, security, deployment
scripts/            Operational scripts
```

---

## Running it

```bash
cp .env.example .env
#  Generate real secrets before anything else:
#    openssl rand -base64 48   → JWT_SIGNING_KEYS
#    openssl rand -base64 48   → PHONE_HASH_PEPPER

docker compose -f infrastructure/docker/docker-compose.yml up -d
```

That brings up PostgreSQL, Redis, NATS, MinIO, OpenSearch, Prometheus, Grafana,
Loki and nginx, applies the migrations, and starts the API and worker.

| Service | URL |
|---|---|
| API | http://localhost:8080 |
| Metrics | http://localhost:9090/metrics |
| Grafana | http://localhost:3000 |
| MinIO console | http://localhost:9001 |

### Backend without Docker

```bash
cd backend
go run ./cmd/migrate -dir ../database/migrations up
go run ./cmd/api
```

### Tests

```bash
cd backend
go test ./...                                      # unit tests only
SOBH_TEST_POSTGRES_DSN=postgres://… go test ./...  # + integration and end-to-end

cd ../apps/mobile
flutter gen-l10n && dart run build_runner build --delete-conflicting-outputs
flutter analyze --fatal-infos && flutter test
```

Integration tests skip themselves when `SOBH_TEST_POSTGRES_DSN` is unset, so the
default run needs no database. The end-to-end suite additionally starts an
in-process NATS server and an in-memory Redis, so it needs nothing else
installed. Set `SOBH_TEST_LOG=1` to see the server's own logs while a test runs.

### Load test

```bash
# Against the compose stack, or any environment with SMS_ECHO_CODES on.
k6 run -e STAGE=smoke -e ACCOUNT_POOL=20 scripts/loadtest/messaging.js
k6 run -e STAGE=10k  -e BASE_URL=https://staging.example scripts/loadtest/messaging.js
```

Stages are `smoke`, `10k`, `50k` and `100k`, matching the §78 ramp. The
thresholds are the §79 targets, so k6 exits non-zero when one is crossed —
the run is a gate, not a traffic generator.

The harness seeds its own accounts, which needs `SMS_ECHO_CODES=true`.
Configuration refuses that setting in production, so the test cannot be pointed
at real users by accident.

---

## Design decisions worth knowing

**One conversation container.** Private chats, groups and channels are all rows
in `chats`, with `groups` and `channels` as extension tables and `chat_members`
as the single membership table. Messaging, sync, search and moderation therefore
have exactly one code path rather than three that drift apart.

**Sequences, not timestamps.** Every message gets a monotonic per-chat `seq`,
allocated under a row lock. Ordering, pagination and sync all key off it, so
clock skew between nodes cannot reorder a conversation.

**Idempotent sends.** Every send carries a client-generated
`client_message_id`. Retrying returns the original message instead of creating a
duplicate — which is what makes the offline outbox safe to retry indefinitely.

**Two delivery paths.** Chats below 1000 members get a per-recipient event log
and resume purely by cursor. Above that — a channel with tens of thousands of
subscribers — writing a log row per subscriber per post does not scale, so those
are pull-based: broadcast live to clients with the chat open, reconciled by
comparing per-chat cursors. See `docs/protocol/websocket.md`.

**Realtime is best-effort; the log is durable.** Cross-node fan-out over NATS is
at-most-once. That is acceptable precisely because the event log in PostgreSQL
is the record of truth and clients resume from a cursor: a node dying loses
frames, never messages.

**No invented cryptography.** Passwords use Argon2id, tokens are random values
stored as SHA-256 hashes, contact discovery uses an HMAC pepper. End-to-end
encryption for secret chats is X3DH + Double Ratchet performed on the device;
the server stores public key material and opaque ciphertext only.

---

## Implementation status

The specification describes the complete platform. This repository implements
it in the staged order the specification itself prescribes, so that every stage
is tested and usable before the next begins. Nothing below is mocked or
stubbed: what is marked complete is wired to a real database and covered by
tests.

### Complete

| Area | What exists |
|---|---|
| **Database** | 97 domain tables covering every area of the specification, plus the migration ledger. 18 migrations with working `down` steps, verified by applying and reverting all of them against a live PostgreSQL — 98 tables to 1 and back. |
| **Backend foundation** | Config from environment with production validation, structured logging with redaction, PostgreSQL pool with transaction helpers, Redis, NATS JetStream, MinIO, OpenSearch, Prometheus metrics, health/readiness probes, migration runner with checksums and advisory locking. |
| **HTTP layer** | Single response envelope, ~60 stable error codes, request id, real-IP resolution behind trusted proxies, security headers, CORS, per-route metrics, panic recovery, timeouts. |
| **Authentication** | OTP with sliding-window limits that fail closed, phone normalisation (E.164, Persian and Arabic digits), JWT with key rotation, refresh-token rotation with replay detection, two-step verification, session and device management, login history. |
| **Messaging** | Chats and membership, permissions with per-member overrides, send/edit/delete/react/read, drafts that follow you between devices without a refresh overwriting one in progress, per-chat sequences, per-user event log, unread and mention counters, slow mode, edit windows, clearing a conversation for yourself or for both sides, and chat folders as saved filters. |
| **Realtime** | WebSocket with authenticated handshake, protocol versioning, heartbeats, ack correlation, resume-from-cursor, slow-consumer eviction, cross-node routing, presence. |
| **Media** | Resumable presigned multipart uploads, magic-byte validation against the declared MIME, ClamAV scanning, image variants with BlurHash, ffmpeg video renditions and posters, Opus normalisation and waveforms for voice. |
| **Groups & channels** | Creation, named role bundles resolved between the chat's defaults and a member's own overrides, ownership transfer, promotion and demotion with rank checks, invite links with limits and expiry that can be redeemed as well as issued, join requests, distinct-viewer counting, per-post statistics, public directory, forum topics that open as conversations of their own with per-topic reading and unread counts, channel post signatures, and comments as replies in a linked discussion group. |
| **Communities** | Rooms grouped into sections, starter rooms on creation, membership cascading into default rooms. |
| **Stories & polls** | Privacy scopes evaluated per viewer in SQL with deny-list override, views, reactions, close friends with an editable list, 24-hour expiry; polls with single/multiple choice, quizzes, anonymity that is never lifted afterwards, a named voter list when it is not anonymous, and transactional vote replacement. |
| **Calls** | WebRTC signalling that relays SDP and ICE without parsing them, one session row per leg recording what it negotiated and the network it was on, participant lifecycle, media state, ephemeral HMAC TURN credentials. |
| **News** | Editorial state machine with publish as a separate permission, categories with per-locale names, authors, tags, feed modes (latest, popular with time decay, following, breaking) plus a banner for the single story breaking now, bookmarks, follows, view counting. |
| **Search** | OpenSearch indices built for Persian and Arabic — letter folding, zero-width non-joiner handling, digit folding — with prefix analyzers, member-scoped message search, and reindex from PostgreSQL. |
| **Notifications** | Settings, quiet hours, preview suppression, push token lifecycle, FCM HTTP v1 and APNs over HTTP/2, token retirement, collapse keys, paged breaking-news fan-out. |
| **Anti-spam** | A rolling score per subject fed by reports, blocks and rate-limit trips, weighted by what each signal is, decaying in the worker so it cannot ratchet. A restriction lapses by itself and stops only cold outreach: groups and anyone who has the account in their contacts keep working. |
| **Account recovery** | A two-step password that can be set, changed and removed — proving the current one first, so a live session cannot replace the factor that guards against a live session. Recovery is reachable from the moment somebody discovers they have forgotten it. A recovery email address that does nothing until a code proves it, revoked when the address changes, and answering identically whether or not an address is on an account. Completing a recovery clears the two-step password and every session with it. |
| **Admin** | RBAC-gated endpoints for users, reports, bans, flags, analytics and audit log; every mutation audited; bans revoke sessions immediately; Flutter Web panel with dashboard, users and their operator roles, reports, scoped bans, search reindexing, editorial queue, authors, categories and feature flags. |
| **Background work** | Durable job consumers with backoff and poison-message handling, media processing, push delivery, search indexing, and maintenance (OTP expiry, event-log pruning, story expiry, abandoned uploads, scheduled publishing). |
| **Infrastructure** | Docker Compose dev stack, distroless image, nginx edge config, Prometheus alerts derived from the §79 targets, Grafana provisioning, Kubernetes manifests with PDB/HPA/NetworkPolicy and backup CronJobs, encrypted backup and verified restore scripts, CI with lint, tests, reversible-migration check and image scanning. |
| **Protocol** | OpenAPI 3.1 covering every served endpoint, and a full WebSocket protocol document. A test walks the real router in both directions, so an undocumented route and a documented route that does not exist both fail the build. |
| **Contacts** | Discovery by HMAC digest under a server-published pepper — a phone number never leaves the device — with full and incremental sync, favourites, blocking in both directions, privacy resolved per viewer in SQL, and contact requests whose acceptance writes both address books at once. |
| **Secret chats** | The server's half of §24: a key directory and a mailbox. Prekeys are handed out exactly once under `FOR UPDATE SKIP LOCKED`, identity rotation clears stale keys and sessions, and acknowledged ciphertext is deleted rather than flagged. Opening an encrypted chat is idempotent on an ordered pair, so two devices racing land in one conversation. |
| **Secret chats on the device** | X3DH and the Double Ratchet through `libsignal_protocol_dart` (GPL-3.0, recorded per §84.19) — no construction is invented (§84.16–17). Private keys and ratchet state live in the iOS keychain and the Android keystore; the identity is generated once and published at launch so the device is reachable; prekeys top up when the server says they are low. Safety numbers are shown per device for out-of-band comparison, and signing out wipes every key. |
| **Flutter** | Clean-architecture foundation, design tokens, four locales with correct RTL, envelope-aware client with collapsed token refresh, WebSocket client with jittered backoff, Drift schema with a real offline outbox. Five-tab shell with per-tab navigation stacks; chats with media, voice notes and polls; contacts; groups and channels with member administration, invite links and join requests; stories with a composer and viewer; WebRTC calls; communities; the news feed with articles and bookmarks; search; notifications; profile and settings. |
| **Mobile media** | Presigned multipart upload straight to object storage, images with reserved aspect ratio, video posters, voice notes recorded in Opus and drawn with the server's waveform, and a readiness gate so nothing renders before it is scanned and processed. |
| **Mobile identity** | Editing the profile with partial updates, and claiming a username with availability checked as you type and the 30-day release hold explained before you rename rather than after. |
| **Mobile bots** | Registering a bot, issuing and revoking tokens with the secret shown once behind a dialog that cannot be dismissed by a stray tap, privacy mode, and webhook registration. |
| **Mobile organisation** | Long-press to copy, forward or pin; a sticker picker and store; scheduling with the queue and its failures visible; mute and archive. |
| **Mobile calls** | A real WebRTC peer connection over the signalling relay, with candidate buffering, per-call TURN credentials, mute, camera and speaker controls, and incoming calls caught above the whole navigator. |

### Verified, not asserted

Two independent measurements, both against real dependencies.

**The end-to-end suite** assembles the production router against a real
PostgreSQL, a real Redis protocol implementation and an in-process NATS with
JetStream, then signs in over HTTP, opens a chat, sends over a WebSocket and
resumes from a cursor.

**The k6 harness** runs against the assembled stack — the API binary,
PostgreSQL, Redis, NATS and MinIO, all four reporting healthy — driving mixed
REST and WebSocket traffic with think time.

| Path | End-to-end suite | k6 smoke | Budget (§79) |
|---|---|---|---|
| Message send (HTTP) | 57 ms | 5 ms | 200 ms |
| Send ack | 4 ms | 6 ms | 300 ms |
| Delivery to recipient | 4 ms | 4 ms | 500 ms |

Re-measured with the bot observer live on the send path, so the figures include
the cost of resolving bot recipients for every message rather than describing an
earlier build.

Both assert rather than report: the suite fails the build on a regression, and
k6 exits 99 when a threshold is crossed — verified by running it against a
deliberately impossible target and confirming it fails.

### The bot platform

**@sobhfather_bot** is a real bot you message, exactly as in Telegram. It asks
what to call your bot, asks for a handle, and hands you the token:
`/newbot` `/mybots` `/token` `/revoke` `/setname` `/setdescription` `/setabout`
`/setcommands` `/setprivacy` `/setinline` `/setjoingroups` `/deletebot`
`/cancel` `/help`. Registering is also an ordinary API, for anyone who would
rather script it.

BotFather runs inside the server rather than as a program holding a token,
because a BotFather token would create bots for anyone and read every bot its
caller owns. The database refuses to issue one, with a trigger rather than a
convention.

A bot **is** a user row carrying `is_bot`. That is the whole design: every
membership, permission, delivery and sync path already written works on a bot
unchanged, so there is no parallel implementation that can drift from the one
people actually use.

Four decisions worth stating, because each differs from the obvious choice:

- **The token goes in the `Authorization` header, not the URL.** Telegram puts
  it in the path, which is convenient and also writes the credential into every
  access log, proxy trace and browser history along the way. A header costs the
  bot author nothing and does not.
- **Only a SHA-256 hash is stored.** A token is shown once, at issue. A lost
  one is replaced rather than recovered, and a database dump yields nothing
  usable. Five may be live at once so a rotation is not an outage.
- **Privacy mode is enforced where updates are selected, not where they are
  delivered.** The bot is never handed a message it is not entitled to see and
  then trusted to discard it.
- **Updates are a durable queue, not a fan-out buffer.** A bot confirms what it
  has processed by asking for the next offset, so one that restarts mid-batch
  resumes rather than losing the batch. The backlog is bounded per bot, so an
  abandoned bot loses its oldest updates instead of filling the table.

Webhooks are validated through the same SSRF guard as link unfurling — https
only, public addresses only, checked in the dialer rather than before it — and
every delivery is signed with a per-registration HMAC-SHA256 secret.

Bots are interfaces, not just correspondents: **inline keyboards** under a
message, **callback queries** when one is tapped, and **inline mode** — typing
`@somebot pizza` in any chat asks that bot for results without adding it there.
A tap must name a button that is really on that message and come from someone
in the chat, so a bot switching on `data` cannot be driven by a stranger. In
inline mode the bot is told the query and who asked, and deliberately not where
they are typing; nothing is sent until the person picks a result, and the
message is then sent by them, through the ordinary send path.

### Closed since the last release

Each of these had a schema and nothing else. All are now served, tested against
real PostgreSQL, and documented in `protocol/rest/openapi.yaml`:

| Feature | What it does now |
|---|---|
| Forwarding | Attribution survives a chain, and the source chat's membership is joined into the read so a guessable message id leaks nothing |
| Scheduled messages | Queued in their own table, published through the ordinary send path, exactly-once by idempotency key |
| Pinned messages | Route, permission check and a chat-wide event |
| Stickers | Sets, per-user installation with ordering, search by title, slug or emoji |
| Usernames | Claiming, with a 30-day hold on a released name so it cannot be squatted |
| Profile editing | Partial updates with rune-counted limits |
| Archived and muted chats | Per member, so archiving a group does not archive it for everyone |
| Link previews | OpenGraph unfurling behind the SSRF guard, with a shared cache that also caches failures |
| Location messages | Fixed points, venues, and live location that updates the message rather than posting a movement log. The device keeps a share moving for as long as it runs — across a restart, since closing the app is not ending the share — and the sender can stop it before its deadline |
| Contact messages | Name and one number, picked from the address book — not the whole record |
| Inline keyboards | Buttons under a bot's message, and the taps they produce |
| Inline mode | `@bot query` from any chat's compose box, without the bot joining it — the results appear above the composer and the chosen one is sent by the person, not the bot |

Scheduling turned out to be the interesting one. Migration 0004 had reserved
`messages.scheduled_at`, but `messages.seq` is `NOT NULL` and a seq-less row
would sort to the top of `ORDER BY seq DESC` — an unsent draft at the head of
every member's history. Migration 0011 gives a queued post its own table, so a
row in `messages` keeps meaning "in the conversation".

### Still not built

**A local store for encrypted history.** Secret chats work end to end, but the
decrypted text lives in memory for as long as the conversation is open and
nowhere else — closing the app loses the history. The local Drift database is
not encrypted at rest, and writing plaintext into it would move the message from
a place the operating system protects to a file any process with storage access
can read. Keeping history without giving that up means an encrypted local store
(SQLCipher under Drift, keyed from the platform keystore), which is a change to
the whole local database rather than to this one feature.

**Load testing at scale (§22).** The harness implements the §78 ramp to 500k
concurrent, but running it needs a deployed cluster. What is measured above is
a single process: it bounds latency, not capacity.

`docs/architecture/roadmap.md` carries the per-stage detail.

## Documentation

| Document | Contents |
|---|---|
| `docs/protocol/websocket.md` | Frame format, events, sync, reconnection, scaling |
| `protocol/rest/openapi.yaml` | REST contract: all 180 operations across 147 paths, kept in step with the router by a test |
| `docs/deployment/README.md` | Running it: configuration, secrets and rotation, Kubernetes, migrations, backup, disaster recovery, and what each alert means |
| `docs/architecture/roadmap.md` | Per-stage delivery status, and the trade-offs taken deliberately |
| `docs/security/third-party-licences.md` | Every direct dependency and its licence (§84.19), including the one that constrains distribution |

Not yet written: a threat model, and prose descriptions of the database and
sync models. Those systems are built and covered by tests —
`docs/protocol/websocket.md` describes the sync protocol — but the explanatory
documents do not exist, and an earlier version of this table listed them as
though they did.

---

## Contributing rules

1. Every schema change is a migration with a working `down`.
2. No secret is ever committed; configuration comes from the environment.
3. No feature is "done" without tests and error, loading and empty states.
4. No colour, spacing or string is hard-coded in a widget.
5. No cryptographic primitive is invented; only reviewed constructions.
6. Breaking protocol changes require a version bump.

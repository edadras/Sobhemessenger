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
| **Database** | 74 tables covering every domain in the specification. Up **and** down migrations, verified by applying and reverting against a live PostgreSQL. |
| **Backend foundation** | Config from environment with production validation, structured logging with redaction, PostgreSQL pool with transaction helpers, Redis, NATS JetStream, MinIO, OpenSearch, Prometheus metrics, health/readiness probes, migration runner with checksums and advisory locking. |
| **HTTP layer** | Single response envelope, ~60 stable error codes, request id, real-IP resolution behind trusted proxies, security headers, CORS, per-route metrics, panic recovery, timeouts. |
| **Authentication** | OTP with sliding-window limits that fail closed, phone normalisation (E.164, Persian and Arabic digits), JWT with key rotation, refresh-token rotation with replay detection, two-step verification, session and device management, login history. |
| **Messaging** | Chats and membership, permissions with per-member overrides, send/edit/delete/react/read/draft, per-chat sequences, per-user event log, unread and mention counters, slow mode, edit windows. |
| **Realtime** | WebSocket with authenticated handshake, protocol versioning, heartbeats, ack correlation, resume-from-cursor, slow-consumer eviction, cross-node routing, presence. |
| **Media** | Resumable presigned multipart uploads, magic-byte validation against the declared MIME, ClamAV scanning, image variants with BlurHash, ffmpeg video renditions and posters, Opus normalisation and waveforms for voice. |
| **Groups & channels** | Creation, roles with per-member overrides, ownership transfer, rank-checked member administration, invite links with limits and expiry, join requests, distinct-viewer counting, per-post statistics, public directory. |
| **Communities** | Rooms grouped into sections, starter rooms on creation, membership cascading into default rooms. |
| **Stories & polls** | Privacy scopes evaluated per viewer in SQL with deny-list override, views, reactions, close friends, 24-hour expiry; polls with single/multiple choice, quizzes, anonymity and transactional vote replacement. |
| **Calls** | WebRTC signalling that relays SDP and ICE without parsing them, participant lifecycle, media state, ephemeral HMAC TURN credentials. |
| **News** | Editorial state machine with publish as a separate permission, categories with per-locale names, authors, tags, feed modes (latest, popular with time decay, following, breaking), bookmarks, follows, view counting. |
| **Search** | OpenSearch indices built for Persian and Arabic — letter folding, zero-width non-joiner handling, digit folding — with prefix analyzers, member-scoped message search, and reindex from PostgreSQL. |
| **Notifications** | Settings, quiet hours, preview suppression, push token lifecycle, FCM HTTP v1 and APNs over HTTP/2, token retirement, collapse keys, paged breaking-news fan-out. |
| **Admin** | RBAC-gated endpoints for users, reports, bans, flags, analytics and audit log; every mutation audited; bans revoke sessions immediately; Flutter Web panel with dashboard, users, reports, editorial queue and feature flags. |
| **Background work** | Durable job consumers with backoff and poison-message handling, media processing, push delivery, search indexing, and maintenance (OTP expiry, event-log pruning, story expiry, abandoned uploads, scheduled publishing). |
| **Infrastructure** | Docker Compose dev stack, distroless image, nginx edge config, Prometheus alerts derived from the §79 targets, Grafana provisioning, Kubernetes manifests with PDB/HPA/NetworkPolicy and backup CronJobs, encrypted backup and verified restore scripts, CI with lint, tests, reversible-migration check and image scanning. |
| **Protocol** | OpenAPI 3.1 for the implemented surface and a full WebSocket protocol document. |
| **Contacts** | Discovery by HMAC digest under a server-published pepper — a phone number never leaves the device — with full and incremental sync, favourites, blocking in both directions, and privacy resolved per viewer in SQL. |
| **Secret chats** | The server's half of §24: a key directory and a mailbox. Prekeys are handed out exactly once under `FOR UPDATE SKIP LOCKED`, identity rotation clears stale keys and sessions, and acknowledged ciphertext is deleted rather than flagged. |
| **Flutter** | Clean-architecture foundation, design tokens, four locales with correct RTL, envelope-aware client with collapsed token refresh, WebSocket client with jittered backoff, Drift schema with a real offline outbox. Five-tab shell with per-tab navigation stacks; chats with media, voice notes and polls; contacts; groups and channels with member administration, invite links and join requests; stories with a composer and viewer; WebRTC calls; communities; the news feed with articles and bookmarks; search; notifications; profile and settings. |
| **Mobile media** | Presigned multipart upload straight to object storage, images with reserved aspect ratio, video posters, voice notes recorded in Opus and drawn with the server's waveform, and a readiness gate so nothing renders before it is scanned and processed. |
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

Both assert rather than report: the suite fails the build on a regression, and
k6 exits 99 when a threshold is crossed — verified by running it against a
deliberately impossible target and confirming it fails.

### Not yet built

**No bot platform.** There is no BotFather equivalent, no bot token issuance,
no bot API, no webhooks and no inline queries. `users.is_bot` exists as a
column and the admin panel displays it, but nothing writes it: an account
cannot currently be created as a bot, and third parties cannot add bots to the
platform. This is a whole subsystem, not a missing endpoint.

**Schema exists, behaviour does not.** These were modelled in the database so
that adding them later needs no migration, but they have no endpoint and no
UI. The columns are inert today:

| Feature | What exists | What is missing |
|---|---|---|
| Forwarding | `forward_from_*` columns, rendered in responses | The forward endpoint |
| Scheduled messages | `scheduled_at` column and partial index | Scheduling and the publisher |
| Pinned messages | `is_pinned`, `SetPinned` in the repository, the `pin_messages` permission | The HTTP route |
| Stickers and GIFs | Media kinds, message types, `send_stickers` permission, `sticker_set` on groups | Sticker set management and a picker |
| Location and contact messages | Both in the message-type constraint | Composing and rendering them |
| Usernames | Unique index, the column, search by it | Claiming and changing one |
| Profile editing | `user_profiles` with every field | The endpoint — the app shows the device name because of this |
| Archived and pinned chats | `is_archived`, `is_pinned` on `chat_members` | The endpoints |
| Link previews | The `embed_links` permission | Unfurling |

**Secret chats on the device (§24).** The server half is complete. X3DH and
the Double Ratchet belong on the device and will use a reviewed implementation
rather than a hand-rolled one (§84.16–17), so this waits on choosing one.

**Load testing at scale (§22).** The harness implements the §78 ramp to 500k
concurrent, but running it needs a deployed cluster. What is measured above is
a single process: it bounds latency, not capacity.

`docs/architecture/roadmap.md` carries the per-stage detail.

## Documentation

| Document | Contents |
|---|---|
| `docs/protocol/websocket.md` | Frame format, events, sync, reconnection, scaling |
| `protocol/rest/openapi.yaml` | REST contract for the implemented surface |
| `docs/architecture/` | System design, database model, sync model, roadmap |
| `docs/security/` | Threat model and security controls |
| `docs/deployment/` | Deployment, backup and disaster recovery |

---

## Contributing rules

1. Every schema change is a migration with a working `down`.
2. No secret is ever committed; configuration comes from the environment.
3. No feature is "done" without tests and error, loading and empty states.
4. No colour, spacing or string is hard-coded in a widget.
5. No cryptographic primitive is invented; only reviewed constructions.
6. Breaking protocol changes require a version bump.

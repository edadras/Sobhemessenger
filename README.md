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
go test ./...                                    # unit tests only
SOBH_TEST_POSTGRES_DSN=postgres://… go test ./...  # + integration tests
```

Integration tests skip themselves when `SOBH_TEST_POSTGRES_DSN` is unset, so the
default run needs no database.

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
| **Messaging** | Chats and membership, permissions with per-member overrides, send/edit/delete/react/read/pin/draft, per-chat sequences, per-user event log, unread and mention counters, slow mode, edit windows. |
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
| **Flutter** | Clean-architecture foundation, design tokens, four locales with correct RTL, envelope-aware client with collapsed token refresh, WebSocket client with jittered backoff, Drift schema with a real offline outbox, sign-in and chat screens. |

### Not yet built

- **Mobile feature screens** — groups, channels, communities, stories, calls,
  news and settings have complete, documented APIs but no Flutter UI yet.
- **Contact sync (§54)** — the privacy-preserving design is in place
  (`users.phone_hash` is an HMAC under a server-side pepper); the batch
  matching endpoint is not written.
- **Secret chats (§24)** — key-exchange tables, prekey storage and ciphertext
  columns exist. The X3DH and Double Ratchet implementation belongs on the
  device and is not written.
- **End-to-end integration (§21)** — every module is tested against a real
  PostgreSQL, but the full stack has not been brought up together and exercised.
- **Load testing (§22)** — the k6 harness implements the §78 ramp with
  thresholds that fail on a missed §79 target; it has not been run.

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

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

The specification describes the complete platform. This repository implements it
in the staged order the specification itself prescribes, so that every stage is
tested and usable before the next begins. Nothing below is mocked or stubbed:
what is marked complete is wired to a real database and covered by tests.

### Complete

| Area | What exists |
|---|---|
| **Database** | 74 tables covering every domain in the specification — identity, devices, chats, messages, media, groups, channels, communities, stories, polls, calls, news, notifications, moderation, RBAC, audit, analytics. Up **and** down migrations, verified by applying and reverting against a live PostgreSQL. |
| **Backend foundation** | Config from environment with production validation, structured logging with redaction, PostgreSQL pool with transaction helpers, Redis, NATS JetStream, MinIO, Prometheus metrics, health/readiness probes, migration runner with checksums and advisory locking. |
| **HTTP layer** | Single response envelope, ~60 stable error codes, request id, real-IP resolution behind trusted proxies, security headers, CORS, per-route metrics, panic recovery, timeouts. |
| **Authentication** | OTP request/verify with sliding-window rate limits that fail closed, phone normalisation (E.164, Persian and Arabic digits), account creation on first sign-in, JWT access tokens with key rotation, refresh-token rotation with replay detection, two-step verification, session and device listing and revocation, login history. |
| **Messaging** | Chats and membership, permission model with per-member overrides, send/edit/delete/react/read/pin/draft, per-chat sequences, per-user event log, unread and mention counters, slow mode, edit windows, history paging by sequence. |
| **Realtime** | WebSocket with authenticated handshake, protocol versioning, heartbeats, request/ack correlation, resume-from-cursor, slow-consumer eviction, per-user and per-chat NATS routing across nodes, presence. |
| **Background work** | Durable job consumers with backoff and poison-message handling, plus maintenance: OTP expiry, event-log pruning, story expiry, abandoned uploads, scheduled article publishing. |
| **Feature flags** | Runtime switches with percentage rollout bucketed per user, cached in-process and shared via Redis. |
| **Infrastructure** | Docker Compose dev stack, distroless multi-stage image, nginx edge config, Prometheus scrape config and alerts derived from the §79 latency targets, Grafana provisioning, CI with lint, tests, reversible-migration check, vulnerability and secret scanning, and image scanning. |
| **Protocol** | OpenAPI 3.1 for the implemented surface, and a full WebSocket protocol document. |
| **Flutter foundation** | Clean-architecture skeleton, design tokens with light and dark palettes, four locales (fa/en/tr/ar) with correct RTL, Dio client that unwraps the envelope and collapses concurrent token refreshes, WebSocket client with jittered backoff and cursor resume, Drift schema with a real offline outbox, Riverpod providers, GoRouter with auth redirect, sign-in and chat screens. |

### Not yet built

These have their database schema, error codes and architectural seams in place,
but no service or UI yet:

- **Media pipeline** — upload sessions, presigned multipart uploads, magic-byte
  and virus validation, image and video variant generation, voice waveforms.
- **Groups, channels, communities** — membership administration, invite links,
  join requests, channel posts and statistics.
- **Stories, polls, calls** — including WebRTC signalling and TURN credentials.
- **News platform** — CMS, editorial workflow, feed, breaking news.
- **Search** — OpenSearch indexing and Persian/Arabic normalisation.
- **Notifications** — FCM and APNs delivery workers.
- **Admin panel** — `apps/admin` is scaffolded but empty.
- **Secret chats** — the key-exchange tables exist; the device-side ratchet does not.
- **Load testing** — no run has been performed against the §78 targets.

`docs/architecture/roadmap.md` carries the delivery order and the acceptance
criteria for each.

---

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

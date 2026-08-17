# Delivery roadmap

The specification describes the whole platform; this file records the order it
is being built in and what "done" means for each stage. The ordering follows
§82 and the staged approach the specification itself recommends in §86: the
architecture accounts for every feature from the start, but implementation
advances in tested increments so that adding video calling later does not
require breaking the messenger apart.

## Definition of done (§83)

A feature is complete only when all of the following hold:

- Database schema and migrations, with a working `down`
- Service logic with authorisation enforced server-side
- REST endpoint, and WebSocket event where the feature is realtime
- Flutter UI including offline behaviour, error, loading and empty states
- Permission handling and localisation for all four locales
- Light and dark theme support
- Unit tests, and integration tests against a real database
- Documentation updated

## Stages

| # | Stage | Status |
|---|---|---|
| 01 | Foundation — config, logging, dependencies, probes | done |
| 02 | Database — full schema, migrations, seeds | done |
| 03 | Authentication — OTP, JWT, refresh rotation, 2FA, sessions | done |
| 04 | Users — profiles, privacy, contacts | done: discovery by HMAC digest, blocking, privacy resolved per viewer in SQL, profile editing and username claiming with a 30-day release hold |
| 05 | WebSocket — hub, heartbeat, acks, sync, cross-node routing | done |
| 06 | Messaging — send, edit, delete, react, read, drafts | done, plus forwarding with attribution, pinning, scheduling and per-member archive and mute |
| 07 | Media — upload sessions, validation, variants | done, including the mobile composer and renderers |
| 08 | Groups — roles, permissions, invites, join requests | done |
| 09 | Channels — posts, statistics, discussion links | done |
| 10 | Communities | done |
| 11 | Stories | done: composer, tray, viewer, views and reactions |
| 12 | Calls — WebRTC signalling, TURN | done: signalling, TURN, history, and a real peer connection in the app |
| 13 | News platform — CMS, feed, breaking news | done |
| 14 | Search — OpenSearch, Persian normalisation | done |
| 15 | Notifications — FCM, APNs | done |
| 16 | Admin panel | done |
| 17 | Security hardening review | ongoing |
| 18 | Monitoring and alerting | done |
| 19 | Backup and disaster recovery | done |
| 20 | Flutter — feature screens | done: every domain with a backend has a client |
| 21 | Integration | done: the real router driven over HTTP and WebSocket against real dependencies |
| 22 | Load testing (§78) | partial: harness run against the assembled stack and passing; the 500k ramp needs a cluster |
| 23 | Production | not started |
| 24 | Bots — registration, tokens, updates, webhooks | done: @sobhfather_bot as a real conversational BotFather, hashed tokens, durable update queue, signed webhooks, inline keyboards, callback queries and inline mode |
| 25 | Stickers and link previews | done: sets with per-user installation, OpenGraph unfurling behind the SSRF guard |
| 26 | Location and contact messages | done: fixed points, venues, live location with server-computed expiry, contact cards |

## Secret chats (§24)

The server half is built: a key directory and a mailbox. It publishes public
key material, hands out each one-time prekey exactly once, and stores opaque
ciphertext until the recipient acknowledges it.

The client half — X3DH and the Double Ratchet — is deliberately not on the
server and is not yet written on the device. Per §84 rules 16 and 17 it will
use a reviewed implementation rather than a hand-rolled one.

## What is left

**Load testing at scale.** `scripts/loadtest/messaging.js` implements the §78
ramp to 500k concurrent with thresholds that fail on a missed §79 target. It
has been run against the assembled stack — API, PostgreSQL, Redis, NATS and
MinIO on one machine — and passes with a wide margin, and a deliberately
impossible threshold was used to confirm a breach really does fail the run.
Reaching the 500k stage needs a deployed cluster and load generators, which is
a capacity question rather than a code one.

**Secret chats on the device.** The server half is complete: a key directory
that hands out each one-time prekey once, and a mailbox that deletes ciphertext
on acknowledgement. X3DH and the Double Ratchet belong on the device, and §84
rules 16 and 17 require a reviewed implementation rather than a hand-rolled
one, so this waits on adopting a vetted library rather than on writing more
protocol code.

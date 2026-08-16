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
| 04 | Users — profiles, privacy, contacts | done: discovery by HMAC digest, blocking, privacy resolved per viewer in SQL |
| 05 | WebSocket — hub, heartbeat, acks, sync, cross-node routing | done |
| 06 | Messaging — send, edit, delete, react, read, drafts | done |
| 07 | Media — upload sessions, validation, variants | done: server and worker; the mobile UI sends text only |
| 08 | Groups — roles, permissions, invites, join requests | done |
| 09 | Channels — posts, statistics, discussion links | done |
| 10 | Communities | done |
| 11 | Stories | done: feed, viewer, views and reactions; no capture screen |
| 12 | Calls — WebRTC signalling, TURN | partial: signalling, ICE servers and history; no peer connection in the app |
| 13 | News platform — CMS, feed, breaking news | done |
| 14 | Search — OpenSearch, Persian normalisation | done |
| 15 | Notifications — FCM, APNs | done |
| 16 | Admin panel | done |
| 17 | Security hardening review | ongoing |
| 18 | Monitoring and alerting | done |
| 19 | Backup and disaster recovery | done |
| 20 | Flutter — feature screens | done for messaging, contacts, groups, channels, stories, news, search, settings |
| 21 | Integration | done: the real router driven over HTTP and WebSocket against real dependencies |
| 22 | Load testing (§78) | partial: §79 latency budgets asserted in-process; the 500k ramp has not been run against a cluster |
| 23 | Production | not started |

## Secret chats (§24)

The server half is built: a key directory and a mailbox. It publishes public
key material, hands out each one-time prekey exactly once, and stores opaque
ciphertext until the recipient acknowledges it.

The client half — X3DH and the Double Ratchet — is deliberately not on the
server and is not yet written on the device. Per §84 rules 16 and 17 it will
use a reviewed implementation rather than a hand-rolled one.

## What is left

**Media in the mobile UI.** The upload session, validation, variant and
playback APIs are complete and tested. The Flutter composer sends text only,
so images, video and voice cannot be attached from the app yet.

**The call screen.** Signalling relays SDP and ICE, TURN credentials are
issued, and history renders. The `flutter_webrtc` peer connection and the
in-call UI are not built.

**Story composition.** The tray, viewer and view recording work against the
real API; there is no capture or editing screen.

**Load testing at scale.** `scripts/loadtest/messaging.js` implements the §78
ramp to 500k concurrent with thresholds that fail on a missed §79 target. It
has not been run against a deployed cluster. The in-process end-to-end
measurements bound latency, not capacity.

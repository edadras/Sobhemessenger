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
| 04 | Users — profiles, usernames, privacy, contacts | partial: identity and privacy defaults exist; contact sync and username claiming do not |
| 05 | WebSocket — hub, heartbeat, acks, sync, cross-node routing | done |
| 06 | Messaging — send, edit, delete, react, read, drafts | done |
| 07 | Media — upload sessions, validation, variants | not started |
| 08 | Groups — roles, permissions, invites, join requests | schema only |
| 09 | Channels — posts, statistics, discussion links | schema only |
| 10 | Communities | schema only |
| 11 | Stories | schema only |
| 12 | Calls — WebRTC signalling, TURN | schema only |
| 13 | News platform — CMS, feed, breaking news | schema only |
| 14 | Search — OpenSearch, Persian normalisation | not started |
| 15 | Notifications — FCM, APNs | schema only |
| 16 | Admin panel | not started |
| 17 | Security hardening review | ongoing |
| 18 | Monitoring and alerting | done |
| 19 | Backup and disaster recovery | scripts pending |
| 20 | Flutter polish | foundation only |
| 21 | Integration | pending |
| 22 | Load testing (§78) | not started |
| 23 | Production | not started |

## Next stage: media (07)

Acceptance criteria:

- `POST /api/v1/media/upload-sessions` returns presigned part URLs for a
  multipart upload; the client uploads directly to object storage.
- Completion validates declared MIME against sniffed magic bytes, enforces the
  size limit and records a SHA-256; a mismatch rejects the object rather than
  correcting it.
- Virus scanning runs when `CLAMAV_ADDR` is set, and is mandatory when
  `VIRUS_SCAN_REQUIRED` is true.
- A worker produces image variants (thumbnail, small, medium, preview) and
  video variants (360p, 720p, 1080p) plus metadata and voice waveforms.
- Downloads are served by presigned URL, or through the CDN when configured.
- Attachments reference `media_id` only; bytes never pass through the API.

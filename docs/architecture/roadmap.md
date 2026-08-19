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
| 27 | Secret chats end to end (§24) | done: X3DH and the Double Ratchet on the device through a reviewed library, keys in the platform keystore, safety numbers per device; history is not persisted — see below |
| 28 | Chat organisation — clearing history, folders | done: one-sided and destructive clearing, folders as saved filters with rules and per-chat overrides |
| 29 | Forum topics (§14) | done: conversion files the existing history under General, per-topic reading, closing and moderation |
| 30 | Channel comments (§15) | done: posts mirrored into a linked discussion group, comments as ordinary replies to the mirror, author signatures |
| 31 | Contact requests (§54) | done: send, accept, reject, withdraw, with blocking and privacy giving one refusal |
| 32 | Anti-spam scoring (§34) | done: weighted signals, a lapsing restriction narrowed to cold outreach, decay in the worker |
| 33 | Email account recovery (§4) | done: a recovery address that does nothing until it is verified, and clears the two-step password with every session |
| 34 | Named role bundles, read receipts, call sessions (§14, §7, §20) | done: a bundle resolved between the chat's defaults and a member's overrides, who has read one message, and one signalling row per participant so a reconnect does not start from nothing |
| 35 | Article galleries and translations (§25) | done: an ordered gallery and a per-locale translation that is a draft until someone signs it off |
| 36 | Clients for the surfaces nothing called | done: the sign-in log, close friends, live location updates, channel statistics, ownership transfer, editorial authors and spam scores — see below |
| 40 | Flags that gate, presence that shows | done: feature flags enforced on the routes they name, live presence surfaced on a profile under the same privacy rule as last seen, and the last six client-side dead ends wired |
| 39 | The clients nothing called | done: inline mode, promoting a member, linking a discussion group, adding a community room, following a news category, looking up an exact handle, validating a sticker-set slug, editing a bot's commands, per-chat folder overrides, turning a forum off, and the unread badge |
| 38 | The last five | done: setting the two-step password, drafts that follow you between devices, reading one forum topic on its own, the breaking-news banner, and a past call's negotiation record |
| 37 | The rest of the unreachable surfaces | done: redeeming an invite link, counting a channel post's views, data export and account deletion, the recovery flow itself, poll voters, secret-session records, community room removal, bans, operator roles, search reindexing, news categories, and the network a call leg is on |

## The endpoints nothing called

Seven endpoints were served, documented, covered by tests, and reached by no
application. That is harder to notice than a broken endpoint, because
everything about it looks healthy — it compiles, it has a route, its handler is
tested. What it does not have is a caller, so nothing has ever checked that the
answer is a shape a client could use.

`GET /auth/login-history` was answering in Go field names — `Event`,
`UserAgent`, `Succeeded` — while every other response in the API is snake_case.
An app reading it would have got nulls where it cast unconditionally, which on
the phone is a crash rather than an error it can show. It also recorded only
successful sign-ins, so the screen it exists to fill would have shown a clean
history while somebody was working through codes against the account.

Live location was the largest of them. The app could *start* a share, and then
never sent a single update or offered any way to stop one: a pin that sits
where it was first dropped while the message claims to be live is worse than
not having the feature, because the person waiting for you believes it. The
device now keeps every running share moving, reads them back from its own
database so one survives the app being closed, and treats the server's "that
share is over" as final.

Two more were found by driving the new clients against a live server rather
than by reading the code. Assigning a role belonging to another chat was
refused in the sense that mattered — the permissions were not granted — but it
was refused by computing the new value from a scoped subquery, which wrote NULL
and reported success: the refusal silently took away whatever role the member
already held. The integration test for that case asserted the call *succeeded*
and had no effect, which is exactly how the second half stayed invisible. And
`EnsureBotFather` failed when BotFather already existed, because a lookup and
an `ON CONFLICT DO NOTHING ... RETURNING` are not one atomic step — two API
nodes starting together is the ordinary case, not a rare one.

A second pass over the same question found the rest, and two more defects with
it. Redeeming an invite link worked on the server and no application could do
it, so an invite was a string somebody could send and nobody could use —
including, until this, somebody who opened one while signed out, because the
sign-in bounce discarded where they had been going. And the handler validating
a call signal accepted `ice-candidate` while the repository that stored one
matched `candidate`: every candidate fell through to the default branch and was
never written, so a session record showed offers and answers with no candidates
at all. That reads as a call that gathered none, rather than as two layers
spelling the same word differently — which is why the vocabulary is now three
constants named once.

Counting a channel post's views is worth its own note. The statistics screen
had been built the day before, and it displayed a `view_count` that nothing in
the app incremented: it would have read zero for ever, on posts hundreds of
people had opened. A screen that is wrong in a plausible direction is worse
than one that is missing.

A third pass closed the last five, and two of them were features with no way
in at all. The app could *enter* a two-step password at sign-in and, once the
recovery screen existed, *clear* a forgotten one — with no way to set one in
the first place. A second factor nobody can turn on is not a second factor.
And tapping a forum topic did nothing: the list showed how many messages each
held, and opening one was not wired to anything, so a forum was a chat with
labels on it.

The self profile did not report whether two-step was on, either, so the
settings screen could not have shown its own state. It does now.

Syncing a draft turned out to be two halves. Pushing it was one line; the
device also had to be willing to take one back, and `upsertChats` deliberately
preserved the local draft column — correctly, since a refresh must not destroy
a sentence in progress. The rule that resolves both: the server's draft fills
an empty box and never overwrites a full one. That is what lets a sentence
started on the desktop turn up on the phone, and it is the one piece of this
work with enough judgement in it to be worth a test of its own.

A fourth pass changed the question being asked, and that is what found the
rest. "Does this path appear in the Dart source?" proves a repository method
exists; it says nothing about whether a screen calls it. Asking "is this method
ever invoked?" turned up twelve clients that were written, compiled and reached
by nothing.

The largest was inline mode. `openQuery`, `results` and `choose` were all
there, complete, and no screen touched any of them — so a feature this file
called done had no way in, and typing `@somebot pizza` left the text sitting in
the box. Promoting a member was the same: the client existed, nothing called
it, and every administrator therefore had to be made by editing the database.
Linking a channel to a discussion group was missing while unlinking was
present, so comments could only ever be switched off; adding a room to a
community was missing while removing one was present, so a community could only
shrink. Following a news category was unreachable, which made the `following`
feed a list that could only ever be empty.

Two smaller ones are worth recording because they are the same shape as the
bugs above. The group sticker-set field took a slug typed by hand and never
checked it existed, so a typo was stored and simply produced no stickers —
indistinguishable from a set that had none. And the unread notification count
was fetched by nothing, so a story, a contact request or a news alert arrived
with nothing to say so.

A fifth pass asked the same question of every layer instead of only the client
one — providers nothing watches, screens nothing opens, service methods nothing
calls — and found the two largest instances of it in the project.

**Feature flags gated nothing.** `featureflags.Require` existed, the panel
could toggle a flag, and no route ever consulted one, so switching a feature
off left it running. The baseline seed made it easy to miss in the opposite
direction: it shipped `stories_enabled`, `calls_enabled`, `communities_enabled`
and `secret_chats_enabled` as FALSE, and all four features worked, so the flags
looked fine. They are now enforced on the route groups they name, and the seed
says TRUE for what is built. An unknown flag key refuses rather than defaulting
to on — a typo in a gate must not silently open what it guards.

**Presence was recorded and never read.** Every connection marked the account
online in Redis and nothing ever asked. A profile now carries `is_online`,
governed by the same privacy rule as last seen and for the same reason: online
now is the sharpest form of when were they last here, so a viewer who may not
have the second must not be handed the first.

The rest were client-side: who watched a story, who has read a message, the
device list, link previews, and another person's profile — each fully built,
each opened by nothing.

`scripts/verify/reachable_surfaces.py` is the probe that asks these questions
from the client's side, and it is where all of them came from.

## Secret chats (§24)

Both halves are built.

**The server** is a key directory and a mailbox. It publishes public key
material, hands out each one-time prekey exactly once, and stores opaque
ciphertext until the recipient acknowledges it — at which point it is deleted
rather than flagged. `POST /api/v1/secret/chats` opens the conversation itself,
idempotently on an ordered pair of users, so two devices racing to start one
land in a single chat. Nothing on the server parses, transforms or inspects a
payload.

**The device** performs X3DH and the Double Ratchet through
`libsignal_protocol_dart`, a port of Signal's own library (GPL-3.0, recorded
per §84.19). §84 rules 16 and 17 forbid inventing or assembling the
construction, so the app supplies transport, storage and identity and does no
cryptography of its own. Private keys and ratchet state are held in the iOS
keychain and the Android keystore, never in the message database. The identity
is generated once and published at every launch — a device whose keys were
never published cannot be reached, and nobody can be asked to wait for it to
come online — and one-time prekeys top up when the server reports them low.

Safety numbers are derived from both identity keys and shown per device, since
that is what they are computed against: a person with a phone and a tablet has
two, and a number that matched for one says nothing about the other. Signing
out wipes every key and session, so that it is a boundary rather than a screen.

### The trade-off that is deliberate

Decrypted text is held in memory while a conversation is open, and nowhere
else. Closing the app loses the history.

Every other conversation in SOBH is offline-first, backed by the local Drift
database. That database is not encrypted at rest. Writing secret-chat plaintext
into it would move the message from a place the operating system protects to a
file any process with storage access can read, and the encryption would then
only be guarding the part of the journey that was already safe.

Keeping history without giving that up needs an encrypted local store —
SQLCipher under Drift, keyed from the platform keystore. That is a change to
the whole local database rather than to this feature, and it is listed under
what is left rather than half-implemented here.

## The columns that had no code

Migration 0005 cut six columns for features nobody then wrote: a channel's
signature switch and comment switch, the link between a channel and its
discussion group in both directions, a group's sticker set, and broadcast mode.
`contact_requests` and `spam_scores` were whole tables in the same position,
and `users.recovery_email` had existed since 0001 with nothing to prove an
address written there belonged to the account holder.

All of them are now wired, and migration 0016 adds what genuinely had no schema
at all: a per-member watermark for clearing history, chat folders with their
include and exclude lists, forum topics with per-topic read cursors, the
correspondence between a channel post and its mirrored copy, and the challenge
table email recovery needs.

`scripts/verify/new_surfaces.py` drives all of it against a running server.

## What is left

**Load testing at scale.** `scripts/loadtest/messaging.js` implements the §78
ramp to 500k concurrent with thresholds that fail on a missed §79 target. It
has been run against the assembled stack — API, PostgreSQL, Redis, NATS and
MinIO on one machine — and passes with a wide margin, and a deliberately
impossible threshold was used to confirm a breach really does fail the run.
Reaching the 500k stage needs a deployed cluster and load generators, which is
a capacity question rather than a code one.

**An encrypted local store.** Secret chats work end to end, but their history
is not kept on the device — see the trade-off recorded above. Adding SQLCipher
under Drift, keyed from the platform keystore, would let encrypted
conversations persist on the same terms as every other one. It touches the
whole local database, including a migration path for existing installs, so it
is its own piece of work.

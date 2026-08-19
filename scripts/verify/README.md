# Verification probes

Things that only a running system can answer, and which every test in the
repository passed while each of them was broken.

Run each against a live stack — the API on `127.0.0.1:8080` with its
dependencies up, and `SMS_ECHO_CODES=true` so the probes can sign in.

| Script | Answers |
|---|---|
| `../smoke.sh` | Does an ordinary session work? 44 requests: sign in, chat, send, read, edit, react, delete, secret chats and their refusals, groups, forward, schedule, bots, search. |
| `every_endpoint.py` | Does any endpoint fail on the server side? Builds real fixtures, then calls every operation in the OpenAPI document. **Any 5xx is a defect**, except a `SERVICE_UNAVAILABLE` from a feature the deployment has switched off — that is the server declining to pretend, not failing. A 4xx is usually the endpoint correctly refusing input the probe did not know how to build. |
| `client_shapes.py` | Does the server accept what the mobile app actually sends? Each request is copied from the Dart repository that issues it, so a refusal here is a real mismatch rather than a guess. |
| `response_shapes.py` | Can the app *read* what the server writes? Every Dart model casts its required fields unconditionally, so a missing field is a crash on the phone rather than an error the app can show. |
| `realtime.py` | Does a message actually arrive? Two people, two sockets: delivery and its latency against the §79 budget, typing, read receipts, reactions, edits and deletions, catching up after being offline, and that a non-member's socket receives nothing. |
| `bot_api_and_media.py` | Can a bot hold a conversation with its own token — authenticate, receive a command as an update, reply, attach an inline keyboard — and does an upload session open? |
| `worker.sh` | Does the worker bind all six job consumers, and does it do real work? Schedules a post, moves its time into the past, and waits for the scheduler to publish it into the conversation. |
| `reachable_surfaces.py` | Do the surfaces that had no client work, and answer in a shape a client can read? The sign-in log, close friends, live location, channel statistics, ownership transfer, role bundles, read receipts and call sessions — 65 checks. |
| `new_surfaces.py` | Do the nine features that had no code work through the API the app calls? Clearing history one side at a time, channel signatures and comments, the discussion-group link, the group sticker set and broadcast mode, contact requests, email recovery, chat folders and forum topics — 65 checks, each a claim about behaviour rather than a status code. |

## Why these exist

`reachable_surfaces.py` exists because seven endpoints were served, documented,
covered by tests, and called by nothing. That is harder to notice than a broken
endpoint: everything about it looks healthy. What it lacks is a caller, so
nothing has ever checked that the answer is a shape a client could use — and
`/auth/login-history` was answering in Go field names (`Event`, `UserAgent`)
while every other response in the API is snake_case. It also found that
assigning a role belonging to another chat was refused in the sense that
mattered — the permissions were not granted — while the update wrote NULL and
reported success, so the refusal silently took away whatever role the member
already held. The integration test for that case asserted the call *succeeded*
and had no effect, which is how the second half went unnoticed.

`every_endpoint.py` found the news feed returning 500 on every request: the
query named four placeholders and always passed six arguments, so PostgreSQL
refused it. Nothing else caught it, because compiling says nothing about a
query's argument count.

`client_shapes.py` found that the app sent `type: "audio"` when placing a call
while the server and the calls table both say `voice` — every voice call was
rejected with a validation error.

`realtime.py` found that the sender's own devices were never told about their
own messages: `appendUserEvents` excluded the actor, so a second device learned
nothing from `/sync` — the app's whole multi-device catch-up path — and could
only find the message by refetching the conversation.

`new_surfaces.py` found two things about its own fixtures rather than the
server, and both are worth recording because the first kind of failure looks
exactly like the second. Groups and channels are created through `POST /chats`,
not `POST /groups`; the probe's first version got `None` back and then compared
two absent values in several checks, which *passed*. Fixtures now stop the run
when they come back empty. And a channel post from an account with no display
name, no username and no custom title is signed with nothing — correctly, since
there is no name to sign with and the phone number is not a substitute — so the
probe sets a display name first, and a test now pins the nameless case.

`worker.sh` was passing on evidence from an earlier run: it signed in with two
fixed phone numbers, so every run shared one conversation, and a post published
minutes earlier satisfied the search before this run's post had gone anywhere.
Fresh numbers and a nonce in the body fixed it — the check now fails when the
scheduler does nothing, which is what it was for.

## A note on 4xx

Several probes deliberately send a request and expect a refusal: plaintext into
an encrypted chat, an unknown privacy key, a bad bot token, a non-member
listening on a socket. A refusal there is the feature working. Read the label,
not the status code.

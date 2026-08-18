# Verification probes

Three things that only a running system can answer, and which every test in the
repository passed while each of them was broken.

Run each against a live stack — the API on `127.0.0.1:8080` with its
dependencies up, and `SMS_ECHO_CODES=true` so the probes can sign in.

| Script | Answers |
|---|---|
| `../smoke.sh` | Does an ordinary session work? 44 requests: sign in, chat, send, read, edit, react, delete, secret chats and their refusals, groups, forward, schedule, bots, search. |
| `every_endpoint.py` | Does any endpoint fail on the server side? Builds real fixtures, then calls every operation in the OpenAPI document. **Any 5xx is a defect.** A 4xx is usually the endpoint correctly refusing input the probe did not know how to build. |
| `client_shapes.py` | Does the server accept what the mobile app actually sends? Each request is copied from the Dart repository that issues it, so a refusal here is a real mismatch rather than a guess. |
| `response_shapes.py` | Can the app *read* what the server writes? Every Dart model casts its required fields unconditionally, so a missing field is a crash on the phone rather than an error the app can show. |
| `realtime.py` | Does a message actually arrive? Two people, two sockets: delivery and its latency against the §79 budget, typing, read receipts, reactions, edits and deletions, catching up after being offline, and that a non-member's socket receives nothing. |
| `bot_api_and_media.py` | Can a bot hold a conversation with its own token — authenticate, receive a command as an update, reply, attach an inline keyboard — and does an upload session open? |
| `worker.sh` | Does the worker bind all six job consumers, and does it do real work? Schedules a post, moves its time into the past, and waits for the scheduler to publish it into the conversation. |

## Why these exist

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

## A note on 4xx

Several probes deliberately send a request and expect a refusal: plaintext into
an encrypted chat, an unknown privacy key, a bad bot token, a non-member
listening on a socket. A refusal there is the feature working. Read the label,
not the status code.

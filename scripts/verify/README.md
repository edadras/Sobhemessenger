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

## Why these exist

`every_endpoint.py` found the news feed returning 500 on every request: the
query named four placeholders and always passed six arguments, so PostgreSQL
refused it. Nothing else caught it, because compiling says nothing about a
query's argument count.

`client_shapes.py` found that the app sent `type: "audio"` when placing a call
while the server and the calls table both say `voice` — every voice call was
rejected with a validation error.

#!/usr/bin/env python3
"""Drive the surfaces that existed on the server with no client to reach them.

Seven endpoints were served, documented and tested, and no application ever
called one of them. That is a harder thing to notice than a broken endpoint,
because everything about it looks healthy: it compiles, it has a route, its
handler is covered. What it does not have is a caller, so nothing has ever
checked that the answer is a shape a client could use.

The login history is the clearest case. It returned its rows in Go field names
— `Event`, `UserAgent` — while every other response in the API is snake_case,
and no client existed to be broken by it.

So each check here is written from the client's side: it asks for exactly what
the app now asks for, and asserts on the fields the app actually reads.

Run with the stack up and SMS_ECHO_CODES=true.
"""
import json, os, secrets, sys, time, urllib.error, urllib.request, uuid

API = os.environ.get("SOBH_API", "http://127.0.0.1:8080/api/v1")

passed = 0
failed = 0


def call(method, path, token=None, body=None):
    data = json.dumps(body).encode() if body is not None else None
    req = urllib.request.Request(API + path, data=data, method=method)
    req.add_header("Content-Type", "application/json")
    if token:
        req.add_header("Authorization", "Bearer " + token)
    try:
        with urllib.request.urlopen(req, timeout=20) as response:
            return response.status, json.loads(response.read().decode() or "{}")
    except urllib.error.HTTPError as error:
        raw = error.read().decode()
        try:
            return error.code, json.loads(raw or "{}")
        except Exception:
            return error.code, {"raw": raw[:300]}
    except Exception as error:
        return 0, {"transport": str(error)}


def data_of(body):
    return body.get("data") or {}


def check(label, condition, detail=""):
    global passed, failed
    if condition:
        passed += 1
        print(f"  ok   {label}")
    else:
        failed += 1
        print(f"  FAIL {label}   {detail}")


def section(title):
    print(f"\n=== {title} ===")


def signin():
    phone = "+98912" + "".join(secrets.choice("0123456789") for _ in range(7))
    status, body = call("POST", "/auth/otp/request", body={"phone": phone})
    code = data_of(body).get("debug_code")
    if not code:
        raise SystemExit(f"no debug code for {phone}: {status} {body}")
    status, body = call("POST", "/auth/otp/verify", body={
        "phone": phone, "code": code, "device_name": "probe",
        "platform": "android", "app_version": "1.0.0"})
    account = data_of(body)
    if not account.get("access_token"):
        raise SystemExit(f"could not sign in {phone}: {status} {body}")
    return account["access_token"], account["user_id"], phone


def make_chat(token, kind, title, **extra):
    body = {"type": kind, "title": title}
    body.update(extra)
    status, response = call("POST", "/chats", token, body)
    chat_id = data_of(response).get("chat_id")
    if not chat_id:
        raise SystemExit(f"could not create a {kind}: {status} {response}")
    return chat_id


# ---------------------------------------------------------------- fixtures

alice_token, alice_id, alice_phone = signin()
bob_token, bob_id, bob_phone = signin()
carol_token, carol_id, _ = signin()

status, body = call("POST", "/chats/private", alice_token, {"user_id": bob_id})
private_chat = data_of(body).get("chat_id")
if not private_chat:
    raise SystemExit(f"could not open a private chat: {status} {body}")

group = make_chat(alice_token, "group", "گروه آزمون", member_ids=[bob_id])
channel = make_chat(alice_token, "channel", "کانال آزمون")

# ------------------------------------------------------------ login history

section("sign-in history (settings → security)")

status, body = call("GET", "/auth/login-history", alice_token)
check("the history can be read", status == 200, f"{status} {body}")
entries = data_of(body).get("history") or []
check("signing in put something in it", len(entries) > 0, str(body))

if entries:
    entry = entries[0]
    # This is the check the endpoint never had a client to make. Every one of
    # these is a field the app casts unconditionally; a PascalCase key here is
    # a null cast on the phone, not a message the app can show.
    for field in ("event", "platform", "succeeded", "created_at"):
        check(f"`{field}` is present and snake_case",
              field in entry, f"keys were {sorted(entry)}")
    check("`succeeded` is a boolean, not a string",
          isinstance(entry.get("succeeded"), bool), repr(entry.get("succeeded")))
    check("`created_at` parses as a timestamp",
          isinstance(entry.get("created_at"), str)
          and "T" in entry.get("created_at", ""), repr(entry.get("created_at")))
    check("no Go field names leak through",
          not any(k[:1].isupper() for k in entry), str(sorted(entry)))

# A refused code is the entry worth reading, and it needs a second challenge to
# produce — which the resend cooldown holds off for a minute after signing in.
# Rather than sit and wait, the attempt is made at the end of the run, by which
# time the rest of the probe has spent the cooldown.
SIGNED_IN_AT = time.monotonic()

# ------------------------------------------------------------ close friends

section("close friends (story privacy)")

status, body = call("GET", "/stories/close-friends", alice_token)
check("the list can be read before it is set", status == 200, f"{status} {body}")
check("an unset list is empty rather than absent",
      data_of(body).get("user_ids") == [], str(body))

status, _ = call("PUT", "/stories/close-friends", alice_token,
                 {"user_ids": [bob_id, carol_id]})
check("the list can be set", status in (200, 204), f"got {status}")

status, body = call("GET", "/stories/close-friends", alice_token)
saved = set(data_of(body).get("user_ids") or [])
check("both people are on it", saved == {bob_id, carol_id}, str(saved))

# Replacement, not merge — which is why the editor sends the whole list.
status, _ = call("PUT", "/stories/close-friends", alice_token,
                 {"user_ids": [bob_id]})
status, body = call("GET", "/stories/close-friends", alice_token)
saved = set(data_of(body).get("user_ids") or [])
check("saving a shorter list removes the rest", saved == {bob_id}, str(saved))

status, body = call("POST", "/stories", alice_token,
                    {"type": "text", "caption": "برای نزدیکان",
                     "background": "#123456", "privacy": "close_friends"})
check("a close-friends story is accepted", status in (200, 201), f"{status} {body}")
story_id = data_of(body).get("id")

status, body = call("GET", "/stories", bob_token)
visible = [s["id"] for s in (data_of(body).get("stories") or [])]
check("a close friend sees it", story_id in visible, str(visible))

status, body = call("GET", "/stories", carol_token)
visible = [s["id"] for s in (data_of(body).get("stories") or [])]
check("someone taken off the list does not",
      story_id not in visible, str(visible))

# --------------------------------------------------------- live location

section("live location (the pin that has to keep moving)")

status, body = call("POST", f"/chats/{private_chat}/messages", alice_token,
                    {"client_message_id": str(uuid.uuid4()), "type": "location",
                     "content": "",
                     "payload": {"latitude": 35.70, "longitude": 51.40,
                                 "horizontal_accuracy": 12.0,
                                 "live_period_seconds": 3600}})
check("a live location can be shared", status in (200, 201), f"{status} {body}")
live_message = data_of(body).get("id")
payload = data_of(body).get("payload") or {}
# The app cannot compute this itself, so it has to come back on the send.
check("the server returns the deadline it computed",
      isinstance(payload.get("live_until"), str), str(payload))

status, body = call("PUT", f"/messages/{live_message}/live-location", alice_token,
                    {"latitude": 35.71, "longitude": 51.41,
                     "horizontal_accuracy": 8.0, "heading": 90.0, "speed": 1.4})
check("the author can move it", status == 200, f"{status} {body}")
moved = (data_of(body).get("payload") or {})
check("the new position is what came back",
      abs((moved.get("latitude") or 0) - 35.71) < 1e-6, str(moved))
check("moving it does not extend the share",
      moved.get("live_until") == payload.get("live_until"),
      f"{payload.get('live_until')} -> {moved.get('live_until')}")

status, _ = call("PUT", f"/messages/{live_message}/live-location", bob_token,
                 {"latitude": 0.0, "longitude": 0.0})
check("nobody else can move it", status == 404, f"got {status}")

status, _ = call("DELETE", f"/messages/{live_message}/live-location", alice_token)
check("the author can stop it", status in (200, 204), f"got {status}")

status, body = call("PUT", f"/messages/{live_message}/live-location", alice_token,
                    {"latitude": 35.72, "longitude": 51.42})
# The controller treats this as terminal and stops trying, so it has to be a
# refusal the client can tell apart from a network failure.
check("a stopped share cannot be restarted", status == 409, f"{status} {body}")

status, _ = call("DELETE", f"/messages/{live_message}/live-location", alice_token)
check("stopping twice is refused the same way", status == 409, f"got {status}")

# ------------------------------------------------------- channel statistics

section("channel post statistics")

status, body = call("POST", f"/chats/{channel}/messages", alice_token,
                    {"client_message_id": str(uuid.uuid4()), "type": "text",
                     "content": "پست کانال"})
post_id = data_of(body).get("id")
check("a channel post can be made", bool(post_id), f"{status} {body}")

status, body = call("GET", f"/chats/{channel}/statistics?message_id={post_id}",
                    alice_token)
check("statistics can be read by the owner", status == 200, f"{status} {body}")
rows = data_of(body).get("statistics")
check("the answer is a list, even when nothing has happened yet",
      isinstance(rows, list), str(body))

status, _ = call("GET", f"/chats/{channel}/statistics?message_id={post_id}",
                 carol_token)
check("a non-member is refused", status in (403, 404), f"got {status}")

status, body = call("GET", f"/chats/{channel}/statistics?message_id=not-a-uuid",
                    alice_token)
check("a malformed id is a 400, not a 500", status == 400, f"got {status}")

# ------------------------------------------------------- ownership transfer

section("transfer of ownership")

handover = make_chat(alice_token, "group", "واگذاری", member_ids=[bob_id])

status, _ = call("POST", f"/chats/{handover}/transfer-ownership", bob_token,
                 {"user_id": bob_id})
check("a member cannot take the chat", status in (403, 404), f"got {status}")

status, _ = call("POST", f"/chats/{handover}/transfer-ownership", alice_token,
                 {"user_id": bob_id})
check("the owner can hand it over", status in (200, 204), f"got {status}")

status, body = call("GET", f"/chats/{handover}/members", alice_token)
members = {m["user_id"]: m["role"] for m in (data_of(body).get("members") or [])}
check("the new owner holds the role", members.get(bob_id) == "owner", str(members))
check("the old owner is no longer owner",
      members.get(alice_id) != "owner", str(members))

status, _ = call("POST", f"/chats/{handover}/transfer-ownership", alice_token,
                 {"user_id": alice_id})
check("the former owner cannot take it back",
      status in (403, 404), f"got {status}")

# ------------------------------------------------------------- role bundles

section("named role bundles")

status, body = call("POST", f"/chats/{group}/roles", alice_token,
                    {"name": "ناظر", "rank": 10,
                     "permissions": {"pin_messages": True,
                                     "delete_messages": True}})
check("a bundle can be created", status in (200, 201), f"{status} {body}")
role_id = (data_of(body).get("role") or {}).get("id")

status, body = call("GET", f"/chats/{group}/roles", alice_token)
roles = data_of(body).get("roles") or []
check("it appears in the list", any(r["id"] == role_id for r in roles), str(roles))
check("it starts with nobody holding it",
      all(r.get("member_count") == 0 for r in roles if r["id"] == role_id),
      str(roles))

status, body = call("POST", f"/chats/{group}/roles", alice_token,
                    {"name": "غلط", "permissions": {"fly_a_plane": True}})
check("a permission the server does not know is refused",
      status == 422, f"{status} {body}")

status, _ = call("PUT", f"/chats/{group}/members/{bob_id}/role-bundle",
                 alice_token, {"role_id": role_id})
check("a member can be given the bundle", status in (200, 204), f"got {status}")

status, body = call("GET", f"/chats/{group}/roles", alice_token)
holders = {r["id"]: r.get("member_count") for r in (data_of(body).get("roles") or [])}
check("the holder count follows", holders.get(role_id) == 1, str(holders))

# A bundle belongs to one chat. Pinning another chat's role onto a member here
# would be a permission grant nobody in this chat ever approved.
status, body = call("POST", f"/chats/{channel}/roles", alice_token,
                    {"name": "دیگر", "permissions": {"pin_messages": True}})
other_role = (data_of(body).get("role") or {}).get("id")
status, _ = call("PUT", f"/chats/{group}/members/{bob_id}/role-bundle",
                 alice_token, {"role_id": other_role})
check("a role from another chat cannot be assigned",
      status in (404, 422), f"got {status}")

status, _ = call("PUT", f"/chats/{group}/members/{bob_id}/role-bundle",
                 alice_token, {"role_id": None})
check("the bundle can be taken away", status in (200, 204), f"got {status}")

status, _ = call("DELETE", f"/chats/{group}/roles/{role_id}", alice_token)
check("a bundle can be deleted", status in (200, 204), f"got {status}")

# ------------------------------------------------------------ read receipts

section("who has read a message")

status, body = call("POST", f"/chats/{private_chat}/messages", alice_token,
                    {"client_message_id": str(uuid.uuid4()), "type": "text",
                     "content": "خوانده شد؟"})
read_target = data_of(body).get("id")
target_seq = data_of(body).get("seq")

status, body = call("GET", f"/messages/{read_target}/reads", alice_token)
check("the list can be read before anyone has", status == 200, f"{status} {body}")
check("nobody has read it yet",
      (data_of(body).get("count") or 0) == 0, str(body))

status, _ = call("POST", f"/chats/{private_chat}/read", bob_token,
                 {"seq": target_seq})
check("the other side can mark it read", status in (200, 204), f"got {status}")

status, body = call("GET", f"/messages/{read_target}/reads", alice_token)
readers = [r.get("user_id") for r in (data_of(body).get("reads") or [])]
check("the reader is named", bob_id in readers, str(data_of(body)))
entry = (data_of(body).get("reads") or [{}])[0]
for field in ("user_id", "display_name", "read_at"):
    check(f"a receipt carries `{field}`", field in entry, str(sorted(entry)))

status, _ = call("GET", f"/messages/{read_target}/reads", carol_token)
check("someone outside the chat is refused", status == 404, f"got {status}")

# --------------------------------------------------------------- call legs

section("call sessions")

status, body = call("POST", "/calls", alice_token,
                    {"chat_id": private_chat, "type": "voice"})
call_id = data_of(body).get("id") or data_of(body).get("call_id")
check("a call can be started", bool(call_id), f"{status} {body}")

if call_id:
    status, body = call("GET", f"/calls/{call_id}/sessions", alice_token)
    check("its sessions can be listed", status == 200, f"{status} {body}")
    sessions = data_of(body).get("sessions")
    check("the answer is a list", isinstance(sessions, list), str(body))
    for session in sessions or []:
        # Signalling material is for the peer, not for anyone who can see the
        # call. Returning it here would hand a participant list the means to
        # impersonate a leg.
        check("no SDP or candidates are handed out",
              "sdp" not in session and "candidates" not in session,
              str(sorted(session)))

    status, _ = call("GET", f"/calls/{call_id}/sessions", carol_token)
    check("someone outside the call is refused",
          status in (403, 404), f"got {status}")

# ---------------------------------------------------- editorial and spam ops

section("operator surfaces (refused without the role, never 5xx)")

# The probe holds an ordinary account, so what is checked is that these refuse
# cleanly rather than failing. An operator run is a separate exercise; a 5xx
# here would be a defect either way.
for label, method, path in (
    ("editorial authors", "GET", "/editorial/authors"),
    ("article gallery", "GET", f"/editorial/articles/{uuid.uuid4()}/gallery"),
    ("article translations", "GET",
     f"/editorial/articles/{uuid.uuid4()}/translations"),
    ("spam score", "GET", f"/admin/spam-scores/{alice_id}"),
):
    status, body = call(method, path, alice_token)
    check(f"{label}: refused rather than broken",
          400 <= status < 500, f"{status} {body}")

# ------------------------------------------------- the refused sign-in, last

section("a refused code reaches the sign-in history")

# The cooldown is a minute from the sign-in at the top of this run. Whatever
# the rest of the probe did not use up is waited out here, once.
COOLDOWN = 61
remaining = COOLDOWN - (time.monotonic() - SIGNED_IN_AT)
if remaining > 0:
    print(f"  ..   waiting {remaining:.0f}s for the OTP resend cooldown")
    time.sleep(remaining)

status, body = call("POST", "/auth/otp/request", body={"phone": alice_phone})
check("a second code can be requested after the cooldown",
      status == 200, f"{status} {body}")

status, body = call("POST", "/auth/otp/verify", body={
    "phone": alice_phone, "code": "000000", "device_name": "probe",
    "platform": "android", "app_version": "1.0.0"})
check("the wrong code is refused", status == 401, f"{status} {body}")

status, body = call("GET", "/auth/login-history", alice_token)
entries = data_of(body).get("history") or []
# Someone trying codes against an account is exactly what this screen exists to
# show. A history of successes only would look clean while it was happening.
check("the attempt is in the history",
      any(e.get("succeeded") is False for e in entries),
      str([(e.get("event"), e.get("succeeded")) for e in entries[:5]]))
check("the successful sign-in is still there too",
      any(e.get("succeeded") is True for e in entries),
      str([(e.get("event"), e.get("succeeded")) for e in entries[:5]]))

print("\n" + "=" * 50)
print(f"passed: {passed}   failed: {failed}")
sys.exit(1 if failed else 0)

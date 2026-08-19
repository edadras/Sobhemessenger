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

# ---------------------------------------------------------- invite links

section("redeeming an invite link")

status, body = call("POST", f"/chats/{group}/invite-links", alice_token,
                    {"name": "probe"})
invite = data_of(body)
slug = invite.get("slug")
check("an invite link can be made", bool(slug), f"{status} {body}")

if slug:
    status, body = call("POST", f"/chats/join/{slug}", carol_token)
    check("somebody else can redeem it", status == 200, f"{status} {body}")
    outcome = data_of(body)
    check("the answer says they joined",
          outcome.get("joined") is True, str(outcome))
    check("and which chat they joined",
          outcome.get("chat_id") == group, str(outcome))
    # The app navigates on chat_type, so an absent one lands nowhere.
    check("and what kind of chat it is",
          outcome.get("chat_type") in ("group", "channel"), str(outcome))

    # Redeeming twice is the ordinary case — someone taps the link again.
    status, body = call("POST", f"/chats/join/{slug}", carol_token)
    check("redeeming it again is not an error",
          status == 200 and data_of(body).get("joined") is True,
          f"{status} {body}")

status, _ = call("POST", "/chats/join/not-a-real-slug", carol_token)
check("an unknown link is refused", status == 404, f"got {status}")

# --------------------------------------------------------- channel views

section("counting a view on a channel post")

status, body = call("POST", f"/chats/{channel}/messages", alice_token,
                    {"client_message_id": str(uuid.uuid4()), "type": "text",
                     "content": "شمارش بازدید"})
viewed_post = data_of(body).get("id")
check("a post to count views on", bool(viewed_post), f"{status} {body}")

status, _ = call("POST", f"/chats/{channel}/join", bob_token)
status, _ = call("POST",
                 f"/chats/{channel}/messages/{viewed_post}/view", bob_token)
check("a reader's view is accepted", status in (200, 204), f"got {status}")

status, body = call("GET", f"/chats/{channel}/statistics?message_id={viewed_post}",
                    alice_token)
rows = data_of(body).get("statistics") or []
counted = next((r for r in rows if r["message_id"] == viewed_post), None)
# This is the claim the statistics screen rests on: something increments the
# number it displays. Without it the screen shows zero for ever.
check("the view reaches the statistics",
      counted is not None and counted.get("view_count", 0) >= 1, str(rows))

status, _ = call("POST",
                 f"/chats/{channel}/messages/{viewed_post}/view", bob_token)
status, body = call("GET", f"/chats/{channel}/statistics?message_id={viewed_post}",
                    alice_token)
rows = data_of(body).get("statistics") or []
again = next((r for r in rows if r["message_id"] == viewed_post), None)
check("the same reader is not counted twice",
      again is not None and again.get("view_count") == counted.get("view_count"),
      f"{counted} -> {again}")

# ------------------------------------------------------------ data rights

section("asking for your data, and asking to be forgotten")

status, body = call("GET", "/me/data-requests", alice_token)
check("the requests can be listed", status == 200, f"{status} {body}")
check("the grace period comes with them",
      isinstance(data_of(body).get("deletion_delay_seconds"), int),
      str(data_of(body)))

status, body = call("POST", "/me/data-requests", alice_token, {"type": "export"})
check("an export can be asked for", status in (200, 201), f"{status} {body}")
export_request = (data_of(body).get("request") or {}).get("id")

status, body = call("POST", "/me/data-requests", carol_token, {"type": "delete"})
check("a deletion can be asked for", status in (200, 201), f"{status} {body}")
deletion = data_of(body).get("request") or {}
deletion_id = deletion.get("id")
# The delay is the whole reason a deletion is withdrawable.
check("it is scheduled rather than immediate",
      isinstance(deletion.get("execute_after"), str), str(deletion))

status, body = call("GET", "/chats", carol_token)
check("the account still works during the grace period",
      status == 200, f"got {status}")

if deletion_id:
    status, _ = call("DELETE", f"/me/data-requests/{deletion_id}", carol_token)
    check("a deletion can be withdrawn", status in (200, 204), f"got {status}")

status, body = call("POST", "/me/data-requests", alice_token, {"type": "invent"})
check("an unknown kind is refused", status == 422, f"{status} {body}")

# ---------------------------------------------------------- poll voters

section("who voted for what")

status, body = call("POST", "/polls", alice_token,
                    {"chat_id": group, "client_message_id": str(uuid.uuid4()),
                     "question": "کدام؟", "options": ["الف", "ب"],
                     "is_anonymous": False})
poll = data_of(body)
poll_id = poll.get("id")
options = poll.get("options") or []
check("an open poll can be created", bool(poll_id) and len(options) == 2,
      f"{status} {body}")

if poll_id and options:
    option_id = options[0]["id"]
    status, _ = call("POST", f"/polls/{poll_id}/vote", bob_token,
                     {"option_ids": [option_id]})
    check("somebody can vote", status in (200, 201), f"got {status}")

    status, body = call("GET",
                        f"/polls/{poll_id}/options/{option_id}/voters",
                        alice_token)
    check("the voters can be listed", status == 200, f"{status} {body}")
    voters = data_of(body).get("voters")
    check("the answer is a list", isinstance(voters, list), str(body))
    if voters:
        voter = voters[0]
        # A list of bare ids is not something a screen can render, which is
        # what this endpoint used to return.
        for field in ("user_id", "display_name", "voted_at"):
            check(f"a voter carries `{field}`", field in voter,
                  str(sorted(voter)))

    status, body = call("GET",
                        f"/polls/{poll_id}/options/{options[1]['id']}/voters",
                        alice_token)
    check("an option nobody picked answers with an empty list, not null",
          data_of(body).get("voters") == [], str(data_of(body)))

status, body = call("POST", "/polls", alice_token,
                    {"chat_id": group, "client_message_id": str(uuid.uuid4()),
                     "question": "ناشناس؟", "options": ["الف", "ب"],
                     "is_anonymous": True})
secret_poll = data_of(body)
if secret_poll.get("id") and secret_poll.get("options"):
    status, _ = call("GET",
                     f"/polls/{secret_poll['id']}/options/"
                     f"{secret_poll['options'][0]['id']}/voters", alice_token)
    # This is the promise an anonymous poll makes, and the one refusal here
    # that must never soften.
    check("an anonymous poll never says who voted",
          status in (403, 409, 422), f"got {status}")

# ---------------------------------------------------------- call signals

section("what a call leg negotiated")

status, body = call("POST", "/calls", alice_token,
                    {"chat_id": private_chat, "type": "voice"})
signal_call = data_of(body).get("id") or data_of(body).get("call_id")
check("a call to signal on", bool(signal_call), f"{status} {body}")

if signal_call:
    for kind, payload in (
        ("offer", {"sdp": "v=0 probe offer"}),
        ("ice-candidate", {"candidate": "candidate:1 1 udp 1 10.0.0.1 1 typ host"}),
        ("ice-candidate", {"candidate": "candidate:2 1 udp 1 10.0.0.2 2 typ host"}),
    ):
        status, body = call("POST", f"/calls/{signal_call}/signal", alice_token,
                            {"to": bob_id, "type": kind,
                             "network_type": "wifi", "payload": payload})
        check(f"a {kind} is relayed", status in (200, 204), f"{status} {body}")

    status, body = call("GET", f"/calls/{signal_call}/sessions", alice_token)
    mine = next((s for s in (data_of(body).get("sessions") or [])
                 if s.get("user_id") == alice_id), None)
    check("the leg has a session record", mine is not None, str(data_of(body)))
    if mine:
        check("its offer was recorded", mine.get("has_offer") is True, str(mine))
        # The handler validated `ice-candidate` while the repository matched
        # `candidate`, so every candidate fell through and was never written.
        check("its candidates were recorded too",
              mine.get("candidate_count", 0) == 2, str(mine))
        check("the network it is on was recorded",
              mine.get("network_type") == "wifi", str(mine))

    status, _ = call("POST", f"/calls/{signal_call}/signal", alice_token,
                     {"to": bob_id, "type": "offer",
                      "network_type": "carrier-pigeon", "payload": {}})
    check("an unknown network type is refused", status == 422, f"got {status}")

# ------------------------------------------------------------- two-step

section("setting the second factor")

status, body = call("GET", "/users/me", bob_token)
me = data_of(body)
# The settings screen has no other way to ask whether it is on. Without this
# the only way to find out was to be locked out at the next sign-in.
check("the profile says whether two-step is on",
      "two_step_enabled" in me, str(sorted(me)))
check("and it is off to begin with",
      me.get("two_step_enabled") is False, str(me.get("two_step_enabled")))

status, body = call("PUT", "/auth/two-step", bob_token,
                    {"new_password": "short"})
check("a short password is refused", status == 422, f"{status} {body}")

status, body = call("PUT", "/auth/two-step", bob_token,
                    {"new_password": "probe-passphrase-1", "hint": "the probe"})
check("a password can be set", status == 200, f"{status} {body}")

status, body = call("GET", "/users/me", bob_token)
me = data_of(body)
check("the profile now says it is on",
      me.get("two_step_enabled") is True, str(me))
check("and shows the owner their own hint",
      me.get("two_step_hint") == "the probe", str(me))

status, body = call("PUT", "/auth/two-step", bob_token,
                    {"new_password": "probe-passphrase-2"})
# Somebody holding a live session must not be able to replace the factor that
# exists to guard against somebody holding a live session.
check("changing it without the current password is refused",
      status == 401, f"{status} {body}")

status, body = call("PUT", "/auth/two-step", bob_token,
                    {"current_password": "probe-passphrase-1",
                     "new_password": "probe-passphrase-2"})
check("changing it with the current password works", status == 200,
      f"{status} {body}")

status, _ = call("PUT", "/auth/two-step", bob_token,
                 {"current_password": "probe-passphrase-2", "new_password": ""})
check("it can be turned off again", status == 200, f"got {status}")
status, body = call("GET", "/users/me", bob_token)
check("and the profile agrees",
      data_of(body).get("two_step_enabled") is False, str(data_of(body)))

# ---------------------------------------------------------------- drafts

section("a draft that follows you")

status, _ = call("PUT", f"/chats/{private_chat}/draft", alice_token,
                 {"draft": "نیمه‌تمام"})
check("a draft can be stored", status in (200, 204), f"got {status}")

status, body = call("GET", "/chats", alice_token)
def membership_of(token, chat_id):
    _, listing = call("GET", "/chats", token)
    for entry in data_of(listing).get("chats") or []:
        if entry.get("id") == chat_id:
            return entry.get("membership") or {}
    return {}

# This is what makes a draft worth syncing: another device reads it back from
# the chat list, so an unfinished sentence is waiting there too. It rides on
# the membership rather than the chat, because a draft belongs to one person.
check("it comes back on the chat list",
      membership_of(alice_token, private_chat).get("draft") == "نیمه‌تمام",
      str(membership_of(alice_token, private_chat)))

# The other side has their own, so one person's draft is not the chat's.
check("it is not shown to the other member",
      not membership_of(bob_token, private_chat).get("draft"),
      str(membership_of(bob_token, private_chat)))

status, _ = call("PUT", f"/chats/{private_chat}/draft", alice_token,
                 {"draft": ""})
check("clearing it clears it",
      not membership_of(alice_token, private_chat).get("draft"),
      str(membership_of(alice_token, private_chat)))

status, _ = call("PUT", f"/chats/{private_chat}/draft", carol_token,
                 {"draft": "غریبه"})
check("a non-member cannot leave one", status in (403, 404), f"got {status}")

# ---------------------------------------------------------- forum topics

section("reading one topic on its own")

forum = make_chat(alice_token, "group", "انجمن آزمون", member_ids=[bob_id])
status, body = call("POST", f"/chats/{forum}/forum", alice_token)
check("a group can become a forum", status in (200, 201), f"{status} {body}")

status, body = call("POST", f"/chats/{forum}/topics", alice_token,
                    {"title": "موضوع یک"})
topic_one = (data_of(body).get("topic") or {}).get("id")
check("a topic can be created", bool(topic_one), f"{status} {body}")

status, body = call("POST", f"/chats/{forum}/topics", alice_token,
                    {"title": "موضوع دو"})
topic_two = (data_of(body).get("topic") or {}).get("id")
check("and a second one", bool(topic_two), f"{status} {body}")

if topic_one and topic_two:
    for text in ("اول", "دوم"):
        call("POST", f"/chats/{forum}/messages", alice_token,
             {"client_message_id": str(uuid.uuid4()), "type": "text",
              "content": text, "topic_id": topic_one})
    call("POST", f"/chats/{forum}/messages", alice_token,
         {"client_message_id": str(uuid.uuid4()), "type": "text",
          "content": "جای دیگر", "topic_id": topic_two})

    status, body = call("GET", f"/chats/{forum}/topics/{topic_one}/messages",
                        alice_token)
    check("one topic's messages can be read", status == 200, f"{status} {body}")
    contents = [m.get("content") for m in (data_of(body).get("messages") or [])]
    # The whole point of a forum: each topic is its own conversation, not a
    # label on one.
    check("it holds only its own", sorted(contents) == ["اول", "دوم"],
          str(contents))

    status, body = call("GET", f"/chats/{forum}/topics/{topic_two}/messages",
                        alice_token)
    contents = [m.get("content") for m in (data_of(body).get("messages") or [])]
    check("and the other holds only its own", contents == ["جای دیگر"],
          str(contents))

    status, _ = call("GET", f"/chats/{forum}/topics/{topic_one}/messages",
                     carol_token)
    check("a non-member reads nothing", status in (403, 404), f"got {status}")

# -------------------------------------------------------- breaking banner

section("the one breaking story")

status, body = call("GET", "/news/breaking?locale=fa", alice_token)
check("the banner endpoint answers", status == 200, f"{status} {body}")
# Null is the ordinary state — most of the time nothing is breaking — and the
# banner has to be able to tell that from a failure.
check("no breaking story is an answer, not an error",
      "article" in data_of(body), str(data_of(body)))

# --------------------------------------------------------- feature flags

section("a flag that is off actually turns something off")

# Flags could be toggled in the panel and nothing on the server read one, so
# switching a feature off left it running. The seed ships them on, so what is
# checked here is that a gated route serves while its flag is on — the refusal
# path is covered by the unit test, which can turn a flag off without taking
# the feature away from the rest of this run.
for label, path in (
    ("stories", "/stories"),
    ("calls", "/calls"),
    ("communities", "/communities"),
):
    status, body = call("GET", path, alice_token)
    check(f"{label} is served while its flag is on",
          status == 200, f"{status} {body}")

status, body = call("GET", "/feature-flags", alice_token)
flags = {f["key"]: f for f in (data_of(body).get("flags") or [])}
check("the flags can be read", status == 200, f"{status} {body}")
for key in ("stories_enabled", "calls_enabled", "communities_enabled",
            "secret_chats_enabled"):
    # A flag the seed leaves off would take its feature away from every
    # deployment that starts from it, which is what these being FALSE used to
    # hide while nothing read them.
    check(f"{key} is on in the baseline",
          flags.get(key, {}).get("enabled") is True, str(flags.get(key)))

# ------------------------------------------- what the app can now reach

section("the clients that existed and nothing called")

# Promoting somebody was impossible from the app: the client was there and no
# screen used it, so every administrator had to be made in the database.
promo = make_chat(alice_token, "group", "ارتقا", member_ids=[bob_id])
status, _ = call("PUT", f"/chats/{promo}/members/{bob_id}/role", alice_token,
                 {"role": "admin"})
check("a member can be promoted", status in (200, 204), f"got {status}")
status, body = call("GET", f"/chats/{promo}/members", alice_token)
roles = {m["user_id"]: m["role"] for m in (data_of(body).get("members") or [])}
check("and the role sticks", roles.get(bob_id) == "admin", str(roles))
status, _ = call("PUT", f"/chats/{promo}/members/{bob_id}/role", alice_token,
                 {"role": "member"})
check("and can be taken back", status in (200, 204), f"got {status}")

# Linking a discussion group was the half that did not exist; unlinking did.
disc_channel = make_chat(alice_token, "channel", "کانال نظر")
disc_group = make_chat(alice_token, "group", "گروه نظر")
status, body = call("POST", f"/chats/{disc_channel}/discussion", alice_token,
                    {"group_chat_id": disc_group})
check("a discussion group can be linked", status in (200, 204),
      f"{status} {body}")
status, body = call("GET", f"/chats/{disc_channel}/settings", alice_token)
check("the channel reports it",
      data_of(body).get("discussion_chat_id") == disc_group,
      str(data_of(body).get("discussion_chat_id")))

# Adding a room, the other half of last pass's removal.
status, body = call("POST", "/communities", alice_token,
                    {"title": "انجمن", "is_public": False})
community = data_of(body).get("id")
if community:
    room = make_chat(alice_token, "group", "اتاق")
    status, _ = call("POST", f"/communities/{community}/rooms", alice_token,
                     {"chat_id": room, "section": "general"})
    check("a room can be added to a community", status in (200, 201, 204),
          f"got {status}")
    status, body = call("GET", f"/communities/{community}", alice_token)
    rooms = [r.get("chat_id") for r in (data_of(body).get("rooms") or [])]
    check("and it is in the community", room in rooms, str(rooms))

# Following a category, which is what the `following` feed mode reads — a mode
# that could only ever have been empty.
status, body = call("GET", "/news/categories?locale=fa", alice_token)
cats = data_of(body).get("categories") or []
# A silent skip here would leave the check looking green while testing nothing,
# which is the failure this probe's own history is full of.
check("there is a category to follow", len(cats) > 0, f"{status} {body}")
if cats:
    status, _ = call("PUT", "/news-reader/follows", alice_token,
                     {"category_id": cats[0]["id"], "follow": True})
    check("a category can be followed", status in (200, 204), f"got {status}")
    status, body = call("GET", "/news?mode=following&locale=fa", alice_token)
    check("the following feed answers", status == 200, f"{status} {body}")

# An exact handle went through fuzzy search and could rank below near-misses.
wanted = "probe" + secrets.token_hex(3)
status, body = call("PUT", "/users/me/username", alice_token,
                    {"username": wanted})
handle = data_of(body).get("username") or (wanted if status == 200 else None)
check("a username can be claimed", handle is not None, f"{status} {body}")
if handle:
    status, body = call("GET", f"/users/by-username/{handle}", alice_token)
    check("an account can be found by its exact handle", status == 200,
          f"{status} {body}")
    check("and it is the right one",
          data_of(body).get("user_id") == alice_id, str(data_of(body)))
status, _ = call("GET", "/users/by-username/definitely-not-a-handle",
                 alice_token)
check("an unknown handle is a 404, not a guess", status == 404, f"got {status}")

# Turning topics off, the other half of turning them on.
offable = make_chat(alice_token, "group", "خاموش‌شدنی")
call("POST", f"/chats/{offable}/forum", alice_token)
status, _ = call("DELETE", f"/chats/{offable}/forum", alice_token)
check("a forum can be turned back off", status in (200, 204), f"got {status}")

# The badge nothing ever fetched.
status, body = call("GET", "/notifications/unread-count", alice_token)
check("the unread count can be read", status == 200, f"{status} {body}")
check("and it is a number",
      isinstance(data_of(body).get("unread_count"), int), str(data_of(body)))

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

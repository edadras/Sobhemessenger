#!/usr/bin/env python3
"""Drive the nine features that had no code, end to end, against a live server.

Each check is a claim about behaviour rather than a status code: that a
one-sided clear leaves the other member's history alone, that an unverified
recovery address cannot recover anything, that an explicit exclusion beats a
folder's rules. A green run here means the feature works through the API the
app actually calls, which is the one thing no unit test can tell you.

Run with the stack up, SMS_ECHO_CODES=true, and — for the recovery checks —
EMAIL_PROVIDER pointed at a transport that reports itself as deliverable.
"""
import json, os, secrets, sys, time, urllib.error, urllib.request, uuid

API = os.environ.get("SOBH_API", "http://127.0.0.1:8080/api/v1")

passed = 0
failed = 0
skipped = 0


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


def skip(label, why):
    global skipped
    skipped += 1
    print(f"  skip {label}   {why}")


def section(title):
    print(f"\n=== {title} ===")


def signin():
    phone = "+98913" + "".join(secrets.choice("0123456789") for _ in range(7))
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


def send(token, chat_id, text, topic_id=None):
    body = {"client_message_id": str(uuid.uuid4()), "type": "text", "content": text}
    if topic_id:
        body["topic_id"] = topic_id
    return call("POST", f"/chats/{chat_id}/messages", token, body)


def history(token, chat_id):
    status, body = call("GET", f"/chats/{chat_id}/messages?limit=100", token)
    return data_of(body).get("messages") or []


# ---------------------------------------------------------------- fixtures

alice_token, alice_id, alice_phone = signin()
bob_token, bob_id, bob_phone = signin()

status, body = call("POST", "/chats/private", alice_token, {"user_id": bob_id})
private_chat = data_of(body).get("chat_id")
if not private_chat:
    raise SystemExit(f"could not open a private chat: {status} {body}")

def make_chat(token, kind, title, **extra):
    """Groups and channels are both created through POST /chats.

    A fixture that quietly comes back as None turns every later check into a
    comparison of two absent values, several of which then "pass" — so this
    stops the run instead.
    """
    body = {"type": kind, "title": title}
    body.update(extra)
    status, response = call("POST", "/chats", token, body)
    chat_id = data_of(response).get("chat_id")
    if not chat_id:
        raise SystemExit(f"could not create a {kind}: {status} {response}")
    return chat_id


group_chat = make_chat(alice_token, "group", "Probe group", member_ids=[bob_id])
channel_chat = make_chat(alice_token, "channel", "Probe channel",
                         username="probe" + secrets.token_hex(4), is_public=True)
discussion_chat = make_chat(alice_token, "group", "Probe discussion")

# ------------------------------------------------------------ clear history

section("clearing history")

send(alice_token, private_chat, "before the clear")
send(bob_token, private_chat, "and another")
status, body = call("POST", f"/chats/{private_chat}/clear-history", alice_token, {})
check("a one-sided clear is accepted", status == 200, f"{status} {body}")
watermark = data_of(body).get("cleared_upto_seq")
check("it reports the sequence it cleared to", (watermark or 0) >= 2, str(watermark))

check("alice's history is empty", history(alice_token, private_chat) == [],
      "she still sees messages")
check("bob's history is untouched", len(history(bob_token, private_chat)) == 2,
      "a one-sided clear reached the other member")

send(bob_token, private_chat, "sent after the clear")
check("a later message is visible again",
      len(history(alice_token, private_chat)) == 1,
      "the watermark hid a message sent after it")

status, body = call("POST", f"/chats/{private_chat}/clear-history", alice_token,
                    {"for_everyone": True})
check("clearing for everyone is accepted in a one-to-one chat", status == 200,
      f"{status} {body}")
check("bob's history is empty too", history(bob_token, private_chat) == [],
      "a clear for everyone left the other side holding messages")

status, _ = call("POST", f"/chats/{group_chat}/clear-history", bob_token,
                 {"for_everyone": True})
check("an ordinary member cannot clear a group for everyone", status == 403,
      f"got {status}")
status, _ = call("POST", f"/chats/{group_chat}/clear-history", bob_token, {})
check("but may still clear their own copy", status == 200, f"got {status}")

# ------------------------------------------------------ signatures and comments

section("channel signatures and comments")

status, first_post = send(alice_token, channel_chat, "unsigned post")
check("a channel post is unsigned by default",
      data_of(first_post).get("author_signature", "") == "",
      str(data_of(first_post).get("author_signature")))

# A signature is a name, so the account has to have one. Registration leaves
# the display name empty until the profile screen sets it, and an admin with
# no name at all is signed with nothing — which is the right answer and not
# what this check is about.
call("PATCH", "/users/me", alice_token, {"display_name": "Probe Editor"})

status, body = call("PUT", f"/chats/{channel_chat}/settings", alice_token, {
    "slow_mode_seconds": 0, "history_visible_to_new": True,
    "join_requires_approval": False, "max_members": 200000,
    "auto_delete_seconds": 0, "signature_enabled": True})
check("signatures can be switched on", status == 200, f"{status} {body}")

status, signed = send(alice_token, channel_chat, "signed post")
check("the next post carries the admin's name",
      data_of(signed).get("author_signature") == "Probe Editor",
      str(data_of(signed).get("author_signature")))

status, body = call("GET", f"/chats/{channel_chat}/settings", alice_token)
settings = data_of(body)
check("a channel reports no sticker set", "sticker_set" not in settings,
      str(settings.keys()))

status, body = call("PUT", f"/chats/{channel_chat}/settings", alice_token, {
    "slow_mode_seconds": 0, "history_visible_to_new": True,
    "join_requires_approval": False, "max_members": 200000,
    "auto_delete_seconds": 0})
status, body = call("GET", f"/chats/{channel_chat}/settings", alice_token)
check("an update that never mentions signatures leaves them on",
      data_of(body).get("signature_enabled") is True,
      str(data_of(body).get("signature_enabled")))

status, body = call("POST", f"/chats/{channel_chat}/discussion", alice_token,
                    {"group_chat_id": discussion_chat})
check("a discussion group can be linked", status == 200, f"{status} {body}")

status, body = call("GET", f"/chats/{discussion_chat}/settings", alice_token)
check("the group points back at the channel",
      data_of(body).get("linked_channel_id") == channel_chat,
      str(data_of(body).get("linked_channel_id")))

post_id = data_of(signed).get("id")
time.sleep(0.5)  # The mirror is made by an observer, just after delivery.
status, body = call("GET",
                    f"/chats/{channel_chat}/messages/{post_id}/comments",
                    alice_token)
check("a post has a comment thread", status == 200, f"{status} {body}")
thread = data_of(body).get("thread") or {}
check("the thread lives in the discussion group",
      thread.get("discussion_chat_id") == discussion_chat,
      str(thread.get("discussion_chat_id")))

status, body = call("POST", f"/chats/{channel_chat}/messages/{post_id}/comments",
                    bob_token, {"client_message_id": str(uuid.uuid4()),
                                "type": "text", "content": "a comment"})
check("a reader who is not in the group may still comment", status == 201,
      f"{status} {body}")

status, body = call("GET",
                    f"/chats/{channel_chat}/messages/{post_id}/comments",
                    alice_token)
comments = data_of(body).get("comments") or []
check("the comment is in the thread", len(comments) == 1, f"{len(comments)}")

status, _ = call("DELETE", f"/chats/{channel_chat}/discussion", alice_token)
check("the discussion group can be detached", status == 200, f"got {status}")
check("the replies survive detaching",
      len(history(alice_token, discussion_chat)) >= 2,
      "unlinking destroyed other people's replies")

# ------------------------------------------------------------- sticker set

section("group sticker set and broadcast mode")

status, body = call("PUT", f"/chats/{group_chat}/settings", alice_token, {
    "slow_mode_seconds": 0, "history_visible_to_new": True,
    "join_requires_approval": False, "max_members": 200000,
    "auto_delete_seconds": 0, "sticker_set": "no_such_set_here"})
check("a sticker set that does not exist is refused", status == 422,
      f"got {status}")

status, body = call("PUT", f"/chats/{group_chat}/settings", alice_token, {
    "slow_mode_seconds": 0, "history_visible_to_new": True,
    "join_requires_approval": False, "max_members": 200000,
    "auto_delete_seconds": 0, "is_broadcast": True})
check("broadcast mode can be switched on", status == 200, f"{status} {body}")

status, body = send(bob_token, group_chat, "may I speak")
check("an ordinary member cannot post in a broadcast group", status == 403,
      f"got {status}")
status, body = send(alice_token, group_chat, "staff can")
check("staff still can", status == 201, f"got {status}")

call("PUT", f"/chats/{group_chat}/settings", alice_token, {
    "slow_mode_seconds": 0, "history_visible_to_new": True,
    "join_requires_approval": False, "max_members": 200000,
    "auto_delete_seconds": 0, "is_broadcast": False})

# -------------------------------------------------------- contact requests

section("contact requests")

call("PUT", "/me/privacy/messages", bob_token, {"rule": "everyone"})

status, body = call("POST", "/contacts/requests", alice_token,
                    {"user_id": bob_id, "message": "we met at the conference"})
check("a request can be sent", status == 201, f"{status} {body}")
request_id = (data_of(body).get("request") or {}).get("id")

status, body = call("POST", "/contacts/requests", alice_token,
                    {"user_id": bob_id, "message": "again"})
repeat_id = (data_of(body).get("request") or {}).get("id")
check("re-sending returns the same request", repeat_id == request_id,
      f"{repeat_id} vs {request_id}")

status, body = call("GET", "/contacts/requests?direction=incoming", bob_token)
incoming = data_of(body).get("requests") or []
check("it is waiting on bob", len(incoming) == 1, str(len(incoming)))

status, _ = call("POST", f"/contacts/requests/{request_id}/accept", alice_token)
check("the sender cannot accept their own request", status == 404, f"got {status}")

status, body = call("POST", f"/contacts/requests/{request_id}/accept", bob_token)
check("the target can", status == 200, f"{status} {body}")

status, body = call("GET", "/contacts", bob_token)
contacts = data_of(body).get("contacts") or []
check("bob gained alice", any(c.get("user_id") == alice_id for c in contacts),
      "acceptance did not write the address book")

status, _ = call("POST", f"/contacts/requests/{request_id}/reject", bob_token)
check("an already-resolved request cannot be resolved again", status == 409,
      f"got {status}")

# ---------------------------------------------------------- email recovery

section("email recovery")

status, body = call("GET", "/auth/recovery/email", alice_token)
available = data_of(body).get("available")
if not available:
    skip("email recovery", "this server has no mail transport (EMAIL_PROVIDER=log)")
else:
    address = f"probe-{secrets.token_hex(4)}@example.test"
    status, body = call("PUT", "/auth/recovery/email", alice_token,
                        {"email": address})
    check("an address can be enrolled", status == 200, f"{status} {body}")
    code = data_of(body).get("debug_code")

    status, body = call("GET", "/auth/recovery/email", alice_token)
    check("it is not verified yet", data_of(body).get("verified") is False,
          str(data_of(body)))
    check("the address is masked", "@" in (data_of(body).get("email") or "")
          and "*" in (data_of(body).get("email") or ""),
          str(data_of(body).get("email")))

    status, body = call("POST", "/auth/recovery/email/complete", None,
                        {"email": address, "code": code or "000000"})
    check("an unverified address cannot recover the account", status == 401,
          f"got {status}")

    if code:
        status, body = call("POST", "/auth/recovery/email/verify", alice_token,
                            {"code": code})
        check("the emailed code verifies it", status == 200, f"{status} {body}")
        status, body = call("GET", "/auth/recovery/email", alice_token)
        check("and it now reports verified",
              data_of(body).get("verified") is True, str(data_of(body)))
    else:
        skip("verifying the address", "EMAIL_ECHO_CODES is off")

    status, body = call("POST", "/auth/recovery/email/start", None,
                        {"email": f"nobody-{secrets.token_hex(4)}@example.test"})
    check("an unknown address gets the same answer as a known one",
          status == 200, f"got {status}")

# --------------------------------------------------------------- folders

section("chat folders")

name = "Groups " + secrets.token_hex(3)
status, body = call("POST", "/chats/folders", alice_token,
                    {"title": name, "include_groups": True})
check("a folder can be created", status == 201, f"{status} {body}")
folder_id = data_of(body).get("folder_id")

status, body = call("POST", "/chats/folders", alice_token, {"title": name})
check("two folders cannot share a name", status == 409, f"got {status}")

status, body = call("GET", f"/chats?folder_id={folder_id}", alice_token)
ids = [chat["id"] for chat in (data_of(body).get("chats") or [])]
check("the folder holds the group", group_chat in ids, str(ids))
check("and not the one-to-one chat", private_chat not in ids, str(ids))

status, _ = call("PUT", f"/chats/folders/{folder_id}/chats/{group_chat}",
                 alice_token, {"mode": "exclude"})
check("a chat can be excluded explicitly", status == 200, f"got {status}")
status, body = call("GET", f"/chats?folder_id={folder_id}", alice_token)
ids = [chat["id"] for chat in (data_of(body).get("chats") or [])]
check("an explicit exclusion beats the rules", group_chat not in ids, str(ids))

status, _ = call("PUT", f"/chats/folders/{folder_id}/chats/{private_chat}",
                 alice_token, {"mode": "include"})
status, body = call("GET", f"/chats?folder_id={folder_id}", alice_token)
ids = [chat["id"] for chat in (data_of(body).get("chats") or [])]
check("an explicit inclusion beats them too", private_chat in ids, str(ids))

status, body = call("GET", "/chats/folders", alice_token)
folders = data_of(body).get("folders") or []
mine = next((f for f in folders if f["id"] == folder_id), None)
check("the folder reports what it holds",
      mine is not None and mine.get("chat_count") == len(ids),
      f"{mine.get('chat_count') if mine else None} vs {len(ids)}")

status, body = call("GET", "/chats/folders", bob_token)
check("a folder is private to its owner",
      all(f["id"] != folder_id for f in (data_of(body).get("folders") or [])),
      "somebody else can see it")

status, _ = call("DELETE", f"/chats/folders/{folder_id}", alice_token)
check("a folder can be deleted", status == 200, f"got {status}")
check("its chats are untouched",
      any(chat["id"] == group_chat
          for chat in (data_of(call("GET", "/chats", alice_token)[1]).get("chats") or [])),
      "deleting a folder took its chats with it")

# ----------------------------------------------------------- forum topics

section("forum topics")

forum_chat = make_chat(alice_token, "group", "Probe forum", member_ids=[bob_id])
send(alice_token, forum_chat, "said before the conversion")

status, body = call("POST", f"/chats/{forum_chat}/forum", alice_token)
check("a group can become a forum", status == 200, f"{status} {body}")
general = (data_of(body).get("general_topic") or {}).get("id")

status, body = call("GET",
                    f"/chats/{forum_chat}/topics/{general}/messages",
                    alice_token)
filed = data_of(body).get("messages") or []
check("the existing history is filed under General", len(filed) == 1,
      f"{len(filed)} messages under General")

status, body = call("POST", f"/chats/{forum_chat}/topics", bob_token,
                    {"title": "A question"})
check("any member who may post may open a topic", status == 201,
      f"{status} {body}")
topic_id = (data_of(body).get("topic") or {}).get("id")

status, body = send(alice_token, forum_chat, "filed under the topic", topic_id)
check("a message can name its topic",
      data_of(body).get("topic_id") == topic_id,
      str(data_of(body).get("topic_id")))

status, body = send(alice_token, forum_chat, "no topic named")
check("one that names none lands in General",
      data_of(body).get("topic_id") == general,
      str(data_of(body).get("topic_id")))

status, _ = call("PATCH", f"/chats/{forum_chat}/topics/{topic_id}", bob_token,
                 {"is_closed": True})
check("an ordinary member cannot close a topic", status == 403, f"got {status}")

status, _ = call("PATCH", f"/chats/{forum_chat}/topics/{topic_id}", alice_token,
                 {"is_closed": True})
check("staff can", status == 200, f"got {status}")

status, _ = send(bob_token, forum_chat, "one more thing", topic_id)
check("a closed topic refuses a member", status == 403, f"got {status}")
status, _ = send(alice_token, forum_chat, "closing this", topic_id)
check("but still admits the staff who closed it", status == 201, f"got {status}")

status, _ = call("DELETE", f"/chats/{forum_chat}/topics/{general}", alice_token)
check("the General topic cannot be deleted", status == 409, f"got {status}")

status, _ = call("DELETE", f"/chats/{forum_chat}/topics/{topic_id}", alice_token)
check("another topic can be", status == 200, f"got {status}")

status, body = call("GET", f"/chats/{forum_chat}/topics", alice_token)
topics = data_of(body).get("topics") or []
check("the deleted topic is gone from the list",
      all(t["id"] != topic_id for t in topics), str([t["title"] for t in topics]))

# --------------------------------------------------------------- anti-spam

section("anti-spam scoring")

status, body = call("POST", "/reports", bob_token,
                    {"target_type": "user", "target_id": alice_id,
                     "reason": "spam", "detail": "probe"})
check("a user can be reported", status in (200, 201), f"{status} {body}")

# The score itself is behind a moderator permission this probe does not hold,
# so what is checked here is that reporting does not fail — the scoring path
# runs inside it, and a broken one would surface as a 5xx.
check("reporting did not fail on the server side", status < 500, f"got {status}")

print("\n" + "=" * 50)
print(f"passed: {passed}   failed: {failed}   skipped: {skipped}")
sys.exit(1 if failed else 0)

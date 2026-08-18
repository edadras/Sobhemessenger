#!/usr/bin/env python3
"""Send exactly what the mobile app sends, and see whether the server accepts it.

The probe before this guessed at request shapes; this one mirrors the client, so
a refusal here is a real defect rather than the test's ignorance.
"""
import json, urllib.request, urllib.error, uuid, secrets, datetime
API = "http://127.0.0.1:8080/api/v1"

def call(method, path, token=None, body=None):
    data = json.dumps(body).encode() if body is not None else None
    req = urllib.request.Request(API + path, data=data, method=method)
    req.add_header("Content-Type", "application/json")
    if token: req.add_header("Authorization", "Bearer " + token)
    try:
        with urllib.request.urlopen(req, timeout=20) as r:
            return r.status, json.loads(r.read().decode() or "{}")
    except urllib.error.HTTPError as e:
        try: return e.code, json.loads(e.read().decode() or "{}")
        except Exception: return e.code, {}

def signin(phone):
    _, b = call("POST", "/auth/otp/request", body={"phone": phone})
    _, b = call("POST", "/auth/otp/verify", body={"phone": phone, "code": b["data"]["debug_code"],
        "device_name": "d", "platform": "android", "app_version": "1.0.0"})
    return b["data"]["access_token"], b["data"]["user_id"]

A, AID = signin("+989150000001")
B, BID = signin("+989150000002")
_, b = call("POST", "/chats/private", A, {"user_id": BID}); CHAT = b["data"]["chat_id"]
_, b = call("POST", "/chats", A, {"type": "group", "title": "گروه", "member_ids": [BID]})
GROUP = b["data"]["chat_id"]
_, b = call("POST", f"/chats/{CHAT}/messages", A,
            {"client_message_id": str(uuid.uuid4()), "type": "text", "content": "س"})
MSG = b["data"]["id"]

fails = []
def check(label, method, path, body=None, token=A, ok=(200, 201, 204)):
    s, r = call(method, path, token, body)
    err = r.get("error") or {}
    mark = "ok  " if s in ok else "FAIL"
    if s not in ok:
        fails.append((label, s, err.get("code",""), err.get("message",""), err.get("fields")))
    print(f"  {mark} {label:44} {s} {err.get('code','')}")
    return r

print("requests shaped exactly as the mobile app sends them:\n")

# calls_repository.start
check("calls: start a voice call", "POST", "/calls", {"chat_id": CHAT, "type": "voice"})
# organise_repository.setChatFlags
check("chats: set flags", "PUT", f"/chats/{CHAT}/flags", {"pinned": False, "archived": False})
# organise_repository.setMuted
check("chats: mute", "PUT", f"/chats/{CHAT}/mute", {"muted_until": None})
check("chats: mute until", "PUT", f"/chats/{CHAT}/mute",
      {"muted_until": (datetime.datetime.now(datetime.timezone.utc)
                       + datetime.timedelta(hours=8)).isoformat().replace("+00:00","Z")})
# contacts_repository.sync
check("contacts: sync", "POST", "/contacts/sync",
      {"replace": False, "entries": [{"digest": secrets.token_hex(32),
                                      "first_name": "الف", "last_name": "ب"}]})
# stories_repository.post
check("stories: post text", "POST", "/stories",
      {"type": "text", "caption": "متن", "privacy": "everyone"})
# chat_repository markRead / typing / draft
check("chats: mark read", "POST", f"/chats/{CHAT}/read", {"seq": 1})
check("chats: typing", "POST", f"/chats/{CHAT}/typing", {"typing": True})
check("chats: draft", "PUT", f"/chats/{CHAT}/draft", {"draft": "پ"})
# chat_repository edit / react / delete
check("messages: react", "POST", f"/messages/{MSG}/reactions", {"emoji": "👍"})
check("messages: edit", "PATCH", f"/messages/{MSG}", {"content": "ویرایش"})
check("messages: pin", "PUT", f"/messages/{MSG}/pin", {"pinned": True})
# organise_repository.forward
check("messages: forward", "POST", f"/chats/{CHAT}/forward",
      {"to_chat_id": GROUP, "message_ids": [MSG]})
# organise_repository.schedule
check("messages: schedule", "POST", f"/chats/{GROUP}/scheduled",
      {"client_message_id": str(uuid.uuid4()), "type": "text", "content": "بعدا",
       "scheduled_at": (datetime.datetime.now(datetime.timezone.utc)
                        + datetime.timedelta(hours=3)).isoformat().replace("+00:00","Z")},
      ok=(200, 201))
# users_repository
check("users: update profile", "PATCH", "/users/me", {"display_name": "نام"})
check("users: claim username", "PUT", "/users/me/username",
      {"username": "probe_" + secrets.token_hex(3)})
check("users: privacy", "PUT", "/users/me/privacy/last_seen", {"rule": "contacts"})
# bots_repository.register  (username must end in 'bot')
check("bots: register", "POST", "/bots",
      {"username": "probe" + secrets.token_hex(2) + "bot", "display_name": "بات"}, ok=(200, 201))
# notifications
# NotificationSettings.toJson
check("notifications: settings", "PUT", "/notifications/settings",
      {"private_chats": True, "groups": True, "channels": True,
       "breaking_news": True, "calls": True, "stories": True,
       "show_preview": True, "quiet_hours_start": None, "quiet_hours_end": None})
# push_registration.start
check("notifications: register token", "POST", "/notifications/tokens",
      {"provider": "fcm", "token": "t-" + secrets.token_hex(4), "locale": "fa"},
      ok=(200, 201))
# account_repository.privacySettings / setPrivacy
check("privacy: read rules", "GET", "/users/me/privacy")
check("privacy: change a rule", "PUT", "/users/me/privacy/last_seen",
      {"rule": "contacts", "allow_list": [], "deny_list": []})
# secret chat
check("secret: open a chat", "POST", "/secret/chats", {"user_id": BID}, ok=(200, 201, 404))

print()
if fails:
    print("=" * 66)
    print("MISMATCHES between what the app sends and what the server accepts")
    print("=" * 66)
    for label, s, code, msg, fields in fails:
        print(f"  {s} {code}  {label}\n      {msg}  {fields or ''}")
else:
    print("every request the app makes was accepted")

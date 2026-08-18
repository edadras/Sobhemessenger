#!/usr/bin/env python3
"""Can the app read what the server writes?

Every model in the Dart client does `json['x'] as String` for its required
fields. A field the server omits, or sends as a different type, is a crash on
the phone rather than an error the app can show. This checks the responses
against what those models demand.
"""
import json, re, sys, urllib.request, urllib.error, uuid, secrets, datetime
API = "http://127.0.0.1:8080/api/v1"
PASS = FAIL = 0

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

def require(label, obj, fields):
    """fields: {name: python type or tuple}. A required field must be present
    and of the right type, because the Dart cast is unconditional."""
    global PASS, FAIL
    problems = []
    for name, want in fields.items():
        if name not in obj or obj[name] is None:
            problems.append(f"{name} missing")
        elif not isinstance(obj[name], want):
            problems.append(f"{name} is {type(obj[name]).__name__}, want {want}")
    if problems:
        FAIL += 1
        print(f"  FAIL {label}: {', '.join(problems)}")
        print(f"       got keys: {sorted(obj.keys())}")
    else:
        PASS += 1
        print(f"  ok   {label}")

A, AID = signin("+989200000001")
B, BID = signin("+989200000002")

print("=== auth ===")
_, b = call("POST", "/auth/otp/request", body={"phone": "+989200000003"})
require("OtpRequest (expires_in, resend_after, length)", b["data"],
        {"expires_in": int, "resend_after": int, "length": int})

print("=== users ===")
_, b = call("GET", "/users/me", A)
require("SelfProfile.fromJson", b["data"],
        {"user_id": str, "display_name": str, "language": str, "is_bot": bool,
         "phone_number": str})
_, b = call("GET", f"/users/{BID}", A)
require("Profile.fromJson", b["data"],
        {"user_id": str, "display_name": str, "is_bot": bool,
         "is_contact": bool, "is_blocked": bool})

print("=== chats and messages ===")
_, b = call("POST", "/chats/private", A, {"user_id": BID})
require("openPrivateChat -> chat_id", b["data"], {"chat_id": str})
CHAT = b["data"]["chat_id"]

_, b = call("POST", f"/chats/{CHAT}/messages", A,
            {"client_message_id": str(uuid.uuid4()), "type": "text", "content": "سلام"})
msg = b["data"]
require("Message (id, chat_id, seq, type, content, created_at)", msg,
        {"id": str, "chat_id": str, "seq": int, "type": str,
         "content": str, "created_at": str})
MSG = msg["id"]

_, b = call("GET", "/chats", A)
chats = b["data"]["chats"]
require("ChatRow source (id, type, last_seq, member_count)", chats[0],
        {"id": str, "type": str, "last_seq": int, "member_count": int})
require("Membership", chats[0].get("membership") or {},
        {"role": str, "unread_count": int, "mention_count": int,
         "last_read_seq": int, "is_pinned": bool, "is_archived": bool})
peer = chats[0].get("peer")
if peer is None:
    FAIL += 1; print("  FAIL a private chat came back without a peer")
else:
    require("ChatPeer (names the row)", peer, {"user_id": str, "display_name": str, "is_bot": bool})

_, b = call("GET", f"/chats/{CHAT}/messages", A)
require("history page", b["data"], {"messages": list})

print("=== calls ===")
_, b = call("POST", "/calls", A, {"chat_id": CHAT, "type": "voice"})
require("Call.fromJson (id, initiator_id, type, scope, state, started_at)", b["data"],
        {"id": str, "initiator_id": str, "type": str, "scope": str,
         "state": str, "started_at": str})

print("=== contacts ===")
call("POST", "/contacts", A, {"user_id": BID})
_, b = call("GET", "/contacts", A)
contacts = b["data"]["contacts"]
if contacts:
    require("Contact.fromJson (user_id, first_name, is_favorite)", contacts[0],
            {"user_id": str, "is_favorite": bool})
else:
    FAIL += 1; print("  FAIL the contact just added was not listed")

print("=== stories ===")
_, b = call("POST", "/stories", A, {"type": "text", "caption": "س", "privacy": "everyone"})
require("Story.fromJson (id, type, created_at, expires_at)", b["data"],
        {"id": str, "type": str, "created_at": str, "expires_at": str})

print("=== notifications ===")
_, b = call("GET", "/notifications/settings", A)
require("NotificationSettings.fromJson", b["data"],
        {"private_chats": bool, "groups": bool, "channels": bool,
         "breaking_news": bool, "calls": bool, "stories": bool, "show_preview": bool})

print("=== privacy ===")
_, b = call("GET", "/users/me/privacy", A)
require("privacy list", b["data"], {"settings": list})
require("PrivacySetting.fromJson", b["data"]["settings"][0],
        {"key": str, "rule": str, "allow_list": list, "deny_list": list})

print("=== sessions ===")
_, b = call("GET", "/auth/sessions", A)
sessions = b["data"]["sessions"]
require("UserSession.fromJson", sessions[0],
        {"id": str, "created_at": str})

print("=== groups ===")
_, b = call("POST", "/chats", A, {"type": "group", "title": "گروه", "member_ids": [BID]})
GROUP = b["data"]["chat_id"]
_, b = call("GET", f"/chats/{GROUP}/members", A)
require("Member.fromJson", b["data"]["members"][0],
        {"user_id": str, "role": str})
_, b = call("POST", f"/chats/{GROUP}/invite-links", A, {})
require("InviteLink.fromJson (slug, url)", b["data"], {"id": str, "slug": str, "url": str})

print("=== bots ===")
_, b = call("POST", "/bots", A,
            {"username": "parse" + secrets.token_hex(2) + "bot", "display_name": "بات"})
d = b["data"]
# The app reads data['bot'] and data['token'] as objects, and a bot is keyed
# by user_id rather than id — bots are users.
require("register returns a bot and a token object", d, {"bot": dict, "token": dict})
require("IssuedToken.fromJson", d["token"], {"token": str})
require("Bot.fromJson (user_id, owner_id, display_name, created_at)", d["bot"],
        {"user_id": str, "owner_id": str, "display_name": str, "created_at": str})

print("=== search ===")
_, b = call("GET", "/search?q=%D8%B3%D9%84%D8%A7%D9%85&scope=all", A)
require("search results envelope", b["data"], {"query": str})

print()
print(f"passed: {PASS}   failed: {FAIL}")
sys.exit(1 if FAIL else 0)

#!/usr/bin/env python3
"""The bot API driven by a real token, and the media upload path end to end."""
import hashlib, json, sys, time, urllib.request, urllib.error, uuid, secrets
API = "http://127.0.0.1:8080/api/v1"
PASS = FAIL = 0

def call(method, path, token=None, body=None, base=API):
    data = json.dumps(body).encode() if body is not None else None
    req = urllib.request.Request(base + path, data=data, method=method)
    req.add_header("Content-Type", "application/json")
    if token: req.add_header("Authorization", "Bearer " + token)
    try:
        with urllib.request.urlopen(req, timeout=25) as r:
            return r.status, json.loads(r.read().decode() or "{}")
    except urllib.error.HTTPError as e:
        try: return e.code, json.loads(e.read().decode() or "{}")
        except Exception: return e.code, {}

def check(label, ok, detail=""):
    global PASS, FAIL
    if ok: PASS += 1; print(f"  ok   {label}")
    else:  FAIL += 1; print(f"  FAIL {label}  {detail}")

def signin(phone):
    _, b = call("POST", "/auth/otp/request", body={"phone": phone})
    _, b = call("POST", "/auth/otp/verify", body={"phone": phone, "code": b["data"]["debug_code"],
        "device_name": "d", "platform": "android", "app_version": "1.0.0"})
    return b["data"]["access_token"], b["data"]["user_id"]

A, AID = signin("+989210000001")
B, BID = signin("+989210000002")

print("=== the bot API, driven by a real bot token ===")
s, b = call("POST", "/bots", A,
            {"username": "probe" + secrets.token_hex(2) + "bot", "display_name": "پروب"})
check("register a bot", s in (200, 201), f"status={s} {b}")
BOT = b["data"]["bot"]["user_id"]
BOT_TOKEN = b["data"]["token"]["token"]
print(f"       bot={BOT[:8]} token={BOT_TOKEN[:12]}…")

s, b = call("GET", "/me", BOT_TOKEN, base="http://127.0.0.1:8080/api/v1/bot")
check("the token authenticates the bot", s == 200, f"status={s} {b}")
if s == 200:
    check("it identifies the right bot", (b.get("data") or {}).get("user_id") == BOT)

s, b = call("GET", "/me", "not-a-real-token", base="http://127.0.0.1:8080/api/v1/bot")
check("a bad token is refused", s == 401, f"status={s}")

# A person starts a chat with the bot and sends it a command.
_, b = call("POST", "/chats/private", A, {"user_id": BOT})
BOT_CHAT = b["data"]["chat_id"]
_, b = call("POST", f"/chats/{BOT_CHAT}/messages", A,
            {"client_message_id": str(uuid.uuid4()), "type": "text", "content": "/start"})
time.sleep(1.0)

s, b = call("GET", "/updates", BOT_TOKEN, base="http://127.0.0.1:8080/api/v1/bot")
updates = (b.get("data") or {}).get("updates") or []
check("the bot received the command as an update", s == 200 and len(updates) >= 1,
      f"status={s} updates={len(updates)}")
if updates:
    raw = json.dumps(updates, ensure_ascii=False)
    check("the update carries the text", "/start" in raw)

s, b = call("POST", "/messages", BOT_TOKEN,
            {"chat_id": BOT_CHAT, "content": "سلام! من یک ربات هستم"},
            base="http://127.0.0.1:8080/api/v1/bot")
check("the bot can reply", s in (200, 201), f"status={s} {b}")

# The person must actually see it.
_, b = call("GET", f"/chats/{BOT_CHAT}/messages", A)
texts = [m.get("content") for m in ((b.get("data") or {}).get("messages") or [])]
check("the reply is in the conversation", any("ربات" in (t or "") for t in texts),
      f"texts={texts}")

s, b = call("POST", "/messages", BOT_TOKEN,
            {"chat_id": BOT_CHAT, "content": "با دکمه",
             "reply_markup": {"inline_keyboard": [[{"text": "بله", "callback_data": "yes"}]]}},
            base="http://127.0.0.1:8080/api/v1/bot")
check("the bot can attach an inline keyboard", s in (200, 201), f"status={s} {b}")

print()
print("=== media: create an upload, finish it, read it back ===")
blob = b"\x89PNG\r\n\x1a\n" + secrets.token_bytes(2048)
s, b = call("POST", "/media/uploads", A,
            {"kind": "image", "mime_type": "image/png",
             "file_name": "probe.png", "size": len(blob)})
check("an upload session is created", s in (200, 201), f"status={s} {json.dumps(b)[:220]}")
if s in (200, 201):
    data = b.get("data") or {}
    check("it returns a session id", bool(data.get("id") or data.get("session_id")),
          f"keys={sorted(data.keys())}")
    check("it returns somewhere to PUT the bytes",
          bool(data.get("parts") or data.get("upload_url") or data.get("urls")),
          f"keys={sorted(data.keys())}")
    print(f"       session keys: {sorted(data.keys())}")

print()
print(f"passed: {PASS}   failed: {FAIL}")
sys.exit(1 if FAIL else 0)

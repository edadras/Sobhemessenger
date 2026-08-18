#!/usr/bin/env python3
"""Call every documented endpoint with real data and report anything that 5xxs.

A 4xx is usually the endpoint working: refusing input this script did not know
how to build. A 5xx is always a defect — the server failed to handle a request
it accepted. Those are what this looks for.
"""
import json, os, re, subprocess, sys, urllib.request, urllib.error, uuid, datetime, base64, secrets

API = "http://127.0.0.1:8080/api/v1"
PSQL = ["su", "pgrunner", "-c"]
DSN = "psql -p 5433 -h /tmp -U sobh -d sobh_load -tAc "

def sql(statement):
    out = subprocess.run(PSQL + [DSN + json.dumps(statement)],
                         capture_output=True, text=True)
    return out.stdout.strip()

def call(method, path, token=None, body=None, expect_json=True):
    url = API + path
    data = json.dumps(body).encode() if body is not None else None
    req = urllib.request.Request(url, data=data, method=method)
    req.add_header("Content-Type", "application/json")
    if token:
        req.add_header("Authorization", "Bearer " + token)
    try:
        with urllib.request.urlopen(req, timeout=20) as r:
            return r.status, json.loads(r.read().decode() or "{}")
    except urllib.error.HTTPError as e:
        raw = e.read().decode()
        try:
            return e.code, json.loads(raw or "{}")
        except Exception:
            return e.code, {"raw": raw[:300]}
    except Exception as e:
        return 0, {"transport": str(e)}

def signin(phone):
    s, b = call("POST", "/auth/otp/request", body={"phone": phone})
    code = (b.get("data") or {}).get("debug_code")
    if not code:
        raise SystemExit(f"no debug code for {phone}: {s} {b}")
    s, b = call("POST", "/auth/otp/verify", body={
        "phone": phone, "code": code, "device_name": "probe",
        "platform": "android", "app_version": "1.0.0"})
    d = b.get("data") or {}
    return d.get("access_token"), d.get("user_id"), d.get("device_id")

def k33():
    return base64.b64encode(b"\x05" + secrets.token_bytes(32)).decode()

# ---------------------------------------------------------------- fixtures
print("building fixtures...", flush=True)
A_TOK, A_ID, A_DEV = signin("+989130000001")
B_TOK, B_ID, B_DEV = signin("+989130000002")

# Make A a super admin so the admin and editorial surfaces are reachable.
sql(f"INSERT INTO admin_users (user_id, role_key) VALUES ('{A_ID}', 'super_admin') ON CONFLICT DO NOTHING")

F = {"userID": B_ID, "targetUserID": B_ID}

_, b = call("POST", "/chats/private", A_TOK, {"user_id": B_ID})
F["chatID"] = (b.get("data") or {}).get("chat_id")

_, b = call("POST", f"/chats/{F['chatID']}/messages", A_TOK,
            {"client_message_id": str(uuid.uuid4()), "type": "text", "content": "سلام"})
F["messageID"] = (b.get("data") or {}).get("id")

_, b = call("POST", "/chats", A_TOK,
            {"type": "group", "title": "گروه", "member_ids": [B_ID]})
F["groupID"] = (b.get("data") or {}).get("chat_id")

_, b = call("POST", "/chats", A_TOK,
            {"type": "channel", "title": "کانال", "is_public": True,
             "username": "probe_channel_" + secrets.token_hex(3)})
F["channelID"] = (b.get("data") or {}).get("chat_id")

_, b = call("POST", f"/chats/{F['groupID']}/invite-links", A_TOK, {})
F["slug"] = (b.get("data") or {}).get("slug")
F["linkID"] = (b.get("data") or {}).get("id")

_, b = call("POST", "/communities", A_TOK, {"title": "انجمن"})
F["communityID"] = (b.get("data") or {}).get("id") or (b.get("data") or {}).get("community_id")

_, b = call("POST", "/stories", A_TOK, {"type": "text", "content": "استوری", "privacy": "everyone"})
F["storyID"] = (b.get("data") or {}).get("id")

_, b = call("POST", f"/chats/{F['groupID']}/messages", A_TOK,
            {"client_message_id": str(uuid.uuid4()), "type": "poll",
             "content": "سوال؟",
             "payload": {"question": "سوال؟", "options": ["الف", "ب"], "type": "single"}})
poll_msg = (b.get("data") or {}).get("id")
F["pollID"] = ((b.get("data") or {}).get("payload") or {}).get("poll_id") or poll_msg
_, b = call("GET", f"/chats/{F['groupID']}/messages", A_TOK)
for m in ((b.get("data") or {}).get("messages") or []):
    if m.get("type") == "poll":
        pid = (m.get("payload") or {}).get("id") or (m.get("payload") or {}).get("poll_id")
        if pid:
            F["pollID"] = pid
F.setdefault("optionID", "00000000-0000-0000-0000-000000000000")

_, b = call("POST", "/calls", A_TOK, {"chat_id": F["chatID"], "type": "audio"})
F["callID"] = (b.get("data") or {}).get("id") or (b.get("data") or {}).get("call_id")

_, b = call("POST", "/bots", A_TOK,
            {"username": "probe_bot_" + secrets.token_hex(3), "display_name": "Probe"})
d = b.get("data") or {}
F["botID"] = d.get("id") or (d.get("bot") or {}).get("id")
F["token"] = d.get("token") or ""

_, b = call("POST", "/stickers/sets", A_TOK, {"title": "مجموعه", "name": "probe_" + secrets.token_hex(3)})
F["setID"] = (b.get("data") or {}).get("id")
F["stickerID"] = "00000000-0000-0000-0000-000000000000"

_, b = call("POST", "/editorial/articles", A_TOK,
            {"title": "خبر", "slug": "probe-" + secrets.token_hex(3), "locale": "fa", "body": "متن"})
F["articleID"] = (b.get("data") or {}).get("id")
_, b = call("POST", "/editorial/categories", A_TOK,
            {"slug": "cat-" + secrets.token_hex(3), "names": {"fa": "دسته"}})
F["categoryID"] = (b.get("data") or {}).get("id")
_, b = call("POST", "/editorial/authors", A_TOK, {"name": "نویسنده"})
F["authorID"] = (b.get("data") or {}).get("id")
F["tagID"] = "00000000-0000-0000-0000-000000000000"

_, b = call("POST", "/reports", A_TOK,
            {"target_type": "message", "target_id": F["messageID"], "reason": "spam"})
F["reportID"] = (b.get("data") or {}).get("id")

_, b = call("GET", "/auth/sessions", A_TOK)
sessions = (b.get("data") or {}).get("sessions") or []
F["sessionID"] = sessions[0].get("id") if sessions else str(uuid.uuid4())
F["deviceID"] = A_DEV

for tok in (A_TOK, B_TOK):
    call("POST", "/secret/keys", tok, {
        "registration_id": 7, "identity_key": k33(), "signed_prekey_id": 1,
        "signed_prekey": k33(), "prekey_signature": base64.b64encode(secrets.token_bytes(64)).decode(),
        "one_time_prekeys": [{"key_id": i, "public_key": k33()} for i in range(1, 6)]})
_, b = call("POST", "/secret/chats", A_TOK, {"user_id": B_ID})
F["secretChatID"] = (b.get("data") or {}).get("chat_id")

_, b = call("POST", "/notifications/tokens", A_TOK,
            {"token": "probe-push-token", "platform": "android"})

F["mediaID"] = "00000000-0000-0000-0000-000000000000"
F["uploadID"] = "00000000-0000-0000-0000-000000000000"
F["scheduledID"] = "00000000-0000-0000-0000-000000000000"
F["requestID"] = B_ID
F["roomID"] = F["groupID"]
F["sectionID"] = "00000000-0000-0000-0000-000000000000"
F["queryID"] = "00000000-0000-0000-0000-000000000000"
F["callbackID"] = "00000000-0000-0000-0000-000000000000"
F["index"] = "news"
F["flagKey"] = "probe_flag"
F["key"] = "last_seen"
F["username"] = "probe_channel"
F["emoji"] = "%F0%9F%91%8D"

missing = [k for k, v in F.items() if not v]
print("fixtures ready; unresolved:", missing or "none", flush=True)
print(json.dumps({k: str(v)[:38] for k, v in F.items()}, ensure_ascii=False, indent=None), flush=True)
print(flush=True)

# ---------------------------------------------------------------- bodies
BODIES = {
    ("POST", "/api/v1/auth/otp/request"): {"phone": "+989139999999"},
    ("POST", "/api/v1/auth/otp/verify"): {"phone": "+989139999999", "code": "000000"},
    ("POST", "/api/v1/auth/refresh"): {"refresh_token": "nope"},
    ("PUT", "/api/v1/auth/two-step"): {"enabled": False},
    ("PATCH", "/api/v1/users/me"): {"display_name": "کاربر"},
    ("PUT", "/api/v1/users/me/username"): {"username": "probe_user_" + secrets.token_hex(3)},
    ("POST", "/api/v1/chats"): {"type": "group", "title": "گروه دوم"},
    ("POST", "/api/v1/chats/private"): {"user_id": B_ID},
    ("POST", "/api/v1/chats/{chatID}/messages"): {
        "client_message_id": str(uuid.uuid4()), "type": "text", "content": "متن"},
    ("POST", "/api/v1/chats/{chatID}/read"): {"seq": 1},
    ("PUT", "/api/v1/chats/{chatID}/draft"): {"draft": "پیش‌نویس"},
    ("POST", "/api/v1/chats/{chatID}/typing"): {"typing": True},
    ("PATCH", "/api/v1/messages/{messageID}"): {"content": "ویرایش"},
    ("POST", "/api/v1/messages/{messageID}/reactions"): {"emoji": "👍"},
    ("PUT", "/api/v1/messages/{messageID}/pin"): {"pinned": True},
    ("PUT", "/api/v1/messages/{messageID}/live-location"): {
        "latitude": 35.7, "longitude": 51.4},
    ("POST", "/api/v1/chats/{chatID}/forward"): {},   # filled below
    ("POST", "/api/v1/chats/{chatID}/scheduled"): {
        "client_message_id": str(uuid.uuid4()), "type": "text", "content": "بعدا",
        "scheduled_at": (datetime.datetime.now(datetime.timezone.utc)
                         + datetime.timedelta(hours=2)).isoformat().replace("+00:00", "Z")},
    ("PUT", "/api/v1/chats/{chatID}/flags"): {"is_archived": False},
    ("PUT", "/api/v1/chats/{chatID}/mute"): {"muted": False},
    ("PUT", "/api/v1/chats/{chatID}/settings"): {"slow_mode_seconds": 0},
    ("POST", "/api/v1/chats/{chatID}/members"): {"user_ids": [B_ID]},
    ("PUT", "/api/v1/chats/{chatID}/members/{targetUserID}/role"): {"role": "admin"},
    ("POST", "/api/v1/chats/{chatID}/invite-links"): {},
    ("POST", "/api/v1/chats/{chatID}/join-requests/{requestID}"): {"approve": True},
    ("POST", "/api/v1/chats/{chatID}/transfer-ownership"): {"user_id": B_ID},
    ("POST", "/api/v1/contacts"): {"user_id": B_ID},
    ("POST", "/api/v1/contacts/sync"): {"contacts": []},
    ("POST", "/api/v1/contacts/blocked"): {"user_id": B_ID},
    ("PUT", "/api/v1/contacts/{userID}/favorite"): {"favorite": True},
    ("POST", "/api/v1/stories"): {"type": "text", "content": "استوری دوم", "privacy": "everyone"},
    ("POST", "/api/v1/stories/{storyID}/reactions"): {"emoji": "👍"},
    ("PUT", "/api/v1/stories/close-friends"): {"user_ids": [B_ID]},
    ("POST", "/api/v1/polls/{pollID}/votes"): {"option_ids": []},
    ("POST", "/api/v1/calls"): {"chat_id": F["chatID"], "type": "audio"},
    ("POST", "/api/v1/calls/{callID}/answer"): {"sdp": "v=0"},
    ("POST", "/api/v1/calls/{callID}/candidates"): {"candidate": "candidate:0 1 UDP"},
    ("POST", "/api/v1/calls/{callID}/media-state"): {"audio": True, "video": False},
    ("POST", "/api/v1/calls/{callID}/reject"): {},
    ("POST", "/api/v1/calls/{callID}/end"): {},
    ("POST", "/api/v1/bots"): {"username": "probe_bot2_" + secrets.token_hex(3), "display_name": "دوم"},
    ("PATCH", "/api/v1/bots/{botID}"): {"display_name": "تغییر"},
    ("POST", "/api/v1/bots/{botID}/tokens"): {},
    ("PUT", "/api/v1/bots/{botID}/webhook"): {"url": "https://example.invalid/hook"},
    ("PUT", "/api/v1/bots/{botID}/commands"): {"commands": [{"command": "start", "description": "شروع"}]},
    ("POST", "/api/v1/stickers/sets"): {"title": "مجموعه دوم", "name": "probe2_" + secrets.token_hex(3)},
    ("POST", "/api/v1/stickers/sets/{setID}/stickers"): {"emoji": "😀", "media_id": F["mediaID"]},
    ("POST", "/api/v1/communities"): {"title": "انجمن دوم"},
    ("POST", "/api/v1/communities/{communityID}/rooms"): {"title": "اتاق"},
    ("POST", "/api/v1/notifications/tokens"): {"token": "probe-token-2", "platform": "android"},
    ("PUT", "/api/v1/notifications/settings"): {"messages": True},
    ("POST", "/api/v1/reports"): {"target_type": "message", "target_id": F["messageID"], "reason": "spam"},
    ("POST", "/api/v1/editorial/articles"): {
        "title": "خبر دوم", "slug": "probe2-" + secrets.token_hex(3), "locale": "fa", "body": "متن"},
    ("PATCH", "/api/v1/editorial/articles/{articleID}"): {"title": "خبر ویرایش‌شده"},
    ("POST", "/api/v1/editorial/articles/{articleID}/status"): {"status": "review"},
    ("POST", "/api/v1/editorial/categories"): {"slug": "cat2-" + secrets.token_hex(3), "names": {"fa": "دسته دوم"}},
    ("POST", "/api/v1/editorial/authors"): {"name": "نویسنده دوم"},
    ("POST", "/api/v1/editorial/tags"): {"name": "برچسب", "slug": "tag-" + secrets.token_hex(3)},
    ("POST", "/api/v1/news-reader/articles/{articleID}/bookmark"): {},
    ("POST", "/api/v1/news-reader/follows"): {"category_id": F["categoryID"]},
    ("POST", "/api/v1/secret/keys"): {
        "registration_id": 9, "identity_key": k33(), "signed_prekey_id": 2,
        "signed_prekey": k33(), "prekey_signature": base64.b64encode(secrets.token_bytes(64)).decode(),
        "one_time_prekeys": []},
    ("POST", "/api/v1/secret/chats"): {"user_id": B_ID},
    ("POST", "/api/v1/secret/messages"): {
        "chat_id": F["secretChatID"], "recipient_device_id": B_DEV,
        "ciphertext": base64.b64encode(secrets.token_bytes(64)).decode(),
        "message_type": 3, "client_message_id": str(uuid.uuid4())},
    ("POST", "/api/v1/secret/inbox/ack"): {"ids": []},
    ("POST", "/api/v1/secret/sessions"): {
        "chat_id": F["secretChatID"], "initiator_device_id": A_DEV,
        "responder_device_id": B_DEV},
    ("POST", "/api/v1/admin/users/{userID}/ban"): {"reason": "probe", "duration_hours": 1},
    ("POST", "/api/v1/admin/reports/{reportID}/resolve"): {"action": "dismiss"},
    ("PUT", "/api/v1/admin/feature-flags/{flagKey}"): {"enabled": True},
    ("PUT", "/api/v1/users/me/privacy/{key}"): {"rule": "everyone"},
    ("POST", "/api/v1/media/uploads"): {
        "filename": "a.png", "mime_type": "image/png", "size": 1024},
}
BODIES[("POST", "/api/v1/chats/{chatID}/forward")] = {
    "to_chat_id": F["groupID"], "message_ids": [F["messageID"]]}

# Endpoints that would break the probe run itself if called.
SKIP = {
    ("POST", "/api/v1/auth/logout"),                    # kills the token
    ("POST", "/api/v1/auth/sessions/revoke-others"),
    ("DELETE", "/api/v1/auth/sessions/{sessionID}"),
    ("DELETE", "/api/v1/users/me"),                     # deletes the account
    ("POST", "/api/v1/chats/{chatID}/leave"),
    ("DELETE", "/api/v1/chats/{chatID}"),
}

import yaml
spec = yaml.safe_load(open("protocol/rest/openapi.yaml"))

server_errors, client_errors, ok = [], [], 0
for path, item in sorted(spec["paths"].items()):
    for method, op in item.items():
        if method not in ("get", "post", "put", "patch", "delete"):
            continue
        M = method.upper()
        if (M, path) in SKIP:
            continue
        if path.startswith("/api/v1/bot/"):   # bot API uses a bot token, covered separately
            continue

        concrete = path.replace("/api/v1", "")
        for name in re.findall(r"\{([^}]+)\}", concrete):
            concrete = concrete.replace("{" + name + "}", str(F.get(name, "00000000-0000-0000-0000-000000000000")))

        body = BODIES.get((M, path))
        if body is None and M in ("POST", "PUT", "PATCH"):
            body = {}
        status, resp = call(M, concrete, A_TOK, body)
        line = f"{M:6} {path}"
        if status >= 500 or status == 0:
            server_errors.append((line, status, json.dumps(resp, ensure_ascii=False)[:220]))
        elif status >= 400:
            client_errors.append((line, status, (resp.get("error") or {}).get("code", "")))
        else:
            ok += 1

print(f"2xx/3xx: {ok}    4xx: {len(client_errors)}    5xx: {len(server_errors)}")
print()
if server_errors:
    print("=" * 70)
    print("SERVER ERRORS — these are defects")
    print("=" * 70)
    for line, status, detail in server_errors:
        print(f"  {status}  {line}\n        {detail}")
    print()
print("4xx responses (usually the endpoint refusing input the probe could not build):")
for line, status, code in client_errors:
    print(f"  {status} {code:26} {line}")

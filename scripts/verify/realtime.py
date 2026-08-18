#!/usr/bin/env python3
"""Realtime delivery: the thing a messenger is for.

Two people, two sockets, and the question of whether what one sends actually
reaches the other — over the socket, in time, once, and again after a
reconnection that missed it.
"""
import json, sys, time, urllib.request, urllib.error, uuid
sys.path.insert(0, "/tmp/claude-0/-home-user-Sobhemessenger/2053d389-5696-5666-91ed-c3cba7290c7f/scratchpad")
from ws_client import WS

API = "http://127.0.0.1:8080/api/v1"
PASS, FAIL = 0, 0

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

def check(label, ok, detail=""):
    global PASS, FAIL
    if ok: PASS += 1; print(f"  ok   {label}")
    else:  FAIL += 1; print(f"  FAIL {label}  {detail}")

A, AID = signin("+989180000001")
B, BID = signin("+989180000002")
_, b = call("POST", "/chats/private", A, {"user_id": BID})
CHAT = b["data"]["chat_id"]

print("=== a message reaches the other person over the socket ===")
wa, wb = WS(A), WS(B)
check("A's socket announces the connection", wa.wait_for("connected") is not None)
check("B's socket announces the connection", wb.wait_for("connected") is not None)

# Typing, read receipts and reactions go to the chat subject rather than each
# member's log — only people with the conversation open need them — so a socket
# has to subscribe, exactly as the app does when a chat is opened.
wa.send({"id": "s1", "event": "chat.subscribe", "payload": {"chat_id": CHAT}})
wb.send({"id": "s2", "event": "chat.subscribe", "payload": {"chat_id": CHAT}})
time.sleep(0.4)

cid = str(uuid.uuid4())
t0 = time.time()
status, body = call("POST", f"/chats/{CHAT}/messages", A,
                    {"client_message_id": cid, "type": "text", "content": "پیام آزمایشی"})
check("the send was accepted", status in (200, 201), f"status={status}")
sent_id = (body.get("data") or {}).get("id")

frame = wb.wait_for("message.new", timeout=12)
delay_ms = (time.time() - t0) * 1000
check("B received message.new", frame is not None)
if frame:
    payload = frame.get("payload") or {}
    msg = payload.get("message") or {}
    check("it is the message A sent", msg.get("id") == sent_id,
          f"got {msg.get('id')} want {sent_id}")
    check("it carries the text", msg.get("content") == "پیام آزمایشی",
          f"got {msg.get('content')!r}")
    check("it names the chat", payload.get("chat_id") == CHAT)
    check(f"delivered in {delay_ms:.0f} ms, under the 500 ms budget (§79)", delay_ms < 500,
          f"{delay_ms:.0f} ms")
    check("the frame carries a sync cursor", frame.get("sync_seq") is not None,
          f"sync_seq={frame.get('sync_seq')}")

print()
print("=== the sender's own other devices see it too ===")
frame_a = wa.wait_for("message.new", timeout=8)
check("A's socket also received it, for A's other devices", frame_a is not None)

print()
print("=== typing, read receipts and reactions travel ===")
call("POST", f"/chats/{CHAT}/typing", B, {"typing": True})
check("A saw B start typing", wa.wait_for("typing.start", timeout=8) is not None)

call("POST", f"/chats/{CHAT}/read", B, {"seq": 1})
check("A saw the read receipt", wa.wait_for("message.read", timeout=8) is not None)

call("POST", f"/messages/{sent_id}/reactions", B, {"emoji": "👍"})
check("A saw the reaction", wa.wait_for("message.reaction", timeout=8) is not None)

call("PATCH", f"/messages/{sent_id}", A, {"content": "ویرایش شد"})
check("B saw the edit", wb.wait_for("message.edited", timeout=8) is not None)

call("DELETE", f"/messages/{sent_id}", A)
check("B saw the deletion", wb.wait_for("message.deleted", timeout=8) is not None)

print()
print("=== a device that was offline catches up ===")
wb.close()
missed = []
for i in range(3):
    _, r = call("POST", f"/chats/{CHAT}/messages", A,
                {"client_message_id": str(uuid.uuid4()), "type": "text",
                 "content": f"وقتی آفلاین بودی {i}"})
    missed.append((r.get("data") or {}).get("id"))
time.sleep(0.5)

status, body = call("GET", "/sync?cursor=0&limit=200", B)
events = (body.get("data") or {}).get("events") or []
check("the sync log replays what was missed", status == 200 and len(events) >= 3,
      f"status={status} events={len(events)}")
ids = json.dumps(events, ensure_ascii=False)
check("every missed message is in the replay",
      all(mid and mid in ids for mid in missed),
      f"{sum(1 for m in missed if m and m in ids)}/{len(missed)} found")

wb2 = WS(B)
connected = wb2.wait_for("connected")
check("reconnecting reports a cursor to resume from",
      connected is not None and (connected.get("payload") or {}).get("sync_cursor") is not None)

print()
print("=== a stranger cannot listen in ===")
_, b = call("POST", "/auth/otp/request", body={"phone": "+989180000003"})
_, b = call("POST", "/auth/otp/verify", body={"phone": "+989180000003", "code": b["data"]["debug_code"],
    "device_name": "d", "platform": "android", "app_version": "1.0.0"})
C = b["data"]["access_token"]
wc = WS(C)
wc.wait_for("connected")
call("POST", f"/chats/{CHAT}/messages", A,
     {"client_message_id": str(uuid.uuid4()), "type": "text", "content": "خصوصی"})
leaked = wc.wait_for("message.new", timeout=5)
check("a non-member's socket received nothing", leaked is None,
      f"leaked: {json.dumps(leaked, ensure_ascii=False)[:160]}" if leaked else "")

try:
    WS("not-a-real-token")
    check("an invalid token is refused at the handshake", False, "it connected")
except Exception:
    check("an invalid token is refused at the handshake", True)

for w in (wa, wb2, wc):
    w.close()

print()
print(f"passed: {PASS}   failed: {FAIL}")
sys.exit(1 if FAIL else 0)

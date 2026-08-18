#!/bin/bash
# Real requests against the running API. Every failure is printed with the
# response body, because a smoke test that only reports "failed" is no use.
API=http://127.0.0.1:8080/api/v1
PASS=0; FAIL=0
declare -a PROBLEMS

j() { python3 -c "import sys,json;d=json.load(sys.stdin);print(json.dumps(d,ensure_ascii=False))" 2>/dev/null; }
field() { FIELD_PATH="$1" python3 -c '
import sys, json, os
try:
    d = json.load(sys.stdin)
except Exception:
    print(""); raise SystemExit
for k in os.environ["FIELD_PATH"].split("."):
    d = d.get(k) if isinstance(d, dict) else None
    if d is None:
        break
print(d if d is not None else "")' 2>/dev/null; }

check() { # name expected_status actual_status body
  if [ "$2" = "$3" ]; then PASS=$((PASS+1)); printf '  ok   %-52s %s\n' "$1" "$3"
  else FAIL=$((FAIL+1)); printf '  FAIL %-52s got %s want %s\n' "$1" "$3" "$2"; PROBLEMS+=("$1: got $3 want $2 :: $(echo "$4" | head -c 300)"); fi
}

req() { # method path token body -> sets STATUS and BODY
  local m=$1 p=$2 t=$3 b=$4
  local args=(-s -o /tmp/rbody -w '%{http_code}' -X "$m" "$API$p" -H 'Content-Type: application/json')
  [ -n "$t" ] && args+=(-H "Authorization: Bearer $t")
  [ -n "$b" ] && args+=(-d "$b")
  STATUS=$(curl "${args[@]}")
  BODY=$(cat /tmp/rbody)
}

signin() { # phone -> echoes access token
  local phone=$1
  req POST /auth/otp/request "" "{\"phone\":\"$phone\"}"
  local code
  code=$(echo "$BODY" | field data.debug_code)
  req POST /auth/otp/verify "" "{\"phone\":\"$phone\",\"code\":\"$code\",\"device_name\":\"smoke\",\"platform\":\"android\",\"app_version\":\"1.0.0\"}"
  echo "$BODY" | field data.access_token
}

echo "=== auth ==="
# A phone of its own: asking twice for the same number inside the resend
# window is correctly refused, and reusing one here would break sign-in below.
OTP_PHONE="+98912$(python3 -c "import secrets;print(''.join(secrets.choice('0123456789') for _ in range(7)))")"
req POST /auth/otp/request "" "{\"phone\":\"$OTP_PHONE\"}"
check "request an OTP" 200 "$STATUS" "$BODY"
echo "     otp response: $(echo "$BODY" | head -c 200)"

# Fresh numbers each run. Fixed ones meant every run reused the same two
# accounts, so state left behind by an earlier run — a secret chat that already
# existed, a bot username already claimed — made checks fail for reasons that
# had nothing to do with the code under test.
NONCE=$(python3 -c "import secrets;print(''.join(secrets.choice('0123456789') for _ in range(7)))")
A=$(signin "+98912$NONCE")
NONCE=$(python3 -c "import secrets;print(''.join(secrets.choice('0123456789') for _ in range(7)))")
B=$(signin "+98912$NONCE")
if [ -n "$A" ] && [ -n "$B" ]; then PASS=$((PASS+1)); echo "  ok   two users signed in"
else FAIL=$((FAIL+1)); echo "  FAIL sign-in produced no token"; PROBLEMS+=("sign-in produced no token :: $BODY"); fi

req GET /users/me "$A" ""
check "read own profile" 200 "$STATUS" "$BODY"
AID=$(echo "$BODY" | field data.user_id)
req GET /users/me "$B" ""
BID=$(echo "$BODY" | field data.user_id)
echo "     A=$AID  B=$BID"

echo "=== chats ==="
req GET /chats "$A" ""
check "list chats (empty)" 200 "$STATUS" "$BODY"

req POST /chats/private "$A" "{\"user_id\":\"$BID\"}"
check "open a private chat" 200 "$STATUS" "$BODY"
CHAT=$(echo "$BODY" | field data.chat_id)

req GET /chats "$A" ""
check "list chats after opening one" 200 "$STATUS" "$BODY"
PEER=$(echo "$BODY" | python3 -c "
import sys,json
d=json.load(sys.stdin)['data']['chats']
print(d[0].get('peer',{}).get('user_id','MISSING') if d else 'NOCHATS')" 2>/dev/null)
if [ "$PEER" = "$BID" ]; then PASS=$((PASS+1)); echo "  ok   the chat list names the peer"
else FAIL=$((FAIL+1)); echo "  FAIL peer in list = $PEER, want $BID"; PROBLEMS+=("chat list peer wrong: $PEER"); fi

echo "=== messages ==="
CM=$(python3 -c "import uuid;print(uuid.uuid4())")
req POST "/chats/$CHAT/messages" "$A" "{\"client_message_id\":\"$CM\",\"type\":\"text\",\"content\":\"سلام\"}"
check "send a message" 201 "$STATUS" "$BODY"
MSG=$(echo "$BODY" | field data.id)

req POST "/chats/$CHAT/messages" "$A" "{\"client_message_id\":\"$CM\",\"type\":\"text\",\"content\":\"سلام\"}"
check "resend is idempotent" 201 "$STATUS" "$BODY"

req GET "/chats/$CHAT/messages" "$A" ""
check "read history" 200 "$STATUS" "$BODY"

req POST "/chats/$CHAT/read" "$B" '{"seq":1}'
check "mark read" 200 "$STATUS" "$BODY"

req POST "/chats/$CHAT/typing" "$A" '{"typing":true}'
check "typing indicator" 200 "$STATUS" "$BODY"

req PUT "/chats/$CHAT/draft" "$A" '{"draft":"half a message"}'
check "save a draft" 200 "$STATUS" "$BODY"

req POST "/messages/$MSG/reactions" "$B" '{"emoji":"👍"}'
check "react to a message" 200 "$STATUS" "$BODY"

req PATCH "/messages/$MSG" "$A" '{"content":"سلام دنیا"}'
check "edit own message" 200 "$STATUS" "$BODY"

req PUT "/messages/$MSG/pin" "$A" '{"pinned":true}'
check "pin a message" 200 "$STATUS" "$BODY"

req GET "/chats/$CHAT/pinned" "$A" ""
check "list pinned" 200 "$STATUS" "$BODY"

echo "=== search ==="
req GET "/search?q=%D8%B3%D9%84%D8%A7%D9%85&scope=all" "$A" ""
check "search runs" 200 "$STATUS" "$BODY"

echo "=== secret chats ==="
req POST /secret/chats "$A" "{\"user_id\":\"$BID\"}"
check "secret chat refused without keys" 404 "$STATUS" "$BODY"

K33=$(python3 -c "import base64,os;print(base64.b64encode(b'\x05'+os.urandom(32)).decode())")
K64=$(python3 -c "import base64,os;print(base64.b64encode(os.urandom(64)).decode())")
OT=$(python3 -c "
import base64,os,json
print(json.dumps([{'key_id':i,'public_key':base64.b64encode(b'\x05'+os.urandom(32)).decode()} for i in range(1,6)]))")
for T in "$A" "$B"; do
  req POST /secret/keys "$T" "{\"registration_id\":42,\"identity_key\":\"$K33\",\"signed_prekey_id\":1,\"signed_prekey\":\"$K33\",\"prekey_signature\":\"$K64\",\"one_time_prekeys\":$OT}"
  check "publish key material" 200 "$STATUS" "$BODY"
done

req POST /secret/chats "$A" "{\"user_id\":\"$BID\"}"
check "open a secret chat" 200 "$STATUS" "$BODY"
SECRET=$(echo "$BODY" | field data.chat_id)

req POST /secret/chats "$A" "{\"user_id\":\"$BID\"}"
CREATED=$(echo "$BODY" | field data.created)
if [ "$CREATED" = "False" ] || [ "$CREATED" = "false" ]; then PASS=$((PASS+1)); echo "  ok   opening twice reuses the chat"
else FAIL=$((FAIL+1)); echo "  FAIL second open reported created=$CREATED"; PROBLEMS+=("secret chat not idempotent"); fi

req POST /secret/chats "$A" "{\"user_id\":\"$AID\"}"
check "secret chat with yourself refused" 422 "$STATUS" "$BODY"

CM2=$(python3 -c "import uuid;print(uuid.uuid4())")
req POST "/chats/$SECRET/messages" "$A" "{\"client_message_id\":\"$CM2\",\"type\":\"text\",\"content\":\"plaintext\"}"
check "plaintext into a secret chat refused" 422 "$STATUS" "$BODY"

req PUT "/chats/$SECRET/draft" "$A" '{"draft":"half a secret"}'
check "draft in a secret chat refused" 403 "$STATUS" "$BODY"

req GET "/secret/keys/$BID" "$A" ""
check "claim a key bundle" 200 "$STATUS" "$BODY"
DEV=$(echo "$BODY" | python3 -c "import sys,json;b=json.load(sys.stdin)['data']['bundles'];print(b[0]['device_id'] if b else '')" 2>/dev/null)
SPI=$(echo "$BODY" | python3 -c "import sys,json;b=json.load(sys.stdin)['data']['bundles'];print(b[0].get('signed_prekey_id','MISSING') if b else '')" 2>/dev/null)
if [ "$SPI" = "1" ]; then PASS=$((PASS+1)); echo "  ok   the bundle carries signed_prekey_id"
else FAIL=$((FAIL+1)); echo "  FAIL signed_prekey_id = $SPI"; PROBLEMS+=("bundle missing signed_prekey_id: $SPI"); fi

CM3=$(python3 -c "import uuid;print(uuid.uuid4())")
CIPHER=$(python3 -c "import base64,os;print(base64.b64encode(os.urandom(96)).decode())")
req POST /secret/messages "$A" "{\"chat_id\":\"$SECRET\",\"recipient_device_id\":\"$DEV\",\"ciphertext\":\"$CIPHER\",\"message_type\":3,\"client_message_id\":\"$CM3\"}"
check "send ciphertext" 201 "$STATUS" "$BODY"

req GET /secret/inbox "$B" ""
check "read the encrypted inbox" 200 "$STATUS" "$BODY"
ENV=$(echo "$BODY" | python3 -c "import sys,json;e=json.load(sys.stdin)['data']['envelopes'];print(len(e))" 2>/dev/null)
if [ "$ENV" = "1" ]; then PASS=$((PASS+1)); echo "  ok   the envelope arrived"
else FAIL=$((FAIL+1)); echo "  FAIL inbox held $ENV envelopes, want 1"; PROBLEMS+=("secret inbox empty"); fi

echo "=== groups ==="
req POST /chats "$A" "{\"type\":\"group\",\"title\":\"گروه آزمایشی\",\"member_ids\":[\"$BID\"]}"
check "create a group" 201 "$STATUS" "$BODY"
GROUP=$(echo "$BODY" | field data.chat_id)
req GET "/chats/$GROUP/members" "$A" ""
check "list members" 200 "$STATUS" "$BODY"
req POST "/chats/$GROUP/invite-links" "$A" '{}'
check "create an invite link" 201 "$STATUS" "$BODY"

echo "=== forward and schedule ==="
req POST "/chats/$CHAT/forward" "$A" "{\"to_chat_id\":\"$GROUP\",\"message_ids\":[\"$MSG\"]}"
check "forward into a group" 201 "$STATUS" "$BODY"
WHEN=$(python3 -c "import datetime;print((datetime.datetime.now(datetime.timezone.utc)+datetime.timedelta(hours=1)).isoformat().replace('+00:00','Z'))")
CM4=$(python3 -c "import uuid;print(uuid.uuid4())")
req POST "/chats/$GROUP/scheduled" "$A" "{\"client_message_id\":\"$CM4\",\"type\":\"text\",\"content\":\"later\",\"scheduled_at\":\"$WHEN\"}"
check "schedule a message" 201 "$STATUS" "$BODY"
req GET "/chats/$GROUP/scheduled" "$A" ""
check "list scheduled" 200 "$STATUS" "$BODY"

echo "=== bots ==="
BOT_NAME="smoke$(python3 -c "import secrets;print(secrets.token_hex(3))")_bot"
req POST /bots "$A" "{\"username\":\"$BOT_NAME\",\"display_name\":\"Smoke\"}"
check "register a bot" 201 "$STATUS" "$BODY"

echo "=== contacts, stories, news, notifications ==="
req GET /contacts "$A" ""; check "list contacts" 200 "$STATUS" "$BODY"
req GET /stories "$A" ""; check "list stories" 200 "$STATUS" "$BODY"
req GET /news "$A" ""; check "news feed" 200 "$STATUS" "$BODY"
req GET /notifications/settings "$A" ""; check "notification settings" 200 "$STATUS" "$BODY"
req GET /auth/sessions "$A" ""; check "active sessions" 200 "$STATUS" "$BODY"
req DELETE "/messages/$MSG" "$A" ""; check "delete a message" 200 "$STATUS" "$BODY"

echo
echo "=================================================="
echo "passed: $PASS   failed: $FAIL"
if [ ${#PROBLEMS[@]} -gt 0 ]; then
  echo
  echo "problems:"
  for p in "${PROBLEMS[@]}"; do echo "  - $p"; done
fi
exit $([ "$FAIL" -eq 0 ] && echo 0 || echo 1)

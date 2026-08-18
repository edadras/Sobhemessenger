#!/bin/bash
# The worker's wiring, and one piece of its work observed end to end.
SCRATCH=/tmp/claude-0/-home-user-Sobhemessenger/2053d389-5696-5666-91ed-c3cba7290c7f/scratchpad
API=http://127.0.0.1:8080/api/v1
PASS=0; FAIL=0
ok(){ PASS=$((PASS+1)); echo "  ok   $1"; }
bad(){ FAIL=$((FAIL+1)); echo "  FAIL $1  $2"; }

LOG_LEVEL=info "$SCRATCH/sobh-worker" >/tmp/sobh-worker.log 2>&1 &
WPID=$!
sleep 6

kill -0 "$WPID" 2>/dev/null && ok "the worker is running" || bad "the worker died"
BOUND=$(grep -c "queue consumer bound" /tmp/sobh-worker.log)
[ "$BOUND" -eq 6 ] && ok "all six queue consumers bound" || bad "consumers bound" "$BOUND of 6"
grep -o '"durable":"[a-z-]*"' /tmp/sobh-worker.log | sed 's/^/       /'
ERRS=$(grep -c '"level":"ERROR"' /tmp/sobh-worker.log)
[ "$ERRS" -eq 0 ] && ok "no errors on start" || bad "errors on start" "$ERRS"

# A scheduled post whose time has passed must become a real message, which only
# the worker's scheduler loop can do.
sign() {
  C=$(curl -s -X POST "$API/auth/otp/request" -H 'Content-Type: application/json' -d "{\"phone\":\"$1\"}" \
      | python3 -c "import sys,json;print(json.load(sys.stdin)['data']['debug_code'])")
  curl -s -X POST "$API/auth/otp/verify" -H 'Content-Type: application/json' \
    -d "{\"phone\":\"$1\",\"code\":\"$C\",\"device_name\":\"w\",\"platform\":\"android\",\"app_version\":\"1.0.0\"}"
}
# Fresh numbers each run. Reusing fixed ones meant the same two accounts and
# therefore the same conversation, so a post published by an earlier run
# satisfied the search below before this run's post had gone anywhere — the
# check passed while proving nothing.
SUFFIX=$(python3 -c "import secrets;print(''.join(secrets.choice('0123456789') for _ in range(7)))")
A=$(sign "+98923$SUFFIX"); ATOK=$(echo "$A" | python3 -c "import sys,json;print(json.load(sys.stdin)['data']['access_token'])")
SUFFIX=$(python3 -c "import secrets;print(''.join(secrets.choice('0123456789') for _ in range(7)))")
B=$(sign "+98923$SUFFIX"); BID=$(echo "$B" | python3 -c "import sys,json;print(json.load(sys.stdin)['data']['user_id'])")
CHAT=$(curl -s -X POST "$API/chats/private" -H "Authorization: Bearer $ATOK" -H 'Content-Type: application/json' \
        -d "{\"user_id\":\"$BID\"}" | python3 -c "import sys,json;print(json.load(sys.stdin)['data']['chat_id'])")

WHEN=$(python3 -c "import datetime;print((datetime.datetime.now(datetime.timezone.utc)+datetime.timedelta(minutes=5)).isoformat().replace('+00:00','Z'))")
CM=$(python3 -c "import uuid;print(uuid.uuid4())")
# The nonce is what makes the wait below prove this run's post arrived rather
# than finding one from any other.
NONCE=$(python3 -c "import secrets;print(secrets.token_hex(4))")
SCHED=$(curl -s -X POST "$API/chats/$CHAT/scheduled" -H "Authorization: Bearer $ATOK" -H 'Content-Type: application/json' \
  -d "{\"client_message_id\":\"$CM\",\"type\":\"text\",\"content\":\"از زمان‌بندی آمد $NONCE\",\"scheduled_at\":\"$WHEN\"}")
SID=$(echo "$SCHED" | python3 -c "import sys,json;print(json.load(sys.stdin)['data']['id'])" 2>/dev/null)
[ -n "$SID" ] && ok "a post was scheduled" || bad "scheduling" "$SCHED"

# Move its time into the past so the next tick claims it.
su pgrunner -c "psql -p 5433 -h /tmp -U sobh -d sobh_load -tAc \"UPDATE scheduled_messages SET scheduled_at = now() - interval '1 minute' WHERE id = '$SID'\"" >/dev/null

echo "       waiting for the scheduler tick…"
FOUND=""
for i in $(seq 1 24); do
  sleep 5
  FOUND=$(curl -s "$API/chats/$CHAT/messages" -H "Authorization: Bearer $ATOK" \
          | NONCE="$NONCE" python3 -c "import os,sys,json;print('yes' if any(os.environ['NONCE'] in (m.get('content') or '') for m in json.load(sys.stdin)['data']['messages']) else '')" 2>/dev/null)
  [ -n "$FOUND" ] && break
done
[ -n "$FOUND" ] && ok "the worker published the scheduled post into the conversation" \
                || bad "the scheduled post never appeared" "after $((i*5))s"

PUBLISHED=$(su pgrunner -c "psql -p 5433 -h /tmp -U sobh -d sobh_load -tAc \"SELECT published_message_id IS NOT NULL FROM scheduled_messages WHERE id='$SID'\"")
[ "$PUBLISHED" = "t" ] && ok "the queue row records what it became" || bad "queue row not marked published" "$PUBLISHED"

echo "  worker errors during the run: $(grep -c '\"level\":\"ERROR\"' /tmp/sobh-worker.log)"
kill "$WPID" 2>/dev/null
echo
echo "passed: $PASS   failed: $FAIL"

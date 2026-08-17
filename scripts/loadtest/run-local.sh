#!/usr/bin/env bash
#
# Runs the load test against a locally assembled stack.
#
# The compose file is the normal way to bring SOBH up. This script exists for
# environments where a Docker daemon is not available — a CI runner without
# privileged mode, or a developer machine already running the dependencies —
# and starts the same five processes directly.
#
#   scripts/loadtest/run-local.sh                 # smoke stage
#   scripts/loadtest/run-local.sh 10k             # the first §78 stage
#
# It needs on PATH: k6, redis-server, postgres, minio, nats-server.
set -euo pipefail

STAGE="${1:-smoke}"
ACCOUNT_POOL="${ACCOUNT_POOL:-20}"
REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
WORK_DIR="$(mktemp -d)"

# Ports chosen to avoid a developer's own services.
PG_PORT="${PG_PORT:-5433}"
REDIS_PORT="${REDIS_PORT:-6399}"
NATS_PORT="${NATS_PORT:-4232}"
MINIO_PORT="${MINIO_PORT:-9010}"
API_PORT="${API_PORT:-8080}"

pids=()
cleanup() {
    for pid in "${pids[@]:-}"; do
        kill "$pid" 2>/dev/null || true
    done
    rm -rf "$WORK_DIR"
}
trap cleanup EXIT INT TERM

require() {
    command -v "$1" >/dev/null 2>&1 || {
        echo "missing dependency: $1" >&2
        exit 1
    }
}
for tool in k6 redis-server minio nats-server go; do
    require "$tool"
done

echo "==> starting dependencies"
redis-server --port "$REDIS_PORT" --save '' --appendonly no --daemonize yes
pids+=($!)

MINIO_ROOT_USER=sobhload MINIO_ROOT_PASSWORD=sobhloadsecret \
    minio server "$WORK_DIR/minio" --address "127.0.0.1:$MINIO_PORT" \
    >"$WORK_DIR/minio.log" 2>&1 &
pids+=($!)

nats-server --port "$NATS_PORT" --jetstream --store_dir "$WORK_DIR/jetstream" \
    >"$WORK_DIR/nats.log" 2>&1 &
pids+=($!)

# The load test seeds its own accounts, so it needs a database of its own
# rather than one carrying yesterday's run.
export POSTGRES_DSN="${POSTGRES_DSN:-postgres://postgres@localhost:$PG_PORT/sobh_load?sslmode=disable}"
export REDIS_ADDR="127.0.0.1:$REDIS_PORT"
export NATS_URL="nats://127.0.0.1:$NATS_PORT"
export MINIO_ENDPOINT="127.0.0.1:$MINIO_PORT"
export MINIO_ACCESS_KEY=sobhload
export MINIO_SECRET_KEY=sobhloadsecret
export JWT_SIGNING_KEYS="load:$(head -c 32 /dev/urandom | base64)"
export JWT_ACTIVE_KEY_ID=load
export PHONE_HASH_PEPPER="$(head -c 32 /dev/urandom | base64)"
export SMS_PROVIDER=log
# The harness seeds accounts through the real OTP flow, which needs the code
# back in the response. Configuration refuses this in production.
export SMS_ECHO_CODES=true
export OPENSEARCH_ENABLED=false
export SOBH_ENV=development
export HTTP_ADDR=":$API_PORT"
export LOG_LEVEL=warn

# The limiter is correct to reject a synthetic flood from a small account pool;
# it has its own tests. Raising it here keeps the run measuring the messaging
# path rather than the limiter.
export RL_MESSAGES_PER_MIN=1000000
export RL_API_PER_USER_MIN=1000000
export RL_API_PER_IP_MIN=1000000
export RL_OTP_PER_PHONE_HOUR=1000000
export RL_OTP_PER_IP_HOUR=1000000
export RL_LOGIN_PER_IP_HOUR=1000000

echo "==> applying migrations"
(cd "$REPO_ROOT/backend" && go run ./cmd/migrate -dir ../database/migrations up >/dev/null)

echo "==> starting the API"
(cd "$REPO_ROOT/backend" && go run ./cmd/api >"$WORK_DIR/api.log" 2>&1) &
pids+=($!)

for _ in $(seq 1 60); do
    if curl -fsS "http://127.0.0.1:$API_PORT/ready" >/dev/null 2>&1; then
        break
    fi
    sleep 1
done
if ! curl -fsS "http://127.0.0.1:$API_PORT/ready" >/dev/null 2>&1; then
    echo "the API never became ready:" >&2
    tail -20 "$WORK_DIR/api.log" >&2
    exit 1
fi

echo "==> running k6 (stage $STAGE)"
k6 run \
    -e "STAGE=$STAGE" \
    -e "ACCOUNT_POOL=$ACCOUNT_POOL" \
    -e "BASE_URL=http://127.0.0.1:$API_PORT" \
    "$REPO_ROOT/scripts/loadtest/messaging.js"

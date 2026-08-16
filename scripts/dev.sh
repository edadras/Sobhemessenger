#!/usr/bin/env bash
#
# Developer helper: bring the stack up, apply migrations and seed.
#
#   ./scripts/dev.sh up       start everything
#   ./scripts/dev.sh migrate  apply migrations
#   ./scripts/dev.sh seed     load the baseline seed
#   ./scripts/dev.sh test     run the backend test suite
#   ./scripts/dev.sh down     stop everything

set -Eeuo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
COMPOSE="docker compose -f ${ROOT}/infrastructure/docker/docker-compose.yml"
DSN="${POSTGRES_DSN:-postgres://sobh:sobh_dev_password@localhost:5432/sobh?sslmode=disable}"

case "${1:-up}" in
  up)
    [[ -f "${ROOT}/.env" ]] || { cp "${ROOT}/.env.example" "${ROOT}/.env"; echo "created .env from the template"; }
    $COMPOSE up -d
    echo "API:      http://localhost:8080"
    echo "Metrics:  http://localhost:9090/metrics"
    echo "Grafana:  http://localhost:3000"
    ;;
  migrate)
    cd "${ROOT}/backend" && POSTGRES_DSN="$DSN" go run ./cmd/migrate -dir ../database/migrations up
    ;;
  seed)
    psql "$DSN" -f "${ROOT}/database/seeds/0001_baseline.sql"
    ;;
  test)
    cd "${ROOT}/backend" && SOBH_TEST_POSTGRES_DSN="$DSN" go test -race ./...
    ;;
  down)
    $COMPOSE down
    ;;
  *)
    echo "usage: $0 {up|migrate|seed|test|down}" >&2
    exit 2
    ;;
esac

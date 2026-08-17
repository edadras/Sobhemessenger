# Running SOBH

How to bring the system up, what every setting means, and what to do when
something breaks. Written for someone who did not build it.

Everything here refers to files in this repository. Where a command is given it
has been run; where a step depends on infrastructure this repository does not
contain (a DNS record, a push certificate) that is said plainly rather than
glossed over.

- [What the system is made of](#what-the-system-is-made-of)
- [Running it locally](#running-it-locally)
- [Configuration](#configuration)
- [Secrets, and generating them](#secrets-and-generating-them)
- [Deploying to Kubernetes](#deploying-to-kubernetes)
- [Migrations](#migrations)
- [Backup](#backup)
- [Disaster recovery](#disaster-recovery)
- [Monitoring, and what the alerts mean](#monitoring-and-what-the-alerts-mean)
- [Routine operations](#routine-operations)

---

## What the system is made of

Two binaries out of one codebase, and five stateful dependencies.

| Component | What it does | Losing it means |
|---|---|---|
| `sobh-api` | HTTP and WebSocket. Stateless; scale horizontally. | Nothing works. Roll back or scale up. |
| `sobh-worker` | Media processing, push delivery, search indexing, maintenance. | Messages still send; media stays unprocessed and push stops. |
| PostgreSQL | Every durable fact. The source of truth. | Total outage, and the only loss that is not recoverable from elsewhere. |
| Redis | Rate limits, presence, ephemeral caches. | Rate limits fail **closed** — sign-in and sending stop. Treat as critical. |
| NATS JetStream | Cross-node realtime fan-out and the durable job queue. | Realtime degrades to reconnect-and-resync; queued jobs pause, not lost. |
| MinIO / S3 | Media objects. | Uploads and media playback fail; text messaging continues. |
| OpenSearch | Search. Optional — `OPENSEARCH_ENABLED=false` turns it off. | Search returns empty results. Nothing else is affected. |

The design that matters for operations: **the event log in PostgreSQL is the
record of truth and clients resume from a cursor.** Cross-node fan-out over NATS
is at-most-once, so a node dying loses frames, never messages. This is why NATS
is recoverable and PostgreSQL is not.

---

## Running it locally

```sh
cp .env.example .env          # at the repository root: compose reads ../../.env
$EDITOR .env                  # replace every CHANGE_ME
docker compose -f infrastructure/docker/docker-compose.yml up -d
```

The example file ships with `CHANGE_ME` placeholders rather than working
secrets, so a copied-and-forgotten file fails the length checks instead of
deploying a known key. See [generating them](#secrets-and-generating-them).

That starts every dependency plus the API, the worker, nginx, Prometheus,
Grafana and Loki. Migrations run in their own container before the API starts.

Without Docker, the dependencies can be run natively and the binaries built
directly:

```sh
cd backend
go build ./cmd/api ./cmd/worker ./cmd/migrate
POSTGRES_DSN=... JWT_SIGNING_KEYS=... ./migrate -dir ../database/migrations up
```

For development, `SMS_ECHO_CODES=true` returns the OTP in the response instead
of sending it, so sign-in works without an SMS provider. The configuration
loader **refuses to start** with that set when `SOBH_ENV=production`.

---

## Configuration

Every setting comes from the environment (§84.11); nothing is read from a file
in the image. `backend/internal/config/config.go` is the authority, and it
validates on startup — a bad configuration fails immediately and loudly rather
than at the first request that needs it.

### Required everywhere

| Variable | Meaning |
|---|---|
| `POSTGRES_DSN` | Connection string. Include `sslmode=require` outside a private network. |
| `JWT_SIGNING_KEYS` | `kid:secret[,kid:secret]`. More than one is how rotation works. |
| `JWT_ACTIVE_KEY_ID` | Which of those keys signs new tokens. |
| `PHONE_HASH_PEPPER` | Peppers the contact-discovery digest. **Changing it makes every existing contact match fail.** |
| `PUBLIC_BASE_URL` | The externally reachable origin. Used to build links; must be `https://` in production. |
| `SOBH_ENV` | `development`, `staging` or `production`. Selects the stricter checks below. |

### What production additionally refuses to start without

The loader rejects all of these, so an unsafe deployment cannot come up quietly:

- `PHONE_HASH_PEPPER` shorter than 32 bytes
- any signing key shorter than 32 bytes
- `SMS_ECHO_CODES=true` — it would print verification codes to callers
- `SMS_PROVIDER=log` — a real provider is required
- missing `MINIO_ACCESS_KEY` / `MINIO_SECRET_KEY`
- a `PUBLIC_BASE_URL` that is not `https://`
- `TURN_SERVERS` set without `TURN_SECRET`

### The rest

Grouped by prefix, all with working defaults: `HTTP_ADDR`, `METRICS_ADDR`,
`SHUTDOWN_TIMEOUT`, `NODE_ID`, `TRUSTED_PROXIES`, `LOG_LEVEL`, `LOG_FORMAT`;
`POSTGRES_*` pool sizing; `REDIS_*`; `NATS_*`; `MINIO_*`; `OPENSEARCH_*`;
`OTP_*`; `ACCESS_TOKEN_TTL` and `REFRESH_TOKEN_TTL`; `MEDIA_*` size and MIME
limits; `RL_*` rate limits; `SMS_*`; `FCM_*` and `APNS_*`; `TURN_*`;
`CLAMAV_ADDR`; `CDN_BASE_URL`.

`TRUSTED_PROXIES` deserves attention: real-IP resolution and therefore per-IP
rate limiting depend on it. Set it to the CIDRs of your load balancers only. Too
wide and a caller can spoof their address to escape rate limits; too narrow and
every request appears to come from the balancer, so one person's abuse rate-limits
everyone.

---

## Secrets, and generating them

No secret is committed (§84.10). Generate real ones before the first start:

```sh
# Signing keys and the contact pepper: 48 random bytes each.
echo "JWT_SIGNING_KEYS=k1:$(head -c 48 /dev/urandom | base64 -w0)"
echo "PHONE_HASH_PEPPER=$(head -c 48 /dev/urandom | base64 -w0)"

# Backup encryption: keep the private half off the machine being backed up.
age-keygen -o backup-identity.txt
```

In Kubernetes these live in the `sobh-secrets` and `sobh-backup-secrets`
Secrets, referenced by `envFrom` in `infrastructure/kubernetes/`. The manifests
deliberately contain no values.

### Rotating a signing key

Rotation is why `JWT_SIGNING_KEYS` takes a list. Tokens carry the `kid` they
were signed with, so both keys stay valid during the overlap:

1. Add the new key alongside the old: `JWT_SIGNING_KEYS=k1:old,k2:new`.
2. Deploy. Existing tokens still verify.
3. Set `JWT_ACTIVE_KEY_ID=k2`. Deploy. New tokens use the new key.
4. Wait longer than `REFRESH_TOKEN_TTL` so no token signed with `k1` remains.
5. Remove `k1`. Deploy.

Skipping the overlap signs everyone out.

`PHONE_HASH_PEPPER` **cannot** be rotated this way. The digests stored for
contact discovery are computed under it, so changing it orphans every one of
them until each user re-syncs their address book.

---

## Deploying to Kubernetes

```sh
kubectl apply -f infrastructure/kubernetes/00-namespace.yaml
kubectl create secret generic sobh-secrets --from-env-file=production.env -n sobh
kubectl apply -f infrastructure/kubernetes/
```

The manifests provide: a `sobh-api` Deployment with a migration init container,
Service, PodDisruptionBudget and HorizontalPodAutoscaler (CPU 70%, memory 80%);
a `sobh-worker` Deployment; an Ingress; a NetworkPolicy; and backup CronJobs.

Migrations run as an init container, so **a rolling deploy applies them before
any new pod serves traffic**. This is what makes the ordering rule below matter:
a migration must be safe against the previous version of the code, because for
the length of the rollout both are running.

The stateful dependencies are not in these manifests. Use an operator or a
managed service; running PostgreSQL as a bare Deployment is how data gets lost.

---

## Migrations

```sh
./migrate -dir ../database/migrations up       # apply everything pending
./migrate -dir ../database/migrations status   # what is applied, what is not
./migrate -dir ../database/migrations down 1   # revert the most recent
```

Every migration has a working `down` and CI checks that it reverses (§84.12).
The runner records a checksum per migration and takes an advisory lock, so
concurrent deploys cannot apply the same migration twice and an edited migration
that has already run is refused rather than silently skipped.

**Both halves of a rolling deploy run at once.** A migration that drops a column
the currently-deployed code still selects takes the site down for the length of
the rollout. Split it: add the new column and deploy the code that writes both,
then backfill, then remove the old column in a later release.

---

## Backup

`scripts/backup.sh` dumps PostgreSQL, encrypts with `age` before anything
leaves the host, verifies the archive is readable, and prunes by tier.

```sh
export POSTGRES_DSN=... BACKUP_AGE_RECIPIENT=age1... BACKUP_DIR=/var/backups/sobh
./scripts/backup.sh daily
./scripts/backup.sh weekly --with-media    # also mirrors object storage
```

Retention is 14 days for daily, 90 for weekly, 400 for monthly. The CronJobs in
`infrastructure/kubernetes/05-cronjobs.yaml` run daily at 03:00 and weekly on
Sunday at 03:30 UTC.

Set `BACKUP_REMOTE` to an rclone remote for offsite copies. A backup that only
exists on the machine it was taken from is not a backup.

**The private key must not live on the machine being backed up.** `age` encrypts
to a public key, so the backup host needs only the recipient. Store the identity
file somewhere the backup host cannot read, or a compromise of that host is a
compromise of every backup it ever wrote.

---

## Disaster recovery

The target is one hour to restore (§38).

```sh
export POSTGRES_DSN=<target> BACKUP_AGE_IDENTITY_FILE=/secure/backup-identity.txt
./scripts/restore.sh /var/backups/sobh/daily/sobh-daily-20260816T030000Z.dump.age
```

It restores in parallel and refuses a non-empty database unless
`ALLOW_NON_EMPTY=true`, so a restore cannot half-overwrite a live one by
accident.

### Order of recovery

1. **PostgreSQL first.** Nothing else is meaningful without it, and it is the
   only component whose loss is not recoverable from another source.
2. **Redis: start empty.** It holds only rate-limit counters, presence and
   caches. Do not restore it.
3. **NATS: start empty.** Queued jobs are lost; the durable consequences of
   those jobs are not, because the facts they act on are in PostgreSQL. Media
   awaiting processing is found again by the maintenance job.
4. **Object storage** from its own mirror. Media is referenced by id, so
   messages referring to missing objects render as unavailable attachments
   rather than breaking the conversation.
5. **OpenSearch: rebuild, do not restore.** It is entirely derived:
   ```sh
   curl -XPOST "$API/api/v1/admin/search/reindex/news"
   curl -XPOST "$API/api/v1/admin/search/reindex/users"
   curl -XPOST "$API/api/v1/admin/search/reindex/chats"
   curl -XPOST "$API/api/v1/admin/search/reindex/messages"
   ```
   The message rebuild is bounded to recent messages, which is what makes
   search useful again fastest.

### Test the restore

An untested backup is a guess. Restore the latest archive into a scratch
database and check the migration ledger is complete and the table count matches:

```sh
POSTGRES_DSN=postgres://.../sobh_restore_test ./scripts/restore.sh <archive>
psql "$DSN" -c "SELECT max(version) FROM schema_migrations;"
psql "$DSN" -c "SELECT count(*) FROM information_schema.tables WHERE table_schema='public';"
```

---

## Monitoring, and what the alerts mean

Prometheus scrapes `METRICS_ADDR` (`:9090` by default). Rules are in
`infrastructure/monitoring/`, and the thresholds come from the §79 targets:
API p95 under 200 ms, WebSocket delivery under 500 ms, send acknowledgement
under 300 ms.

`/health` is liveness — the process is up. `/ready` is readiness — dependencies
answered. Kubernetes must use `/ready` for the readiness probe and `/health` for
liveness; using `/ready` for liveness means a brief database blip restarts every
pod at once and turns a recoverable incident into an outage.

What to look at when an alert fires:

| Symptom | Look at first |
|---|---|
| API p95 over budget | PostgreSQL connection saturation (`POSTGRES_MAX_CONNS` against the server's own limit), then slow queries. |
| Sign-in and sending both failing | Redis. Rate limits fail closed, so an unreachable Redis blocks both. |
| Realtime silent, REST fine | NATS, then the WebSocket eviction counter — a slow consumer being dropped is intended behaviour. |
| Media stuck "processing" | The worker, then ClamAV. Nothing is served before it is scanned. |
| Push not arriving | `FCM_*` / `APNS_*` credentials, then the token retirement counter. |
| Search empty, everything else fine | OpenSearch reachable and `OPENSEARCH_ENABLED`. A disabled search returns empty results by design rather than erroring. |

---

## Routine operations

**Scaling.** `sobh-api` is stateless — scale on CPU, which the HPA already does.
Realtime fan-out crosses nodes over NATS, so more replicas need no coordination.
Watch PostgreSQL connections: every replica opens up to `POSTGRES_MAX_CONNS`, so
the pool size times the replica count must stay under the server's limit.

**Deploying.** Rolling, with the migration init container ahead of it. Watch
readiness rather than liveness during a rollout.

**Log level.** `LOG_LEVEL=debug` for an incident; it is noisy and logs are
redacted for secrets, not for volume. Return it afterwards.

**Feature flags.** Toggled from the admin panel, stored in the database, so no
deploy is needed to turn a feature off during an incident.

**What is not covered here.** Provisioning the stateful dependencies, DNS and
TLS certificates, obtaining SMS and push credentials, and the load test at 500k
concurrent from §78 — that last one needs a real cluster and load generators,
which is a capacity exercise rather than a deployment step.

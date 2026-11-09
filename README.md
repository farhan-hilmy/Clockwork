# Clockwork

Transcend Beyond the Clock.

Clockwork is a lightweight, self-hosted automation and task scheduling platform for developers. Define an automation once, schedule it, and let Clockwork run it reliably, whether the task belongs to Clockwork itself or to an external application.

> **Status: Milestone 4 (Reliability).** Create HTTP and shell tasks with cron schedules through the UI and API. Clockwork runs them on schedule or on demand (**Run now**), applies each task's timeout, retries retryable failures with backoff, and records every attempt with its status, duration and an output excerpt. Runs held by a crashed worker are recovered and retried, overlapping runs follow the task's concurrency policy, and several instances can safely share one database. Per-application API keys arrive in M5. See [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) for the design and the full roadmap.

## Quick start (Docker Compose)

Requires Docker with Compose v2.

```sh
cp .env.example .env
# Set CLOCKWORK_ADMIN_TOKEN, for example:  openssl rand -hex 32
docker compose up -d --build
```

Open <http://localhost:8080> and sign in with your admin token.

Task definitions live in the `pgdata` volume, so they survive restarts and `docker compose down`. Only `docker compose down -v` deletes them.

## Configuration

All configuration comes from environment variables. The server refuses to start if a value is missing or invalid.

| Variable | Default | Description |
|---|---|---|
| `CLOCKWORK_DATABASE_URL` | required | PostgreSQL connection URL. Compose sets this for you. |
| `CLOCKWORK_ADMIN_TOKEN` | required | Bearer token for the UI and API, at least 16 characters. |
| `CLOCKWORK_ENABLE_SHELL` | `false` | Allow shell tasks. See [Security](#security). |
| `CLOCKWORK_HTTP_ADDR` | `:8080` | Listen address. |
| `CLOCKWORK_WEB_DIR` | empty | Directory with the built UI. Set in the Docker image. |
| `CLOCKWORK_LOG_FORMAT` | `json` | `json` or `text`. |
| `CLOCKWORK_LOG_LEVEL` | `info` | `debug`, `info`, `warn` or `error`. |
| `CLOCKWORK_SHUTDOWN_TIMEOUT` | `30s` | On SIGTERM, how long in-flight HTTP requests and running executions may continue. Executions still running after it are recorded as `interrupted` and retried if attempts remain. |
| `CLOCKWORK_WORKERS` | `4` | How many executions may run at once (1 to 256). |
| `CLOCKWORK_HTTP_DENY_PRIVATE` | `false` | Refuse HTTP tasks whose target resolves to a loopback, link-local or private address. See [Security](#security). |
| `CLOCKWORK_SCHEDULER_INTERVAL` | `1s` | How often the scheduler looks for due tasks (100ms to 1m). |
| `CLOCKWORK_MISFIRE_GRACE` | `1m` | How late a run can be queued and still count as on time (1s to 24h). Later runs are misfires and follow the task's `misfire_policy`. |

## API

Every `/api/v1` request needs `Authorization: Bearer <CLOCKWORK_ADMIN_TOKEN>`.

| Method and path | Purpose |
|---|---|
| `GET /healthz` | Liveness. No auth. |
| `GET /readyz` | Readiness, including the database. No auth. |
| `GET /api/v1/meta` | Server version and available runners. |
| `GET /api/v1/applications` | Applications and their task counts. |
| `GET /api/v1/tasks?limit=&cursor=&q=&enabled=&application_id=` | List tasks, newest first. |
| `POST /api/v1/tasks` | Create a task. |
| `GET /api/v1/tasks/{id}` | Get one task. |
| `PATCH /api/v1/tasks/{id}` | Partial update. Send `"schedule": null` to remove the schedule. |
| `POST /api/v1/tasks/{id}/enable` and `/disable` | Resume or pause a task. |
| `DELETE /api/v1/tasks/{id}` | Delete a task and its history. |
| `POST /api/v1/tasks/{id}/run` | Queue a run now (`202`), even if the task is paused or unscheduled. |
| `GET /api/v1/executions?task_id=&status=&limit=&cursor=` | List executions, newest first. |
| `GET /api/v1/executions/{id}` | Get one execution. |

Example:

```sh
curl -s localhost:8080/api/v1/tasks \
  -H "Authorization: Bearer $CLOCKWORK_ADMIN_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "name": "health-check-api",
    "runner": {"type": "http", "config": {"method": "GET", "url": "https://api.example.com/healthz",
               "headers": {"Authorization": "Bearer abc"}, "expected_status": [200]}},
    "schedule": {"cron": "*/5 * * * *", "timezone": "Asia/Jakarta"},
    "timeout_seconds": 10,
    "retry": {"max_attempts": 3, "backoff_initial_seconds": 5, "backoff_max_seconds": 60}
  }'
```

Errors always use one shape. A `422` lists every invalid field at once:

```json
{"error": {"code": "validation_failed", "message": "one or more fields are invalid",
           "details": {"schedule.cron": "invalid cron expression: ...", "runner.config.url": "must be an absolute http or https URL"}}}
```

Secret-looking headers, query parameters and environment variables (`Authorization`, `Cookie`, `*token*`, `*secret*`, `*password*`, `*key*` and similar) are returned as `********`. To keep a stored secret when you update a task, send `********` back unchanged.

### Schedules

- Standard 5-field cron (`minute hour day-of-month month day-of-week`), plus `@hourly`, `@daily`, `@weekly`, `@monthly` and `@yearly`. Seconds and `@every` are not supported.
- `timezone` is an IANA name such as `Asia/Jakarta` and defaults to `UTC`. Cron fields are evaluated in that timezone.
- Daylight saving time:
  - A local time that doesn't exist on the day clocks spring forward is skipped.
  - A job pinned to a specific hour runs once on the day clocks fall back.
  - Interval jobs such as `*/5` keep running through the repeated hour.
- `misfire_policy` decides what happens to runs missed while Clockwork was down (more than `CLOCKWORK_MISFIRE_GRACE` late): `skip` (default) drops them, `run_once` queues a single catch-up run. Either way, the schedule then continues from now.
- `concurrency_policy` decides what happens when a run is about to start while another run of the same task is still going: `forbid` (default) records it as `skipped`, `allow` runs both. This covers scheduled runs, **Run now** and retries alike. Tasks without a schedule always use `forbid`.
- Editing a task's name, runner or retry settings keeps its pending run. Changing the cron or timezone, or resuming a paused task, recomputes the next run from now. Runs missed while a task was paused are not owed.
- Semantics are described in [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md#3-scheduling-and-execution-semantics).

### Runs and retries

Each attempt is one execution. Its `status` is one of:

- `queued`, `running`, `succeeded` or `failed`.
- `retrying`: failed, and the next attempt is queued.
- `interrupted`: Clockwork shut down mid-run, or the worker running it crashed or stalled. It is retried like a retryable failure.
- `skipped`: not run, because another run of the task was active and its `concurrency_policy` is `forbid`.

Delivery is at-least-once. A run that was interrupted after its HTTP request or command already took effect will run again on retry.

| Runner | Succeeds when | Retried (up to `retry.max_attempts`) | Not retried | `output` |
|---|---|---|---|---|
| `http` | status is in `expected_status`, or any 2xx when it is empty | timeout, connection error, status 5xx, 429, 408 | any other unexpected status, blocked target | `status_code`, `content_type`, first 8 KiB of `body`, `body_bytes` |
| `shell` | exit code 0 | timeout, non-zero exit | command could not start | `exit_code`, first 16 KiB of `stdout` and `stderr` |

- The wait before attempt *n*+1 is `backoff_initial_seconds × 2^(n-1)`, capped at `backoff_max_seconds`. A failed attempt's `error.retry_at` says when the next one may start.
- HTTP requests carry `X-Clockwork-Execution-Id` and `X-Clockwork-Attempt`, so receivers can recognise a re-delivery. Delivery is at-least-once: make endpoints idempotent.
- Shell commands run with `sh -c` and see only the task's `env`, plus `PATH` and `CLOCKWORK_EXECUTION_ID`, `CLOCKWORK_TASK_ID` and `CLOCKWORK_ATTEMPT`. On timeout the whole process group is killed.

## Development

Requires Go 1.26+, Node 24+ and Docker (for Postgres).

```sh
# Postgres for development
docker run -d --name clockwork-pg -e POSTGRES_PASSWORD=dev -e POSTGRES_DB=clockwork -p 5432:5432 postgres:17-alpine

# API server on :8080
export CLOCKWORK_DATABASE_URL="postgres://postgres:dev@localhost:5432/clockwork?sslmode=disable"
export CLOCKWORK_ADMIN_TOKEN="dev-token-0123456789"
export CLOCKWORK_LOG_FORMAT=text
go run ./cmd/clockwork

# UI on :5173, proxying /api to :8080
cd web && npm install && npm run dev
```

Migrations run automatically at startup. They live in `internal/store/migrations` and are embedded in the binary.

### Tests

```sh
go test ./...                       # unit + HTTP tests (no database needed)

# Postgres integration tests (uses and truncates the given database).
# -p 1: several packages share the database, so they must not run in parallel.
CLOCKWORK_TEST_DATABASE_URL="postgres://postgres:dev@localhost:5432/clockwork_test?sslmode=disable" go test -p 1 ./...

cd web && npm test && npm run build # UI unit tests, typecheck, production build
```

The race detector needs cgo. On a machine without a C toolchain, run the suite in a Linux container against the same Postgres:

```sh
docker run --rm -v "$PWD:/src:ro" -w /src -e GOFLAGS=-buildvcs=false \
  -e CLOCKWORK_TEST_DATABASE_URL="postgres://postgres:dev@host.docker.internal:5432/clockwork_test?sslmode=disable" \
  golang:1.26 go test -race -p 1 ./...
```

## Security

- **Every API call requires the admin token.** Treat it like a root password. Compose binds the port to `127.0.0.1`. Put a TLS-terminating reverse proxy in front before exposing Clockwork to a network.
- **Shell tasks run arbitrary commands** as the Clockwork process user, with access to anything that user can reach. They are disabled unless `CLOCKWORK_ENABLE_SHELL=true`. The Docker image runs as an unprivileged user (uid 10001). External applications (M5) will never be allowed to create shell tasks.
- **HTTP tasks can reach anything the server can reach**, including internal services and cloud metadata endpoints. Set `CLOCKWORK_HTTP_DENY_PRIVATE=true` to refuse loopback, link-local and private targets. The check runs on the resolved address at connect time, so DNS names pointing inward are refused too. Outbound proxies are not used.
- Secrets in task configuration are stored in plain text in Postgres, and masked in API responses and logs. Response bodies and command output are stored as excerpts: don't have tasks print secrets.
- Clockwork has not had a security review. Don't run it on an untrusted network or claim it is production-secure without one.

## Troubleshooting

| Symptom | Fix |
|---|---|
| `required variable CLOCKWORK_ADMIN_TOKEN is missing` | Set it in `.env` (see `.env.example`). |
| `configuration: CLOCKWORK_ADMIN_TOKEN must be at least 16 characters` | Use a longer token, such as `openssl rand -hex 32`. |
| `/readyz` returns 503 | The database is unreachable. Check `docker compose logs db` and `CLOCKWORK_DATABASE_URL`. |
| UI keeps asking for the token | The token changed on the server. Sign in again with the new value. |
| "shell tasks are disabled on this server" | Expected by default. Set `CLOCKWORK_ENABLE_SHELL=true` only if you accept the risk. |
| Port 8080 already in use | Set `CLOCKWORK_PORT` in `.env`. |
| Execution failed with `config: the shell runner is not enabled on this server` | The task was created while shell was enabled. Re-enable it, or change the task to an HTTP runner. |
| Execution failed with `blocked: destination … is not a public address` | `CLOCKWORK_HTTP_DENY_PRIVATE` is on and the URL resolves to an internal address. |
| Runs stay `queued` | Check the logs for `executor started` and for claim errors, and that the task's `run_after` has passed. |
| Many runs are `skipped` | Runs of that task last longer than its schedule interval, and its `concurrency_policy` is `forbid`. Lengthen the interval, shorten the run, or set `allow`. |
| A run shows `interrupted` with "lease expired" | The instance running it crashed, was killed, or stalled for over 30 seconds. It is retried if attempts remain. |

## License

Not yet chosen.

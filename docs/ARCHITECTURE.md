# Clockwork architecture

Clockwork is a self-hosted automation engine. It stores task definitions, decides when they are due, runs them through pluggable runners, and records every attempt. Other applications use it through a versioned HTTP API.

**Clockwork owns scheduling and execution coordination. Applications that integrate with it own their business logic.**

## 1. Shape of the system

One Go binary and one PostgreSQL database. There is no Redis, message broker or Kubernetes.

```
             ┌──────────────────────── clockwork (single Go process) ─────────────────────────┐
 Web UI ───▶ │  httpapi  ──▶  tasks (registry, validation)                                    │
 Apps   ───▶ │     │                                                                          │
 curl   ───▶ │     └──────▶  executions (history)                                             │
             │                                                                                │
             │  scheduler ──writes due occurrences──▶  claimer ──▶ executor pool ──▶ runners │ ──▶ HTTP targets / host shell
             │                                                       │                        │
             │                                                       └──▶ webhooks dispatcher │ ──▶ app webhook receivers
             └──────────────────────────────────────┬─────────────────────────────────────────┘
                                                    ▼
                                               PostgreSQL
```

### Packages (`internal/`)

| Package | Responsibility | Milestone |
|---|---|---|
| `config` | Reads and validates environment configuration. | M1 |
| `store` | Postgres connection pool and embedded migrations. | M1 |
| `tasks` | Task model, validation, repository and service. Knows nothing about HTTP or execution. | M1 |
| `schedule` | Cron parsing and next-run calculation (timezone/DST aware), with an injectable clock. | M1 (calc) / M2 (loop) |
| `httpapi` | Routing, JSON encoding, error envelope, auth middleware. | M1 |
| `scheduler` | Loop that turns due schedules into `queued` executions. Never runs tasks itself. | M2 |
| `executions` | Execution model, history queries, manual "run now". | M2 |
| `executor` | Worker pool: claims queued executions, applies timeout and retry, records results. | M3/M4 |
| `runners` | `Runner` interface plus `http` and `shell` implementations. | M3 |
| `apps` | Application identity, API keys, ownership checks. | M5 |
| `webhooks` | Endpoint registration, signed delivery, retries, delivery log. | M6 |

These boundaries are enforced by direction of imports: `httpapi` → `tasks`/`apps`/… → `store`. Domain packages never import `httpapi`, and runners never import the scheduler.

## 2. Data model

| Table | Purpose |
|---|---|
| `applications` | Every task belongs to one. `app_clockwork` is the default context for tasks created in the UI. |
| `tasks` | Definition: runner type + JSON config, optional schedule (cron, timezone, misfire and concurrency policy), timeout, retry policy, enabled flag, `next_run_at`. |
| `executions` | One row per attempt: trigger, status, attempt number, input, output or error, timestamps, duration, optional idempotency key. |
| `app_credentials` (M5) | Hashed API keys per application, supporting rotation and revocation. |
| `webhook_endpoints`, `webhook_deliveries` (M6) | Registered endpoints and per-event delivery attempts. |

IDs are prefixed ULIDs (`tsk_…`, `exe_…`, `app_…`). They sort by creation time, which makes cursor pagination simple.

## 3. Scheduling and execution semantics

**Separation.** The scheduler only inserts `queued` executions for due occurrences and advances `next_run_at`, in one transaction. Workers pick up queued rows independently, so a slow task cannot delay scheduling.

**One claim per occurrence.** `executions` has a unique key on `(task_id, scheduled_for, attempt)`. A second scheduler instance inserting the same occurrence hits the constraint and skips it. Workers claim rows with `SELECT … FOR UPDATE SKIP LOCKED`. This is the "claimer" interface; a distributed implementation can replace it later without touching runners.

**Delivery is at-least-once, not exactly-once.** If the process dies after a runner has caused a side effect but before the result is saved, the recovered execution may run again. Clockwork does not claim exactly-once execution of external side effects. Integrations should send an `Idempotency-Key` and make their own endpoints idempotent.

**On time vs. missed.** Each pass (every `CLOCKWORK_SCHEDULER_INTERVAL`, default 1s) locks due tasks with `FOR UPDATE SKIP LOCKED`. An occurrence queued no more than `CLOCKWORK_MISFIRE_GRACE` (default 1m) after its time is on time and is always queued. Older occurrences are missed.

**Missed runs** (process was down when a run was due), per task:
- `skip` (default): record nothing and move `next_run_at` to the next future time.
- `run_once`: enqueue a single catch-up run, then continue from now. The catch-up uses the earliest missed occurrence as `scheduled_for`. If an on-time occurrence is queued in the same pass, that run serves as the catch-up.

**Edits.** Changing a task's cron or timezone, or resuming it, recomputes `next_run_at` from now. Other edits keep the pending occurrence, so editing a task as it falls due does not drop the run. If such an edit races with the scheduler and writes back an occurrence that was already queued, the unique key rejects the duplicate.

**Manual runs.** `POST /tasks/{id}/run` inserts a `queued` execution with trigger `manual` and no `scheduled_for`. It works for paused and unscheduled tasks.

`run_all` replay is deliberately left out of the MVP.

**Overlapping runs**, per task, enforced when a worker claims an execution (so it covers scheduled runs, manual runs and retries):
- `forbid` (default): if a run of the task is `running`, the claimed execution is recorded as `skipped`. Claims for a `forbid` task are serialised with a transaction-scoped advisory lock on the task ID, so two workers or instances cannot both start overlapping runs. An advisory lock rather than a row lock: a task delete cascades into execution rows the claimer already holds, and a task row lock would deadlock with it.
- `allow`: runs overlap, up to the global worker limit.

Tasks without a schedule have no place to set the policy and use `forbid`.

**Execution.** A single dispatcher claims the oldest `queued` execution whose `run_after` has passed, using `FOR UPDATE SKIP LOCKED`, and hands it to a worker. At most `CLOCKWORK_WORKERS` executions run at once. The worker runs the task's runner under `timeout_seconds` and records `status`, `output`, `error`, `started_at`, `finished_at` and `duration_ms`. The update only applies while the row is still `running`, so a row that was reassigned in the meantime is left to its new owner. A panicking runner fails its execution rather than the pool.

**Retries.** Runners return errors classified as retryable (timeouts, connection errors, HTTP 5xx/429/408, non-zero shell exits) or not (invalid config, unexpected other status, blocked target). After a retryable failure, if attempts remain, the attempt is marked `retrying` and the next attempt is inserted in the same transaction as a new `queued` execution with `attempt + 1`, the same trigger and `scheduled_for`, and `run_after = now + backoff`. The backoff is `backoff_initial_seconds × 2^(attempt-1)`, capped at `backoff_max_seconds`.

**Leases and crash recovery.**
- A claimed execution holds a 30-second lease (`lease_expires_at`). Its worker renews it every 10 seconds while the runner works, however long the task's timeout.
- At startup and every 15 seconds, each instance sweeps `running` executions whose lease has expired (`FOR UPDATE SKIP LOCKED`, so instances do not collide). It marks them `interrupted` with `error.kind = "interrupted"` and applies the retry policy, as for a retryable failure.
- A worker that stalls past its lease and then wakes up finds, at its next renewal, that the row is no longer `running`. It cancels its run and discards the result, so the work is not finished twice by two owners.
- Recovery cannot undo side effects the interrupted attempt already caused. This is the at-least-once case described above.

**Graceful shutdown.** On SIGTERM: the scheduler stops, the executor stops claiming, and in-flight runs may continue for `CLOCKWORK_SHUTDOWN_TIMEOUT`. Runs still going are then canceled (killing shell process groups), recorded as `interrupted`, and retried if attempts remain. The database pool closes only after every run is recorded. If the process is killed before that, the leases expire and another instance, or the next start, recovers the runs.

## 4. APIs

- `/healthz` (liveness) and `/readyz` (database reachable).
- `/api/v1/...`: one versioned JSON API used both by the web UI (admin token) and, from M5, by external applications (per-app API keys, scoped to their own tasks and executions).
- Errors always use this envelope: `{"error": {"code": "validation_failed", "message": "...", "details": {...}}}`.
- Lists use cursor pagination: `?limit=50&cursor=<last id>`, and responses include `next_cursor`.
- OpenAPI document served at `/api/v1/openapi.json` (M5).

## 5. Security

| Risk | Mitigation |
|---|---|
| Unauthenticated access | Every `/api/v1` route requires a bearer token. In M1 that is the admin token (`CLOCKWORK_ADMIN_TOKEN`), compared in constant time; the server refuses to start without one. |
| Arbitrary command execution | Shell tasks are disabled unless `CLOCKWORK_ENABLE_SHELL=true`. External applications will never be allowed to create shell tasks. Runs get a timeout, a process-group kill, an output size cap, and only the environment variables set on the task. Running Clockwork as a non-root user in a container is strongly recommended. |
| Credential leakage | Sensitive HTTP headers and shell environment variables are masked as `********` in every API response and log. Sending the mask back on update keeps the stored value. API keys are stored hashed (M5). |
| SSRF via HTTP tasks | `CLOCKWORK_HTTP_DENY_PRIVATE=true` rejects loopback, link-local, private and other non-public targets, checked on the resolved address at connect time (so DNS rebinding is covered) with proxies disabled. In M3 it applies to every task; M5 may enforce it for app-owned tasks regardless of the flag. |
| Oversized input | Request bodies are capped at 1 MiB, and runner configs and stored output excerpts are bounded. |
| Webhook spoofing | Deliveries are HMAC-SHA256 signed with a per-endpoint secret and timestamp (M6). |

Clockwork has not had a security review. Do not expose it to the internet without one.

## 6. Built now vs. extension points

**Built in the MVP:** HTTP and shell runners; a single-instance scheduler on a claim-safe schema; per-app API keys; webhooks.

**Extension points only:**
- the `Runner` interface (custom runners)
- the claimer interface (distributed scheduling)
- `run_all` misfire replay
- workflow chaining
- a plugin marketplace

Integrations for repository sync, social scheduling and Entropy are separate applications built on `/api/v1`. None of them live in this repository.

## 7. Milestones

| # | Scope | Exit criteria |
|---|---|---|
| M1 Foundation | config, schema and migrations, task CRUD API, basic UI, Docker Compose | `go test ./...` green; create, list, edit and delete a task from the UI |
| M2 Scheduling | scheduler loop, due-task discovery, manual trigger | an enabled cron task produces queued executions that survive a restart |
| M3 Execution | HTTP and shell runners, timeouts, retries, history and logs | a scheduled HTTP task records status code, duration and an excerpt |
| M4 Reliability | `SKIP LOCKED` claiming, concurrency policy, lease recovery, graceful shutdown | concurrent-worker and crash-recovery tests pass |
| M5 Integration | apps and API keys, ownership, idempotency keys, OpenAPI, example integration | app A cannot see app B's tasks or executions |
| M6 Webhooks | lifecycle events, signed delivery, retries, delivery log | events delivered with signature; failures retried and tracked |
| M7 Polish | dashboard, integrations view, filters, docs, test coverage | the 16-point definition of done passes |

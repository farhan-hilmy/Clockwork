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
| `webhooks` | Endpoint registration, event outbox, signed delivery (dispatcher), retries, delivery log. | M6 |

These boundaries are enforced by direction of imports: `httpapi` → `tasks`/`apps`/… → `store`. Domain packages never import `httpapi`, and runners never import the scheduler.

## 2. Data model

| Table | Purpose |
|---|---|
| `applications` | Every task belongs to one. `app_clockwork` is the default context for tasks created in the UI. |
| `tasks` | Definition: runner type + JSON config, optional schedule (cron, timezone, misfire and concurrency policy), timeout, retry policy, enabled flag, `next_run_at`. |
| `executions` | One row per attempt: trigger, status, attempt number, input, output or error, timestamps, duration, optional idempotency key. |
| `app_credentials` | API keys per application: name, display prefix, SHA-256 hash of the key, `last_used_at` (written at most once a minute), `revoked_at`. Deleted with their application. |
| `webhook_endpoints` | URLs owned by an application, with an event filter, a signing secret and an enabled flag. |
| `webhook_deliveries` | The outbox: one row per (event, endpoint), carrying the payload, status, attempts, next attempt time and the last response. |

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

**Webhooks.**
- **Outbox:** the executor writes events in the same transaction as the state change they describe: claim (started, skipped), finish (succeeded, failed, retrying, interrupted) and lease recovery (interrupted). A crash can neither lose an event nor emit one for a change that rolled back. The payload is a snapshot of the execution read inside that transaction. When an application has no endpoints, the cost is one indexed lookup.
- **Dispatcher:** it claims due deliveries with `FOR UPDATE SKIP LOCKED`. In the same statement it increments `attempts` and moves `next_attempt_at` one minute ahead as a lease. If the dispatcher dies mid-send, the delivery simply becomes due again; no recovery sweep is needed.
- **Recording:** the outcome is written only if `attempts` still matches, so a concurrent redeliver is not overwritten.
- **Retries:** after 10s, 30s, 2m, 10m, 30m, 1h, 3h, 6h and 15h; after 10 attempts the delivery is `failed`. Redirects are not followed, and the same private-address rule as HTTP tasks applies.
- **Signing:** `Clockwork-Signature: t=<unix>,v1=<hex HMAC-SHA256(secret, "<t>.<body>")>`, computed at send time, so retries carry a fresh timestamp. `Clockwork-Delivery` stays the same across retries.
- **Secrets:** stored as is, because HMAC needs them. They are returned only on create and rotate, and rotation takes effect immediately.

## 4. APIs

- `/healthz` (liveness) and `/readyz` (database reachable).
- `/api/v1/...`: one versioned JSON API, used both by the web UI (admin token) and by external applications (per-application API keys).
- **Authentication** resolves every request to a principal: the admin, or one application. API keys are `cwk_` plus 256 random bits. They are looked up by SHA-256 hash, which is enough for keys of that entropy. A revoked key, or a key of a deleted application, answers 401. A key of a disabled application answers 403.
- **Ownership** is enforced in the HTTP layer, the only place that knows the principal. An application's lists are filtered to its own `application_id`. Single resources owned by another application answer 404 rather than 403, so their existence does not leak. Creates are forced into the caller's application. The `/applications` endpoints are admin-only. The domain packages stay principal-agnostic.
- **Restrictions for applications:** no shell runner, whatever `CLOCKWORK_ENABLE_SHELL` says. Their HTTP tasks may only reach public addresses unless `CLOCKWORK_APP_HTTP_ALLOW_PRIVATE=true`. The executor decides this per execution, from the task's `application_id`.
- **Idempotency:** `POST /tasks/{id}/run` accepts `Idempotency-Key`. The key is unique per application through `executions (application_id, idempotency_key)`. Concurrent requests with one key insert once; the others read that row and get `200 Idempotent-Replayed: true`. A key reused on another task answers 409. Retries the executor creates carry no key.
- **Disabled applications:** their keys are refused and the scheduler skips their tasks. Due runs then wait, and when the application is re-enabled, each task's misfire policy applies.
- Errors always use this envelope: `{"error": {"code": "validation_failed", "message": "...", "details": {...}}}`.
- Lists use cursor pagination: `?limit=50&cursor=<last id>`, and responses include `next_cursor`.
- An OpenAPI 3.1 document is served without authentication at `/api/v1/openapi.json`. It is embedded in the binary, and a test checks it lists exactly the routed endpoints.

## 5. Security

| Risk | Mitigation |
|---|---|
| Unauthenticated access | Every `/api/v1` route except `openapi.json` requires a bearer token: the admin token (`CLOCKWORK_ADMIN_TOKEN`, compared in constant time; the server refuses to start without one) or an application API key. |
| One application reading or changing another's data | Ownership checks on every task and execution route; other applications' resources answer 404. Covered by `TestApplicationsCannotSeeEachOther`. |
| Arbitrary command execution | Shell tasks are disabled unless `CLOCKWORK_ENABLE_SHELL=true`. External applications will never be allowed to create shell tasks. Runs get a timeout, a process-group kill, an output size cap, and only the environment variables set on the task. Running Clockwork as a non-root user in a container is strongly recommended. |
| Credential leakage | Sensitive HTTP headers and shell environment variables are masked as `********` in every API response and log. Sending the mask back on update keeps the stored value. API keys are stored as SHA-256 hashes and shown only once, at creation. |
| SSRF via HTTP tasks | `CLOCKWORK_HTTP_DENY_PRIVATE=true` rejects loopback, link-local, private and other non-public targets, checked on the resolved address at connect time (so DNS rebinding is covered) with proxies disabled. Tasks owned by applications get the same check by default, since an application can read its executions' output; `CLOCKWORK_APP_HTTP_ALLOW_PRIVATE=true` lifts it. |
| Oversized input | Request bodies are capped at 1 MiB, and runner configs and stored output excerpts are bounded. |
| Webhook spoofing and replay | Deliveries are HMAC-SHA256 signed over a timestamp and the body with a per-endpoint secret. Receivers verify in constant time and reject old timestamps. |
| SSRF via webhook endpoints | Same address rule as HTTP tasks, checked at connect time; redirects are not followed. |

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

# Definition of done

The MVP is done when all 16 points below pass. Agreed on 2026-11-12, as the exit criterion of milestone M7 in [ARCHITECTURE.md](ARCHITECTURE.md#7-milestones).

Each point is checked in one or more of three ways:

- **Acceptance:** `go run ./scripts/acceptance` against a running Clockwork, usually a fresh Docker Compose install. See [Running the acceptance check](#running-the-acceptance-check).
- **Tests:** the named Go tests. Each runs in CI against Postgres with the race detector.
- **CI:** the GitHub Actions workflow in `.github/workflows/ci.yml`.

| # | Point | Passes when | Checked by |
|---|---|---|---|
| 1 | **Clean install** | From a fresh clone, copy `.env.example` to `.env`, set the token, and run `docker compose up -d --build`. The UI answers on port 8080 and `/readyz` is ok within 2 minutes. The server runs as a non-root user. | Acceptance (readiness, UI, user), CI (image builds) |
| 2 | **Fails fast on bad config** | A missing or invalid setting stops startup with a message naming it. | Tests: `TestLoadRequiresDatabaseAndStrongToken`, `TestLoadRejectsBadValues`, `TestAppPrivateAccessCannotContradictGlobalDeny` |
| 3 | **Tasks in UI and API** | An HTTP task can be created, edited, paused, resumed and deleted. Validation errors are reported per field. Secrets are masked in responses. | Acceptance, tests: `TestTaskLifecycleOverHTTP`, `TestValidationErrorsAre422WithFieldDetails`, `TestCreateRedactsSecretsButStoresThem` |
| 4 | **Schedules** | Cron with timezone and daylight saving works as documented. The next run time in the API matches the cron. | Acceptance, tests: `internal/schedule`, `TestPlanDue` |
| 5 | **On-time runs survive restarts** | An every-minute task runs within 5 seconds of the minute, and keeps being scheduled after a restart. | Acceptance (timing), tests: `TestScheduledRunsAreQueuedAndSurviveRestart` |
| 6 | **Missed runs** | After downtime, `skip` drops missed runs and `run_once` queues one catch-up run. | Tests: `TestMisfirePolicies`, `TestPlanDue` |
| 7 | **Run history** | Each attempt shows status, duration, an output excerpt and its error. History can be filtered by task, status, application and time range. | Acceptance, tests: `TestScheduledHTTPTaskRecordsResult`, `TestExecutionFilters` |
| 8 | **Timeouts and retries** | A timeout ends the run. Retryable failures are retried with backoff up to `max_attempts`; others are not retried. | Tests: `TestHTTPTimeoutIsRetryable`, `TestShellTimeoutKillsCommand`, `TestRetryableFailureIsRetriedWithBackoff`, `TestLastAttemptFailsWithoutRetry`, `TestNonRetryableFailureAndDisabledRunner` |
| 9 | **Concurrency policy** | `forbid` records an overlapping run as skipped. `allow` runs overlapping runs together. | Tests: `TestForbidSkipsOverlappingRunsAcrossInstances`, `TestAllowRunsOverlap` |
| 10 | **Crash safety and several instances** | A run whose worker is killed is marked interrupted, then retried. Several instances sharing one database never run a scheduled occurrence or a queued execution twice. | Tests: `TestCrashedWorkerIsRecoveredAndRetried`, `TestStalledWorkerLosesLeaseAndStops`, `TestConcurrentWorkersRunEachExecutionOnce`, `TestConcurrentSchedulersDoNotDuplicate` |
| 11 | **Graceful shutdown** | On SIGTERM, runs that finish within the grace period succeed. The rest are interrupted and retried. | Tests: `TestShutdownWaitsForRunsWithinGrace`, `TestShutdownInterruptsAndRetries` |
| 12 | **Application isolation** | Application A cannot see or change application B's tasks, executions or webhooks. API keys are stored hashed and can be revoked. A disabled application is refused. | Acceptance, tests: `TestApplicationsCannotSeeEachOther`, `TestWebhookEndpointsOverHTTP`, `TestAPIKeyLifecycle`, `TestPostgresApplicationsAndKeys` |
| 13 | **Integration contract** | `Idempotency-Key` prevents duplicate runs. `/api/v1/openapi.json` lists exactly the routed endpoints. The example integration runs against a fresh install. | Acceptance, tests: `TestIdempotentRunNow`, `TestPostgresIdempotencyAndApplicationFilter`, `TestOpenAPIMatchesRouter` |
| 14 | **Webhooks** | Deliveries are signed and verifiable, failed ones are retried on schedule, and every attempt is logged. | Acceptance, tests: `TestEventsAreDeliveredSigned`, `TestFailedDeliveriesAreRetriedAndTracked`, `TestDeliveryGivesUpThenCanBeRedelivered`, `TestExecutionsEmitWebhookEvents` |
| 15 | **Security baseline** | Every API route except `openapi.json` requires authentication. Shell is off by default and never available to applications. The private-address rules hold. Request bodies are size-capped. Secrets appear in neither responses nor logs. Security headers are set. | Acceptance, tests: `TestAPIRequiresBearerToken`, `TestApplicationRestrictions`, `TestApplicationTasksCannotReachPrivateAddresses`, `TestMalformedRequests`, `TestSecretsStayOutOfLogs` |
| 16 | **Quality gates in CI** | CI runs on every push: formatting, vet, Go tests with the race detector against Postgres, UI tests, typecheck and build, and the Docker image. Statement coverage of `internal/` is at least 70%. | CI |

## Running the acceptance check

Against a fresh Docker Compose install:

```sh
cp .env.example .env            # set CLOCKWORK_ADMIN_TOKEN
docker compose up -d --build
CLOCKWORK_URL=http://localhost:8080 CLOCKWORK_ADMIN_TOKEN=... go run ./scripts/acceptance
```

The check takes about 90 seconds, most of it waiting for a scheduled run. It prints one line per point and exits non-zero if any fails. It creates applications and tasks whose names start with `acceptance-`, and removes them at the end.

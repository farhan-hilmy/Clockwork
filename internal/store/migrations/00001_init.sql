-- +goose Up
CREATE TABLE applications (
    id          text PRIMARY KEY,
    name        text NOT NULL UNIQUE CHECK (length(name) BETWEEN 1 AND 100),
    description text NOT NULL DEFAULT '',
    enabled     boolean NOT NULL DEFAULT true,
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now()
);

-- Default context for tasks created directly in Clockwork.
INSERT INTO applications (id, name, description)
VALUES ('app_clockwork', 'clockwork', 'Default Clockwork application context');

CREATE TABLE tasks (
    id                      text PRIMARY KEY,
    application_id          text NOT NULL REFERENCES applications (id) ON DELETE RESTRICT,
    name                    text NOT NULL CHECK (length(name) BETWEEN 1 AND 100),
    description             text NOT NULL DEFAULT '',
    runner_type             text NOT NULL CHECK (runner_type IN ('http', 'shell')),
    runner_config           jsonb NOT NULL,
    schedule_cron           text,
    schedule_timezone       text NOT NULL DEFAULT 'UTC',
    misfire_policy          text NOT NULL DEFAULT 'skip' CHECK (misfire_policy IN ('skip', 'run_once')),
    concurrency_policy      text NOT NULL DEFAULT 'forbid' CHECK (concurrency_policy IN ('forbid', 'allow')),
    timeout_seconds         integer NOT NULL CHECK (timeout_seconds BETWEEN 1 AND 86400),
    max_attempts            integer NOT NULL CHECK (max_attempts BETWEEN 1 AND 20),
    backoff_initial_seconds integer NOT NULL CHECK (backoff_initial_seconds BETWEEN 1 AND 3600),
    backoff_max_seconds     integer NOT NULL CHECK (backoff_max_seconds BETWEEN 1 AND 86400),
    enabled                 boolean NOT NULL DEFAULT true,
    next_run_at             timestamptz,
    created_at              timestamptz NOT NULL DEFAULT now(),
    updated_at              timestamptz NOT NULL DEFAULT now(),
    UNIQUE (application_id, name),
    CHECK (backoff_max_seconds >= backoff_initial_seconds)
);

-- Due-task discovery for the scheduler (M2).
CREATE INDEX tasks_due_idx ON tasks (next_run_at) WHERE enabled AND next_run_at IS NOT NULL;

CREATE TABLE executions (
    id              text PRIMARY KEY,
    task_id         text NOT NULL REFERENCES tasks (id) ON DELETE CASCADE,
    application_id  text NOT NULL REFERENCES applications (id) ON DELETE RESTRICT,
    trigger         text NOT NULL CHECK (trigger IN ('scheduled', 'manual', 'api')),
    status          text NOT NULL CHECK (status IN ('queued', 'running', 'succeeded', 'failed', 'retrying', 'interrupted', 'canceled', 'skipped')),
    attempt         integer NOT NULL DEFAULT 1 CHECK (attempt >= 1),
    scheduled_for   timestamptz,
    input           jsonb,
    output          jsonb,
    error           jsonb,
    idempotency_key text,
    lease_expires_at timestamptz,
    created_at      timestamptz NOT NULL DEFAULT now(),
    started_at      timestamptz,
    finished_at     timestamptz,
    duration_ms     bigint
);

-- A scheduled occurrence can only be materialised once, whatever number of schedulers run.
CREATE UNIQUE INDEX executions_occurrence_uq ON executions (task_id, scheduled_for, attempt) WHERE scheduled_for IS NOT NULL;
CREATE UNIQUE INDEX executions_idempotency_uq ON executions (application_id, idempotency_key) WHERE idempotency_key IS NOT NULL;
CREATE INDEX executions_task_idx ON executions (task_id, id DESC);
CREATE INDEX executions_claim_idx ON executions (created_at) WHERE status = 'queued';

-- +goose Down
DROP TABLE executions;
DROP TABLE tasks;
DROP TABLE applications;

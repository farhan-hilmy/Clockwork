-- +goose Up
-- Lets the executor count an application's running executions cheaply when it
-- enforces CLOCKWORK_APP_MAX_RUNNING at claim time.
CREATE INDEX executions_running_app_idx ON executions (application_id) WHERE status = 'running';

-- +goose Down
DROP INDEX executions_running_app_idx;

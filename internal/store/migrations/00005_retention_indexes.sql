-- +goose Up
-- Let the retention sweep (CLOCKWORK_RETENTION) find old finished history
-- without scanning whole tables. Each index covers exactly the rows it may delete.
CREATE INDEX executions_retention_idx ON executions (finished_at)
    WHERE finished_at IS NOT NULL AND status NOT IN ('queued', 'running');
CREATE INDEX webhook_deliveries_retention_idx ON webhook_deliveries (last_attempt_at)
    WHERE status <> 'pending';

-- +goose Down
DROP INDEX webhook_deliveries_retention_idx;
DROP INDEX executions_retention_idx;

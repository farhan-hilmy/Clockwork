-- +goose Up
-- Earliest time a queued execution may be claimed. Retries use it for backoff.
ALTER TABLE executions ADD COLUMN run_after timestamptz NOT NULL DEFAULT now();

DROP INDEX executions_claim_idx;
CREATE INDEX executions_claim_idx ON executions (run_after, id) WHERE status = 'queued';

-- +goose Down
DROP INDEX executions_claim_idx;
CREATE INDEX executions_claim_idx ON executions (created_at) WHERE status = 'queued';
ALTER TABLE executions DROP COLUMN run_after;

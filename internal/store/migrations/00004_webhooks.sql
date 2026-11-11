-- +goose Up
CREATE TABLE webhook_endpoints (
    id             text PRIMARY KEY,
    application_id text NOT NULL REFERENCES applications (id) ON DELETE CASCADE,
    url            text NOT NULL CHECK (length(url) BETWEEN 1 AND 2048),
    description    text NOT NULL DEFAULT '',
    events         text[] NOT NULL DEFAULT '{}', -- empty means every event type
    secret         text NOT NULL,                -- HMAC key; must be readable to sign, so it is stored as is
    enabled        boolean NOT NULL DEFAULT true,
    created_at     timestamptz NOT NULL DEFAULT now(),
    updated_at     timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX webhook_endpoints_app_idx ON webhook_endpoints (application_id) WHERE enabled;

-- One row per (event, endpoint). This is the outbox: rows are inserted in the
-- same transaction as the change the event describes, then sent by the dispatcher.
CREATE TABLE webhook_deliveries (
    id               text PRIMARY KEY,
    endpoint_id      text NOT NULL REFERENCES webhook_endpoints (id) ON DELETE CASCADE,
    application_id   text NOT NULL,
    event_id         text NOT NULL,
    event_type       text NOT NULL,
    payload          jsonb NOT NULL,
    status           text NOT NULL CHECK (status IN ('pending', 'delivered', 'failed')),
    attempts         integer NOT NULL DEFAULT 0,
    next_attempt_at  timestamptz NOT NULL,
    last_attempt_at  timestamptz,
    last_status_code integer,
    last_error       text,
    last_response    text,
    delivered_at     timestamptz,
    created_at       timestamptz NOT NULL DEFAULT now(),
    UNIQUE (event_id, endpoint_id)
);
CREATE INDEX webhook_deliveries_due_idx ON webhook_deliveries (next_attempt_at) WHERE status = 'pending';
CREATE INDEX webhook_deliveries_endpoint_idx ON webhook_deliveries (endpoint_id, id DESC);

-- +goose Down
DROP TABLE webhook_deliveries;
DROP TABLE webhook_endpoints;

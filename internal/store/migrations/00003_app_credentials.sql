-- +goose Up
-- API keys for applications. Only a SHA-256 hash of each key is stored; the key
-- itself is shown once, when it is created.
CREATE TABLE app_credentials (
    id             text PRIMARY KEY,
    application_id text NOT NULL REFERENCES applications (id) ON DELETE CASCADE,
    name           text NOT NULL CHECK (length(name) BETWEEN 1 AND 100),
    prefix         text NOT NULL,              -- first characters of the key, to recognise it in lists
    secret_hash    bytea NOT NULL UNIQUE,      -- sha256(key)
    created_at     timestamptz NOT NULL DEFAULT now(),
    last_used_at   timestamptz,
    revoked_at     timestamptz
);
CREATE INDEX app_credentials_app_idx ON app_credentials (application_id, created_at DESC);

-- +goose Down
DROP TABLE app_credentials;

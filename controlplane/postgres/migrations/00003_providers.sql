-- +goose Up
-- providers is the registry of configured vendor credentials (ADR-0023): which providers
-- are configured, their vendor type, the capabilities they serve, and which key in the
-- burrow-credentials Secret holds their token. The token itself never lives here — only the
-- non-secret registry. burrowd reads the token from the Secret at call time, so a rotation
-- needs no restart.
CREATE TABLE providers (
    name         TEXT PRIMARY KEY,
    type         TEXT NOT NULL,
    capabilities JSONB NOT NULL DEFAULT '[]'::jsonb,
    secret_key   TEXT NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL
);

-- +goose Down
DROP TABLE providers;

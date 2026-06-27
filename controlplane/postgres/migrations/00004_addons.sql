-- +goose Up
-- addons is the registry of building-block backing services (ADR-0025): which add-ons exist,
-- how they are provided (installed or connected), the concrete adapter backing them, where they
-- live, and what capabilities they serve. Like the provider registry it is the non-secret record
-- of what exists; readiness is a live property of the cluster and is never stored here — burrowd
-- probes the Deployment for that at list time, so a restart needs no reconciliation.
CREATE TABLE addons (
    name         TEXT PRIMARY KEY,
    type         TEXT NOT NULL,
    mode         TEXT NOT NULL,
    backend      TEXT NOT NULL DEFAULT '',
    image        TEXT NOT NULL DEFAULT '',
    endpoint     TEXT NOT NULL DEFAULT '',
    capabilities JSONB NOT NULL DEFAULT '[]'::jsonb,
    secret_key   TEXT NOT NULL DEFAULT '',
    created_at   TIMESTAMPTZ NOT NULL
);

-- +goose Down
DROP TABLE addons;

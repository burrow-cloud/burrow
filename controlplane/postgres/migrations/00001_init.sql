-- +goose Up
-- Initial schema: the deploy-record table and the version-stamp table.
CREATE TABLE releases (
    seq        BIGSERIAL   PRIMARY KEY,
    id         TEXT        NOT NULL UNIQUE,
    app        TEXT        NOT NULL,
    image      TEXT        NOT NULL,
    digest     TEXT        NOT NULL DEFAULT '',
    env        JSONB       NOT NULL DEFAULT '{}'::jsonb,
    command    JSONB       NOT NULL DEFAULT '[]'::jsonb,
    replicas   INTEGER     NOT NULL DEFAULT 0,
    status     TEXT        NOT NULL DEFAULT '',
    supersedes TEXT        NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL
);

CREATE INDEX releases_app_seq_idx ON releases (app, seq);

-- burrow_meta is a single-row table recording the Burrow version that last
-- migrated this database, so the startup upgrade gate can enforce single-minor-step
-- upgrades (ADR-0013). The CHECK pins the primary key to TRUE, so only one row exists.
CREATE TABLE burrow_meta (
    id      BOOLEAN PRIMARY KEY DEFAULT TRUE CHECK (id),
    version TEXT    NOT NULL
);

-- +goose Down
DROP TABLE burrow_meta;
DROP TABLE releases;

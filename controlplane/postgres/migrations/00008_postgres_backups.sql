-- +goose Up
-- postgres_backups is the control-plane index of per-app database backups (ADR-0032). burrowd is
-- not mounted to the backup PVC, so "what backups do I have?" is answered from here, not by
-- scraping the volume — the same rule that keeps the registry of control-plane state in Postgres,
-- not in the cluster. burrowd records a row when a backup Job is started (status pending) and
-- updates it to completed/failed when the Job finishes. The row names the app, the size, and the
-- on-PVC path — never a credential or a connection string (the dump's superuser password reaches
-- the Job only via secretKeyRef, never this table).
CREATE TABLE postgres_backups (
    id         TEXT PRIMARY KEY,
    app        TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    path       TEXT NOT NULL DEFAULT '',
    size_bytes BIGINT NOT NULL DEFAULT 0,
    status     TEXT NOT NULL
);

-- The read path lists by app and, within an app, newest first; an index on (app, created_at DESC)
-- serves both the per-app and the all-apps listing cheaply.
CREATE INDEX postgres_backups_app_created ON postgres_backups (app, created_at DESC);

-- +goose Down
DROP TABLE postgres_backups;

-- +goose Up
-- environment records which environment's instance a dump was taken from (ADR-0067 §1). Each
-- environment has its own Postgres instance holding its own data, so a dump is only a valid source
-- for the environment it came from: restore requires the two to agree rather than letting one
-- environment's contents be written over another's live database.
--
-- The NOT NULL DEFAULT 'default' backfills every existing row to the implicit default environment —
-- the only instance that existed when those dumps were taken — so recorded backups stay restorable
-- exactly as before.
ALTER TABLE postgres_backups ADD COLUMN environment TEXT NOT NULL DEFAULT 'default';

-- The read path filters by app and by environment and orders newest first; extend the existing
-- (app, created_at DESC) index with an environment-keyed one so a per-environment listing is served
-- as cheaply as the per-app one.
CREATE INDEX postgres_backups_env_created ON postgres_backups (environment, created_at DESC);

-- +goose Down
DROP INDEX postgres_backups_env_created;
ALTER TABLE postgres_backups DROP COLUMN environment;

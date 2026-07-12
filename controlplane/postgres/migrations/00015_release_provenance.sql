-- +goose Up
-- Provenance on the deploy record (ADR-0052 §5): every deploy records how it was triggered so the
-- deploy record and audit log stay legible. trigger is "manual" for an explicit CLI or agent deploy —
-- the default, which backfills every existing row — or "auto" for the pull-based passive watcher
-- (Phase 4b); auto_level and auto_tag capture the level that applied and the tag the watcher took, set
-- only for an auto trigger and empty otherwise. `trigger` is a Postgres reserved word, accepted here as
-- a column name in ALTER TABLE and quoted where it is read or written in the store.
ALTER TABLE releases ADD COLUMN trigger TEXT NOT NULL DEFAULT 'manual';
ALTER TABLE releases ADD COLUMN auto_level TEXT NOT NULL DEFAULT '';
ALTER TABLE releases ADD COLUMN auto_tag TEXT NOT NULL DEFAULT '';

-- reason records why auto-deploy was disabled (ADR-0052 §5): the safety stop that turns the level to
-- off when a rollback — or a manual downgrade — lands, so status and the agent can show
-- "auto-deploy: off (disabled by rollback)". Empty when the level was human-set or is the default.
ALTER TABLE app_autodeploy ADD COLUMN reason TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE app_autodeploy DROP COLUMN reason;
ALTER TABLE releases DROP COLUMN auto_tag;
ALTER TABLE releases DROP COLUMN auto_level;
ALTER TABLE releases DROP COLUMN trigger;

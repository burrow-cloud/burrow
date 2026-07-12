-- +goose Up
-- environment records the canonical environment a release was deployed into (ADR-0052 Phase 4a):
-- a named environment, or the reserved "default" for the implicit default environment. Releases move
-- from app-global to per-(app, environment) keying so an app has an independent deploy history — and
-- an independent rollback chain — in each environment, matching how the auto-deploy level is already
-- keyed per (app, environment). The NOT NULL DEFAULT 'default' backfills every existing row to the
-- default environment, so history and status keep reading exactly what they read before.
ALTER TABLE releases ADD COLUMN environment TEXT NOT NULL DEFAULT 'default';

-- +goose Down
ALTER TABLE releases DROP COLUMN environment;

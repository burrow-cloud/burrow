-- +goose Up
-- environment records which environment an add-on instance serves (ADR-0067 §1). Add-ons move from
-- one instance per type per CLUSTER to one instance per type per ENVIRONMENT: databases keep their
-- simple names, so it is the instance — not a naming convention inside a shared one — that keeps an
-- app called `web` in staging apart from an app called `web` in production. Without it, provisioning
-- the second one found the first, and because provisioning is idempotent it did not fail: it rotated
-- the role password and handed back a connection string pointing at the other environment's data.
--
-- The NOT NULL DEFAULT 'default' backfills every existing row to the implicit default environment,
-- which is the only environment an add-on could have been installed into before this. The instance's
-- resource names are unchanged for that environment, so an existing install keeps its pod, its
-- volume, and its superuser credential, and nothing migrates.
ALTER TABLE addons ADD COLUMN environment TEXT NOT NULL DEFAULT 'default';

-- +goose Down
ALTER TABLE addons DROP COLUMN environment;

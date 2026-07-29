-- +goose Up
-- The first environment is called `prod`, not `default` (ADR-0067 §2). ADR-0035 phase 2 synthesized
-- an implicit environment named `default` that was never registered; ADR-0067 makes it a real,
-- registered environment and renames it. The reason is guardrails, not naming taste: ADR-0065 §3
-- makes app.delete and dns.delete deny-by-default and expects the operator to relax them per
-- environment, and an environment called `default` invites `guard set --env default app.delete
-- allow` as the obvious way to stop the friction — at which point production has been relaxed
-- without the word ever being typed.
--
-- The canonical environment name is the KEY control-plane rows are stored under, so the rename has
-- to move with it: reading `default` and `prod` in parallel would mean two names for one
-- environment, which is the ambiguity the rename exists to remove. Every table that stores an
-- environment name is rewritten here, in one transaction.
--
-- NOTHING IN THE CLUSTER MOVES OR IS RENAMED (ADR-0067 §3). An environment's NAME and its NAMESPACE
-- are separate values: `prod` maps to the app namespace the install already uses (`burrow-apps`),
-- not to `burrow-apps-prod`, so apps stay where they are. Resource names follow the same separation
-- one level down — controlplane.AddonInstanceName gives the DEFAULT environment the unqualified name
-- by switching on the DefaultEnvironment constant rather than on its value, so `burrow-postgres`,
-- its PVC and its superuser Secret keep the names they have. This migration touches control-plane
-- metadata only; the environments registry row itself is written by burrowd at startup
-- (Engine.EnsureDefaultEnvironment), because the namespace it maps to is runtime configuration
-- (BURROW_NAMESPACE) that SQL cannot see.
--
-- The audit log is deliberately NOT rewritten. It is an append-only record of what happened
-- (ADR-0027), and an operation recorded against `default` was performed when that was the name;
-- restating history under a name that did not exist yet would make the record less true, not more.

-- Refuse rather than merge if `prod` is already in use as a DISTINCT environment. It can only be so
-- on an install that ran `burrow env add prod --namespace …` before this change, where `prod` names
-- a namespace of its own with its own apps, releases and add-on instance. Folding the old `default`
-- rows into it would silently join two environments' deploy histories and point one environment's
-- apps at the other's database — the precise class of failure ADR-0067 exists to close — so this
-- stops and asks for a human decision instead. The environments table is the primary check; the
-- others catch an environment that was registered, used, and later unregistered.
-- +goose StatementBegin
DO $$
DECLARE
    conflict TEXT;
BEGIN
    SELECT 'the environments registry' INTO conflict FROM environments WHERE name = 'prod' LIMIT 1;
    IF conflict IS NULL THEN
        SELECT 'recorded releases' INTO conflict FROM releases WHERE environment = 'prod' LIMIT 1;
    END IF;
    IF conflict IS NULL THEN
        SELECT 'registered add-ons' INTO conflict FROM addons WHERE environment = 'prod' LIMIT 1;
    END IF;
    IF conflict IS NULL THEN
        SELECT 'recorded backups' INTO conflict FROM postgres_backups WHERE environment = 'prod' LIMIT 1;
    END IF;
    IF conflict IS NULL THEN
        SELECT 'auto-deploy levels' INTO conflict FROM app_autodeploy WHERE environment = 'prod' LIMIT 1;
    END IF;
    IF conflict IS NOT NULL THEN
        RAISE EXCEPTION USING
            MESSAGE = 'an environment named "prod" already exists in ' || conflict,
            DETAIL  = 'ADR-0067 makes "prod" the name of the environment mapped to the app namespace, which this install already uses for a separate environment. Merging the two would join their deploy histories and cross their databases.',
            HINT    = 'Re-register the existing "prod" environment under another name (for example "production") before upgrading, then re-run the upgrade.';
    END IF;
END
$$;
-- +goose StatementEnd

UPDATE releases         SET environment = 'prod' WHERE environment = 'default';
UPDATE addons           SET environment = 'prod' WHERE environment = 'default';
UPDATE postgres_backups SET environment = 'prod' WHERE environment = 'default';
UPDATE app_autodeploy   SET environment = 'prod' WHERE environment = 'default';

-- The column defaults were the backfill values for the columns' own migrations and named the same
-- environment; move them with it so a row inserted without an environment lands on the environment
-- that now exists rather than on a name nothing resolves to.
ALTER TABLE releases         ALTER COLUMN environment SET DEFAULT 'prod';
ALTER TABLE addons           ALTER COLUMN environment SET DEFAULT 'prod';
ALTER TABLE postgres_backups ALTER COLUMN environment SET DEFAULT 'prod';

-- +goose Down
ALTER TABLE postgres_backups ALTER COLUMN environment SET DEFAULT 'default';
ALTER TABLE addons           ALTER COLUMN environment SET DEFAULT 'default';
ALTER TABLE releases         ALTER COLUMN environment SET DEFAULT 'default';

UPDATE app_autodeploy   SET environment = 'default' WHERE environment = 'prod';
UPDATE postgres_backups SET environment = 'default' WHERE environment = 'prod';
UPDATE addons           SET environment = 'default' WHERE environment = 'prod';
UPDATE releases         SET environment = 'default' WHERE environment = 'prod';

DELETE FROM environments WHERE name = 'prod';

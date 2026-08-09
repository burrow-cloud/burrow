-- +goose Up
-- An environment may hold more than one instance of an add-on (ADR-0091). Two columns and one key
-- change, and no new table.
--
-- `addons.name` is already a primary key with no `(type, environment)` constraint above it, so the
-- registry could ALREADY hold two rows for one environment. What it could not do is say which label
-- either row answers to, or which of them an attachment means — and an instance name that is a pure
-- function of `(type, environment)` has no answer for a second one anyway.
--
-- THE LABEL IS BACKFILLED TO THE NAME, which is the whole of ADR-0091 §2's promise that nothing on a
-- live install moves. An environment's first instance keeps the name it has (`burrow-postgres`,
-- `burrow-postgres-staging`) AND is labelled with it, so a guardrail disposition somebody has
-- already written (`prod.burrow-postgres.addon.remove`) keeps matching the instance it was written
-- for, and `addon remove burrow-postgres-staging` keeps naming the same thing. Choosing a prettier
-- label here would silently stop a disposition that reads as protection from being one.
--
-- The label is UNIQUE WITHIN AN ENVIRONMENT rather than globally. That is the granularity a
-- guardrail key needs: ADR-0085's key is `<env>.<name>.<code>`, so the environment is already a
-- separate component and two environments each holding an instance labelled `analytics` are no more
-- ambiguous than two environments each holding an app called `web`. It is not per (type,
-- environment), because the key has no type component to disambiguate with.
ALTER TABLE addons ADD COLUMN label TEXT NOT NULL DEFAULT '';
UPDATE addons SET label = name WHERE label = '';
CREATE UNIQUE INDEX addons_environment_label_key ON addons (environment, label);

-- The attachment key grows the instance it is against. `addon_attachments` was keyed by
-- (addon, app, environment) (migration 00029), which had room for exactly one row per app per
-- environment — the concrete reason an app could not read from two databases, once the variable name
-- stopped being the blocker (issue #462). An app may now hold several attachments in one
-- environment, and a second one must name its own variable, because `DATABASE_URL` is taken and
-- Burrow refuses rather than inventing `DATABASE_URL_2` (ADR-0091 §3).
--
-- THE BACKFILL IS A HISTORICAL FACT, NOT A DERIVATION THE CODE SHARES. Every attachment recorded
-- before this migration was made against the environment's one instance, whose name at that time was
-- a pure function of the add-on type and the environment — `burrow-<type>` for the first environment
-- (`prod` since migration 00018) and `burrow-<type>-<env>` for every other. Writing that name into
-- the column here fixes what those rows always meant. Nothing reads the derivation afterwards: from
-- here the registry row is the mapping between an instance's label and its name (ADR-0091 §2).
ALTER TABLE addon_attachments ADD COLUMN instance TEXT NOT NULL DEFAULT '';
UPDATE addon_attachments
   SET instance = CASE WHEN environment = 'prod' THEN 'burrow-' || addon
                       ELSE 'burrow-' || addon || '-' || environment END
 WHERE instance = '';
ALTER TABLE addon_attachments DROP CONSTRAINT addon_attachments_pkey;
ALTER TABLE addon_attachments ADD PRIMARY KEY (addon, app, environment, instance);

-- +goose Down
ALTER TABLE addon_attachments DROP CONSTRAINT addon_attachments_pkey;
DELETE FROM addon_attachments a
 USING addon_attachments b
 WHERE a.addon = b.addon AND a.app = b.app AND a.environment = b.environment
   AND a.instance > b.instance;
ALTER TABLE addon_attachments ADD PRIMARY KEY (addon, app, environment);
ALTER TABLE addon_attachments DROP COLUMN instance;
DROP INDEX addons_environment_label_key;
ALTER TABLE addons DROP COLUMN label;

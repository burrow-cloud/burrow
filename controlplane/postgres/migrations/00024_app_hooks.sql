-- +goose Up
-- app_hooks stores the lifecycle command an app runs at a named phase (ADR-0072 §1): `pre-deploy`
-- before a deploy's image reaches the cluster, `pre-rollback` before a rollback's older image does.
-- It has to be configuration rather than an argument because the path that needs it has no caller to
-- supply one — auto-deploy exists precisely so nobody has to be there.
--
-- A missing row means NO HOOK, which is today's behaviour exactly, so this table holds only what an
-- operator deliberately set. The key is (app, environment, phase), the same shape app_autodeploy
-- uses: one app can run a migration before every deploy in production and nothing in staging, and
-- the two phases are independent because they run from DIFFERENT IMAGES and must default oppositely
-- (§8) — `pre-deploy` from the image being deployed, `pre-rollback` from the image being left.
--
-- The command is stored as a JSON ARRAY (an argv), not as a string. A migration command routinely
-- carries arguments with spaces, and flattening it to a line would mean re-splitting it later with a
-- shell that is not necessarily in the image. It is the same choice releases.command already makes.
--
-- Burrow does not understand migrations (§10): this is a command, and versioning, ordering and
-- idempotency belong to the user's own tool. Nothing here is secret — the command is the salient
-- fact a reviewer reads, and the app's config and Secret reach it at run time through the Job.
CREATE TABLE app_hooks (
    app         TEXT NOT NULL,
    environment TEXT NOT NULL,
    phase       TEXT NOT NULL,
    command     JSONB NOT NULL,
    PRIMARY KEY (app, environment, phase)
);

-- +goose Down
DROP TABLE app_hooks;

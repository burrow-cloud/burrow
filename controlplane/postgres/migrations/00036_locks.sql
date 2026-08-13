-- +goose Up
-- locks records that somebody protected one thing from the operations that cannot be undone (cloud
-- ADR-0060): deleting an app, removing an add-on instance, detaching from one with --delete-data.
--
-- IT IS STATE ON THE THING, NOT POLICY ABOUT A CALLER. Guardrail dispositions live in the
-- operational-config rows and answer "may this caller do this"; a row here answers "is this thing
-- locked", the same answer for every caller including whoever wrote it. Keeping the two in separate
-- tables is the schema saying so: a lock is not relaxed by editing policy, only by an act against
-- the thing.
--
-- WHY THE CONTROL-PLANE DATABASE AND NOT THE CLUSTER. The lock has to survive a redeploy, a
-- restart and a rollback, and it has to be readable by burrowd on the destructive path. An
-- annotation on the Deployment satisfies neither well: a workload the engine re-applies is rewritten
-- from Burrow's own record of what the app is, and an add-on instance's most important lockable
-- property would then live on an object the removal is about to delete. Every other per-app fact
-- Burrow keeps — the auto-deploy level, the health endpoint, the dependency-check setting — is here
-- for the same reason, and the destructive path already reads this database before it acts.
--
-- The key is (subject, environment, name). SUBJECT distinguishes an app from an add-on instance, so
-- an app and an instance that share a name are two locks rather than one. ENVIRONMENT is part of the
-- key because everything else about an app is per-environment (releases, add-ons, health, auto-deploy)
-- and because the failure this mechanism exists for is a person acting on production while meaning
-- staging: a lock that spanned environments would protect the wrong one. NAME is the app's name, or
-- the add-on instance's LABEL — the half an operator reads and types (ADR-0091 §1, §4) — never the
-- generated cluster name.
--
-- A MISSING ROW MEANS UNLOCKED, and the row is deleted on unlock rather than flipped to false.
-- Unlocked is the state everything starts in and the overwhelming majority stay in, so a table of
-- what IS locked is the small one; and the history of who unlocked what is the audit trail's job,
-- where it belongs and cannot be overwritten by the next lock.
CREATE TABLE locks (
    subject     TEXT NOT NULL,
    environment TEXT NOT NULL,
    name        TEXT NOT NULL,
    locked_at   TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (subject, environment, name)
);

-- +goose Down
DROP TABLE locks;

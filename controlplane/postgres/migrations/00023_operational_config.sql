-- +goose Up
-- operational_config stores the operator-set value of each operational limit (ADR-0068). A limit is
-- a bound a human sets — the replica ceiling, and the retentions and thresholds that follow it — and
-- exceeding one is a validation failure rather than a policy decision, so unlike guardrail_policy
-- there is no disposition here: only a code and a value.
--
-- The key carries the TIER, the same way guardrail_policy's does: a bare limit code is the CLUSTER
-- value and `<env>.<code>` is that environment's. Resolution reads the environment's value first,
-- then the cluster's, then the built-in default in code (ADR-0068 §3), so this table holds only what
-- an operator actually set. Values are stored in their canonical text form ("50", "72h"), so the row
-- reads as what was typed.
--
-- It lives in Postgres with every other piece of control-plane state rather than in a ConfigMap: a
-- ConfigMap has no versioning story and would be a second source of truth for operator intent.
CREATE TABLE operational_config (
    code  TEXT PRIMARY KEY,
    value TEXT NOT NULL
);

-- Drop any stored disposition for app.replica_ceiling, cluster-wide and per environment. The code is
-- no longer a guardrail (ADR-0068 §2): the ceiling is a number, and a number wearing a disposition's
-- clothes is what let `guard set app.replica_ceiling allow` turn the limit off while leaving no way
-- to raise it. The row is DELETED rather than left to be ignored, because an unrecognized code
-- sitting in the policy table is a setting an operator believes is in force.
--
-- An operator who had set it to `allow` loses that and gets the ceiling back, which is the intended
-- correction and a behaviour change on upgrade. What they wanted — a higher bound — is now
-- expressible: `burrow cluster config set [--env <name>] app.replica_ceiling <n>`.
--
-- The env-prefixed form is matched by suffix because an environment name is opaque here. No other
-- guardrail code ends in `.app.replica_ceiling`, so the pattern cannot reach one.
DELETE FROM guardrail_policy
 WHERE code = 'app.replica_ceiling'
    OR code LIKE '%.app.replica_ceiling';

-- +goose Down
-- The dropped dispositions are not restored: they were deleted because they had ceased to mean
-- anything, and inventing rows for codes nobody set would be worse than their absence. A rollback
-- returns to the compiled-in ceiling of 50 with no override, which is the state a fresh install of
-- the earlier version had.
DROP TABLE operational_config;

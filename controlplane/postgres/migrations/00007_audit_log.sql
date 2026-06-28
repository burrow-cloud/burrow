-- +goose Up
-- audit_log is the append-only record of guarded, mutating control-plane operations and the
-- guardrail decisions that applied (ADR-0027). One row per decision and one per execution
-- outcome, so a held operation and its later confirmed run read as two rows. The control plane
-- only ever INSERTs here and SELECTs for the read path (`burrow audit`); there is no update or
-- delete path through the API — retention is an out-of-band operator concern.
--
-- args is redacted by construction at write time: it holds only safe metadata (app/host/addon
-- name, image reference, replica count, env/secret key NAMES) and never an env value or any
-- secret value (ADR-0027/ADR-0004). caller is coarse until an authentication model exists; the
-- column is reserved so identity can be enriched later without a migration of meaning.
CREATE TABLE audit_log (
    id             BIGSERIAL PRIMARY KEY,
    ts             TIMESTAMPTZ NOT NULL,
    operation      TEXT NOT NULL,
    target         TEXT NOT NULL DEFAULT '',
    args           JSONB NOT NULL DEFAULT '{}'::jsonb,
    guardrail_code TEXT NOT NULL DEFAULT '',
    disposition    TEXT NOT NULL DEFAULT '',
    outcome        TEXT NOT NULL,
    result         TEXT NOT NULL DEFAULT '',
    caller         TEXT NOT NULL DEFAULT ''
);

-- The read path filters by target (app/host/addon) and/or operation and/or outcome, always
-- newest-first. An index on id DESC serves the common unfiltered "latest N" tail cheaply.
CREATE INDEX audit_log_id_desc ON audit_log (id DESC);
CREATE INDEX audit_log_target ON audit_log (target);
CREATE INDEX audit_log_operation ON audit_log (operation);

-- +goose Down
DROP TABLE audit_log;

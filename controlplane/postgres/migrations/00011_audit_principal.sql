-- +goose Up
-- principal is the acting identity behind a guarded operation — the actor, distinct from the
-- caller (the control-plane boundary that authenticated the request). It threads the principal
-- (actor) concept through the audit trail now so per-user SSO attribution is additive later
-- (ADR-0038): every existing and new row is seeded with the shared-agent constant, so a later
-- SSO change fills in a real identity rather than migrating the meaning of stored rows. The
-- NOT NULL DEFAULT '' mirrors the caller column: the schema reserves room to enrich identity
-- without a migration of meaning.
ALTER TABLE audit_log ADD COLUMN principal TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE audit_log DROP COLUMN principal;

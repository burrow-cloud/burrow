-- +goose Up
-- An object-storage provider carries more non-secret configuration than a DNS or source provider
-- does (ADR-0063 §1): where the store is, which bucket Burrow recorded, how long backups written
-- there must stay restorable, and the NAMES of the two burrow-credentials keys holding the
-- credential pair. The values themselves stay in the Secret, exactly as every other provider's
-- token does — this is one namespaced SET of keys per provider, not a new credential mechanism.
--
-- Columns rather than a JSON blob, because these are the fields ADR-0063 says live in the row so
-- the destination can be inspected without reading a Secret at all. They are empty for every
-- provider type that has no destination, which is why each carries a default.
ALTER TABLE providers
    ADD COLUMN endpoint              TEXT NOT NULL DEFAULT '',
    ADD COLUMN region                TEXT NOT NULL DEFAULT '',
    -- The bucket is RECORDED, never derived: Burrow only ever writes to the bucket it knows about
    -- (ADR-0063 §4), because inferring one by name is how a tool writes into somebody else's.
    ADD COLUMN bucket                TEXT NOT NULL DEFAULT '',
    ADD COLUMN bucket_created        BOOLEAN NOT NULL DEFAULT FALSE,
    ADD COLUMN access_key_id_key     TEXT NOT NULL DEFAULT '',
    ADD COLUMN secret_access_key_key TEXT NOT NULL DEFAULT '',
    -- The retention window the bucket's lifecycle rules were reconciled against (ADR-0063 §3).
    -- Zero means no window was declared.
    ADD COLUMN retention_days        INTEGER NOT NULL DEFAULT 0;

-- +goose Down
ALTER TABLE providers
    DROP COLUMN endpoint,
    DROP COLUMN region,
    DROP COLUMN bucket,
    DROP COLUMN bucket_created,
    DROP COLUMN access_key_id_key,
    DROP COLUMN secret_access_key_key,
    DROP COLUMN retention_days;

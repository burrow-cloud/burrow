-- +goose Up
-- Where a backup's bytes actually went, and why they did not (ADR-0063 §7).
--
-- destination is written when the row is created, from what was registered AT THAT MOMENT. It is
-- not derived on read from the current provider list, because registering an object store today
-- does not make last month's in-cluster dumps durable and a listing must not imply that it did.
--
-- The NOT NULL DEFAULT 'cluster' backfills every existing row truthfully: before this change there
-- was no write path off the cluster, so every recorded backup reached the PersistentVolumeClaim and
-- nowhere else. That is the honest backfill, and it is the one that keeps the listing readable as
-- "which of my backups would survive losing this cluster".
ALTER TABLE postgres_backups ADD COLUMN destination TEXT NOT NULL DEFAULT 'cluster';

-- provider and object_key address the dump at the vendor: the registry NAME of the object-storage
-- provider and the key the object occupies in its bucket. The endpoint, region, bucket and
-- credential keys live on the providers row and are deliberately not copied here — a backup row
-- points at the registration, so rotating a credential or correcting an endpoint does not have to
-- rewrite history.
ALTER TABLE postgres_backups ADD COLUMN provider TEXT NOT NULL DEFAULT '';
ALTER TABLE postgres_backups ADD COLUMN object_key TEXT NOT NULL DEFAULT '';

-- failure_reason is a member of a closed set — the backup reasons of ADR-0063 §7, or ADR-0074 §2's
-- IssueReason set when the Job never started — so a reader BRANCHES on it instead of parsing prose.
-- failure_detail is one Burrow-authored line: an HTTP status, an attempt count, a length mismatch.
-- Neither ever holds a vendor response body or a credential, which is why the vendor's own text is
-- left in the Job's pod log rather than carried into the registry.
ALTER TABLE postgres_backups ADD COLUMN failure_reason TEXT NOT NULL DEFAULT '';
ALTER TABLE postgres_backups ADD COLUMN failure_detail TEXT NOT NULL DEFAULT '';

-- The signal ADR-0063 §7 alerts on is the age of the last SUCCESSFUL backup, which is this query:
-- newest completed row, optionally per app. Indexing status with created_at DESC keeps it a single
-- index read as the backup history grows, and it is the same index the run of failures since that
-- success is read from — the transition shape ADR-0074 §4 describes, computed from these rows
-- rather than from a second history table that could disagree with them.
CREATE INDEX postgres_backups_status_created ON postgres_backups (status, created_at DESC);

-- +goose Down
DROP INDEX postgres_backups_status_created;
ALTER TABLE postgres_backups DROP COLUMN failure_detail;
ALTER TABLE postgres_backups DROP COLUMN failure_reason;
ALTER TABLE postgres_backups DROP COLUMN object_key;
ALTER TABLE postgres_backups DROP COLUMN provider;
ALTER TABLE postgres_backups DROP COLUMN destination;

-- +goose Up
-- kind records WHICH MECHANISM produced a backup row (ADR-0066 §4).
--
-- Two coexist deliberately and they are not interchangeable. A LOGICAL backup is `pg_dump -Fc` of one
-- app's database through a Job Burrow runs; it restores that one app to the moment of the dump and
-- touches no other. A PHYSICAL backup is a CloudNativePG `Backup` object over the whole instance,
-- written to the pgBackRest repository with the write-ahead log behind it; it restores every database
-- on the instance to any point in the archive window, and it cannot be restored per app at all.
--
-- Without this column the two are told apart by an absent `app`, which is a fact about a row rather
-- than a statement of intent, and the operation a reader reaches for under pressure would depend on
-- inferring one from the other.
--
-- The NOT NULL DEFAULT backfills every existing row to `logical`, which is exact rather than a guess:
-- no physical backup can predate the column, because nothing could take one.
ALTER TABLE postgres_backups ADD COLUMN kind TEXT NOT NULL DEFAULT 'logical';

-- A physical backup belongs to no app, so `app` is empty on those rows and the (app, created_at)
-- index no longer serves the listing that spans them. Ordering by kind and time is what the health
-- surface and the listing both read.
CREATE INDEX postgres_backups_kind_created ON postgres_backups (kind, created_at DESC);

-- +goose Down
DROP INDEX postgres_backups_kind_created;
ALTER TABLE postgres_backups DROP COLUMN kind;

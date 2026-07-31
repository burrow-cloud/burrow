-- +goose Up
-- failure_ledger is the durable record of what the cluster DID after Burrow acted on it (ADR-0074
-- §4). Kubernetes Events expire in an hour, so an app that crash-looped from 02:00 to 02:40 and
-- recovered is by morning indistinguishable from one that has been up for a week — and "when did it
-- start" is the first question anyone asks. Nothing reconstructs that afterwards, so it is recorded
-- at the time or not at all.
--
-- IT IS NOT THE AUDIT LOG AND MUST NOT BE MERGED WITH IT (ADR-0074 §7). audit_log records what
-- Burrow was asked to do and what it decided; it is append-only and its value depends on being
-- complete. This records what happened afterwards, mostly unrequested by anyone, and it is PRUNED
-- (see below). Merging them would subject a security-relevant append-only record to that pruning and
-- make "the audit log is complete" false. They are read together during an incident, which is an
-- argument for a shared clock and a shared object identity, not a shared table.
--
-- ONE ROW PER (OBJECT, REASON), NOT AN EVENT STREAM. An event stream has higher fidelity and is
-- unreadable at exactly the moment it matters: an incident produces thousands of rows describing one
-- problem and the reader has to reconstruct the transition themselves. A row here answers the two
-- questions people actually ask — when did this start, is it still happening — in one line.
--
-- No column here can carry a secret value (ADR-0074 §9). object is a name Burrow assigned or was
-- given, reason is a member of a closed vocabulary, and detail is one bounded Burrow-authored line
-- that the writer passes through a single gate which drops everything after the first newline —
-- which is where the live status surface puts a crash-looping container's own log output.
CREATE TABLE failure_ledger (
    id          BIGSERIAL PRIMARY KEY,
    kind        TEXT NOT NULL,
    object      TEXT NOT NULL,
    environment TEXT NOT NULL DEFAULT '',
    reason      TEXT NOT NULL,
    detail      TEXT NOT NULL DEFAULT '',
    first_seen  TIMESTAMPTZ NOT NULL,
    last_seen   TIMESTAMPTZ NOT NULL,
    resolved_at TIMESTAMPTZ,
    occurrences BIGINT NOT NULL DEFAULT 1
);

-- The uniqueness is PARTIAL, and that is the whole design (ADR-0074 §5). At most one ACTIVE row may
-- exist per (object, reason), so a standing failure accumulates into one row with a count instead of
-- a row per observation. Resolved rows are exempt, so a thing that broke, recovered, and broke again
-- reads as two episodes rather than one long outage. And because the reason is part of the key, a
-- pod that is OOM-killed AND unschedulable holds two rows with independent lifetimes — a single
-- status column per object would silently drop the second, which is the failure this table is about.
CREATE UNIQUE INDEX failure_ledger_active ON failure_ledger (kind, object, environment, reason)
    WHERE resolved_at IS NULL;

-- The listing reads active rows oldest-first (the earliest first_seen in a cascade is the likeliest
-- thing to actually fix) and the history by last_seen within a window; retention deletes by
-- resolved_at. One index each, so none of the three degrades as the table grows.
CREATE INDEX failure_ledger_first_seen ON failure_ledger (first_seen);
CREATE INDEX failure_ledger_last_seen ON failure_ledger (last_seen);
CREATE INDEX failure_ledger_resolved_at ON failure_ledger (resolved_at);

-- observation_windows is what stops a gap in the ledger from reading as health. If the observer was
-- down from 02:00 to 03:00, an empty ledger for that hour says "nothing broke", and a reliability
-- surface that fails silently would be a particularly poor joke (ADR-0074's consequences). Each row
-- is one continuous RUN of the observer: a restart begins a new row, so the time in between stays a
-- literal gap that a reader can see.
--
-- until advances only on a sweep that completed its enumeration of what Burrow manages, so an
-- observer that is running but wedged stops claiming coverage rather than claiming it falsely.
-- degraded_sweeps counts the sweeps that ran but could not read every object, which is partial
-- coverage and is reported as such rather than as clean coverage.
CREATE TABLE observation_windows (
    id              BIGSERIAL PRIMARY KEY,
    started_at      TIMESTAMPTZ NOT NULL,
    until           TIMESTAMPTZ NOT NULL,
    sweeps          BIGINT NOT NULL DEFAULT 0,
    degraded_sweeps BIGINT NOT NULL DEFAULT 0,
    detail          TEXT NOT NULL DEFAULT ''
);

-- The read path asks for the windows overlapping a time range, and retention deletes by the same
-- column.
CREATE INDEX observation_windows_until ON observation_windows (until);

-- +goose Down
DROP TABLE observation_windows;
DROP TABLE failure_ledger;

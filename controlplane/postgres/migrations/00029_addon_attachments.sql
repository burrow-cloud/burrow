-- +goose Up
-- addon_attachments records the ONE thing an attachment could not previously say: the name of the
-- environment variable the connection string was written under (ADR-0031, issue #462).
--
-- Attach had no row of its own. Whether an app was attached is derived by asking the environment's
-- Postgres instance whether a database exists for it, which is a better answer than a stored one —
-- it cannot drift from the database it describes — and it stays the answer. But a DERIVED fact can
-- only produce a name that was never chosen, so the key was a constant: every attached app read
-- DATABASE_URL whether or not that was the name it wanted, and detach, credential rotation and the
-- physical restore's cutover all agreed on it by all hard-coding the same string.
--
-- This is the chosen name and nothing else. It is not a second copy of the attachment: the row does
-- not say an attachment EXISTS, only what it was named if it does, so a row surviving a database
-- that was dropped by hand renames nothing and provisions nothing.
--
-- A MISSING ROW MEANS DATABASE_URL. Every attachment made before this table existed was written
-- under that name, so a missing row is the answer for all of them rather than an error, and an
-- attach that names no variable keeps writing there. The default did not move; a choice was added
-- beside it.
--
-- Keyed by (addon, app, environment): each environment has its own instance (ADR-0067 §1), so the
-- same app attached in staging and in production is two attachments that may be named differently,
-- and the add-on type is in the key so the row stays correct if a second attachable type is added.
CREATE TABLE addon_attachments (
    addon       TEXT NOT NULL,
    app         TEXT NOT NULL,
    environment TEXT NOT NULL,
    env_key     TEXT NOT NULL,
    updated_at  TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (addon, app, environment)
);

-- +goose Down
DROP TABLE addon_attachments;

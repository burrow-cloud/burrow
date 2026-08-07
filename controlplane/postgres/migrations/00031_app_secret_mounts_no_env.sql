-- +goose Up
-- no_env marks a mounted key FILE-ONLY (ADR-0089 §4): projected into the file the row already
-- describes, and kept out of the container's environment. It is the half of that record that
-- actually removes a credential from /proc/<pid>/environ, where every child process inherits it.
--
-- It is a COLUMN ON THE MOUNT rather than a table of its own, and that is the rule "a key can be
-- file-only only if it is mounted" expressed in the schema: there is no row here without a
-- projection, so there is nowhere to record a key that reaches the app by no route at all, and
-- unmounting takes the marking with the file.
--
-- DEFAULT FALSE is the record's own default — mounting adds a file and leaves the variable alone,
-- because the code that reads the file has to be deployed before the variable it replaces
-- disappears. Every mount made before this column existed is exactly that, so the backfill the
-- default performs is the right answer rather than a convenient one.
ALTER TABLE app_secret_mounts ADD COLUMN no_env BOOLEAN NOT NULL DEFAULT FALSE;

-- +goose Down
ALTER TABLE app_secret_mounts DROP COLUMN no_env;

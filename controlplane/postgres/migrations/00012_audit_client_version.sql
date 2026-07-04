-- +goose Up
-- client_version is the release version of the client (CLI or MCP server) that drove a guarded
-- operation, read from the X-Burrow-Client-Version handshake header (ADR-0039). It sits next to the
-- principal in the trail — who did what, with which client, against which server version — so version
-- skew is legible after the fact, not just at request time. The NOT NULL DEFAULT '' mirrors the
-- caller and principal columns: a pre-handshake or pre-migration writer records no version, and every
-- existing row is seeded with '' so adding the column migrates no stored row's meaning.
ALTER TABLE audit_log ADD COLUMN client_version TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE audit_log DROP COLUMN client_version;

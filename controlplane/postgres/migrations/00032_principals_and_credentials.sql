-- +goose Up
-- Who Burrow knows, and what they hold (ADR-0084 §2, §3). Until now there is one token, in one
-- Secret, presented identically by the operator, the second person who joined, the agent and a CI
-- job — so burrowd cannot tell them apart, cannot revoke one of them, and records a literal in the
-- audit column that is supposed to say who acted.
--
-- Two tables. A principal is somebody Burrow knows; a credential is one token they hold. Neither
-- creates a Kubernetes object: the credential is a random string burrowd generates and stores the
-- HASH of, so burrowd needs no `create` on serviceaccounts, no `bind`, and no `tokenreviews` — the
-- permissions that get harder to obtain exactly where per-person access matters most.

-- ADMIN IS A COLUMN, and that is the whole of the decision this migration carries. "Who may issue a
-- credential" is answered by a property burrowd records about a principal, never inferred from who
-- ran the install. An identity provider later supplies the answer to "is this caller an admin"
-- without the concept having to be invented at that moment; if admin-ness were a special case of
-- "the person who installed", SSO would have nothing to attach to.
--
-- name is the principal's handle and is UNIQUE, so a second claim of the same name is a conflict
-- rather than a duplicate identity. revoked_at NULL is the active state: a principal is retired by
-- being marked, not deleted, because the audit rows that name it have to keep meaning something.
CREATE TABLE principals (
    id         TEXT PRIMARY KEY,
    name       TEXT        NOT NULL UNIQUE,
    admin      BOOLEAN     NOT NULL DEFAULT FALSE,
    created_at TIMESTAMPTZ NOT NULL,
    revoked_at TIMESTAMPTZ
);

-- One row per issued token.
--
-- token_hash holds a SHA-256 of the token and the token itself is returned exactly once, at
-- issuance, and never stored. It is UNIQUE because lookup happens by hash on every authenticated
-- request, and a unique index is what makes that a single probe.
--
-- kind is `user`, `agent` or `machine` (ADR-0084 §3). IT IS SET AT ISSUANCE AND READ FROM THIS ROW,
-- never from the request: a caller-declared kind would make a `deny` that binds the agent
-- cooperative, which is the guardrail story collapsing (ADR-0020 requires deny to hold "even
-- against an over-eager or misbehaving agent"). `machine` has no user today and is named now
-- because leaving it out is what produces a second mechanism the first time CI asks.
--
-- expires_at NULL means the credential does not expire, and revoked_at NULL means it is live.
-- Both are here rather than in a follow-up because a credentials table without an expiry and a
-- revocation is not finished — a stolen token reaches burrowd without appearing in the cluster's
-- own audit trail, and these two columns are the answer to that.
--
-- ON DELETE CASCADE: a principal that is genuinely deleted takes its tokens with it. The ordinary
-- retirement path marks revoked_at instead and keeps both rows.
CREATE TABLE credentials (
    id           TEXT PRIMARY KEY,
    principal_id TEXT        NOT NULL REFERENCES principals(id) ON DELETE CASCADE,
    kind         TEXT        NOT NULL,
    token_hash   TEXT        NOT NULL UNIQUE,
    created_at   TIMESTAMPTZ NOT NULL,
    expires_at   TIMESTAMPTZ,
    revoked_at   TIMESTAMPTZ
);

-- Listing a principal's credentials is the other read: an admin reviewing who holds what, and the
-- revoke that follows from it.
CREATE INDEX credentials_principal_id_idx ON credentials (principal_id);

-- +goose Down
DROP TABLE credentials;
DROP TABLE principals;

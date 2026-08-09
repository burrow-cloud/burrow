-- +goose Up
-- Handing a credential to a second person, without the credential travelling (ADR-0084 §2).
--
-- An admin can already record a principal and issue them a token. What that alone produces is a
-- working credential in the admin's terminal, which then has to reach the other person somehow — a
-- chat window, an email, a paste buffer — and a bearer token that has been through any of those is
-- a bearer token somebody else may also be holding. The token is long-lived and full-powered, so
-- there is no moment at which the exposure ends.
--
-- So an invitation is a credential that can do exactly one thing: be exchanged, once, on the
-- recipient's own machine, for the credential they will actually carry. That one is generated
-- there, returned there, and never travels at all.
--
-- enrollment is what marks the difference. It is FALSE for every credential that authenticates a
-- caller — which is every row that exists today, hence the default — and TRUE only for an
-- invitation. burrowd reads it on every lookup and refuses an enrollment credential at every route
-- but the exchange, so an invitation that leaks buys an attacker the ability to become a principal
-- who has been given nothing yet, and not the ability to act as anybody.
--
-- It is a column beside `kind` rather than a fourth kind, because it is not what HOLDS the
-- credential — the answer to that is still `user`, `agent` or `machine` (ADR-0084 §3), and the
-- credential the exchange returns is a `user` one. It is how far the credential reaches, which is a
-- different question and would make `kind` answer two.
ALTER TABLE credentials ADD COLUMN enrollment BOOLEAN NOT NULL DEFAULT FALSE;

-- +goose Down
ALTER TABLE credentials DROP COLUMN enrollment;

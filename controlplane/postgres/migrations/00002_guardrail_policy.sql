-- +goose Up
-- guardrail_policy stores the operator-configured disposition (allow/confirm/deny) for
-- each guardrail. Unset guardrails fall back to the built-in defaults in code
-- (DefaultPolicy), so this table holds only overrides — `guard set` writes here (ADR-0020).
CREATE TABLE guardrail_policy (
    code        TEXT PRIMARY KEY,
    disposition TEXT NOT NULL
);

-- +goose Down
DROP TABLE guardrail_policy;

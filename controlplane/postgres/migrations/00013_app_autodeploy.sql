-- +goose Up
-- app_autodeploy stores the per-app, per-environment auto-deploy level (ADR-0052 §2): how far the
-- pull-based passive watcher may move an app on its own (off/patch/minor/major). It is set by
-- `burrow app auto-deploy` — a human operator action, never the agent (ADR-0052 §6, ADR-0038).
--
-- A missing row means the app runs at the built-in default (minor), so this table holds only
-- overrides: an app never needs a row to have a level, exactly as guardrail_policy holds only
-- overrides on the built-in defaults. The level is keyed by (app, environment) so one app can sit at
-- `patch` in prod while running `major` in staging; the implicit default environment is stored under
-- the reserved name "default".
CREATE TABLE app_autodeploy (
    app         TEXT NOT NULL,
    environment TEXT NOT NULL,
    level       TEXT NOT NULL,
    PRIMARY KEY (app, environment)
);

-- +goose Down
DROP TABLE app_autodeploy;

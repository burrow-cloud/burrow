-- +goose Up
-- app_env is the per-app store of non-secret environment configuration (ADR-0028): the
-- single source of truth for an app's env, managed independently of deploy. Deploy, rollback,
-- and env mutation all render the current store contents inline into the pod template, so a
-- change rolls the Deployment with no ConfigMap or checksum trick. Secrets do not live here —
-- only non-secret config.
CREATE TABLE app_env (
    app   TEXT NOT NULL,
    key   TEXT NOT NULL,
    value TEXT NOT NULL,
    PRIMARY KEY (app, key)
);

-- +goose Down
DROP TABLE app_env;

-- +goose Up
-- app_health records the health endpoint a user or their agent DECLARED for an app (ADR-0076 §5):
-- the path their application serves its readiness answer on, and optionally the port it serves it
-- on. It is intent, like exposures is, and it is the opt-in that turns Burrow's conservative default
-- (a TCP connect on the published port, or no probe at all when no port is known — §3) into an HTTP
-- check against an endpoint that can actually say whether the app is ready to serve.
--
-- It has to be a row rather than a field on a release because the endpoint outlives any one release:
-- it is declared once, when the endpoint is added to the application's code, and every subsequent
-- deploy, rollback, and config reapply renders it into the workload. A release-scoped value would be
-- lost by the next deploy, which is the one place it must not be.
--
-- Keyed by (app, environment) — the key exposures and app_autodeploy already use — because the same
-- app in two environments can serve on two different ports, and a probe is per workload.
--
-- port is nullable-by-convention as 0: "use whatever port the app is published on", resolved at
-- apply time from the exposure, so moving an exposure to a new port does not require the health
-- endpoint to be declared again.
CREATE TABLE app_health (
    app         TEXT NOT NULL,
    environment TEXT NOT NULL,
    path        TEXT NOT NULL,
    port        INTEGER NOT NULL DEFAULT 0,
    updated_at  TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (app, environment)
);

-- +goose Down
DROP TABLE app_health;

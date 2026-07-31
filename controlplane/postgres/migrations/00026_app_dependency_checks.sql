-- +goose Up
-- app_dependency_checks records a decision ABOUT Burrow's own default, not a dependency (ADR-0076
-- §4). The deploy-time dependency check is derived from what Burrow provisioned — the database it
-- attached, the port it published — so there is deliberately nothing here describing what to check:
-- a stored list would be a second copy of the registry, free to drift from the thing it describes,
-- and the whole point of §4 is that the list is a fact Burrow computes rather than one a user
-- maintains.
--
-- What IS stored is whether the default runs. ADR-0072 described the post-deploy phase as
-- user-configured, and §4 puts a Burrow-SUPPLIED hook on it, so a command the user never configured
-- can now run in their image after every deploy. ADR-0076's consequences require that to be visible
-- and disableable rather than silent, and this table is the disableable half.
--
-- A MISSING ROW MEANS ENABLED. The check is the default, so a row exists only where somebody made a
-- decision about it: every app that has never thought about this has no row and is checked, and
-- turning it off is what writes one. That is why enabled is stored explicitly rather than the table
-- being a list of disabled apps — re-enabling leaves a row saying so, which is the difference
-- between "this was considered" and "this was never touched".
--
-- Keyed by (app, environment), the key app_health, exposures and app_autodeploy already use: the
-- same app in two environments has different dependencies (one instance per environment, ADR-0067
-- §1), and an operator may reasonably want production checked and a scratch environment not.
CREATE TABLE app_dependency_checks (
    app         TEXT NOT NULL,
    environment TEXT NOT NULL,
    enabled     BOOLEAN NOT NULL,
    updated_at  TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (app, environment)
);

-- +goose Down
DROP TABLE app_dependency_checks;

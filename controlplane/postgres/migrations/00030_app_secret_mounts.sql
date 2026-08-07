-- +goose Up
-- app_secret_mounts records which of an app's SECRET KEYS are projected into files, and under what
-- filename (ADR-0089 §1, §5). It holds a key name and a filename and never a value: the value lives
-- only in the per-app Kubernetes Secret (ADR-0029), and a mount is a reference to it, so what
-- Postgres learns here is exactly what it already knew — that the key exists.
--
-- It is a row rather than a field on a release because a mount describes how a credential is
-- delivered to whatever code is running, not what that code is. If it rode the release, rolling
-- back to a release cut before the mount existed would REMOVE THE FILE THE RUNNING APP NEEDS — a
-- rollback, the incident escape hatch, would take the credential with it. Config already behaves
-- this way, and a mount is config.
--
-- Keyed by (app, environment, key), the key app_health and exposures already use: the same app in
-- two environments holds two different Secrets, so what it projects out of them is per environment
-- too.
CREATE TABLE app_secret_mounts (
    app         TEXT NOT NULL,
    environment TEXT NOT NULL,
    key         TEXT NOT NULL,
    filename    TEXT NOT NULL,
    updated_at  TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (app, environment, key)
);

-- app_secret_dirs records the one directory an app's mounted keys land in when it is not the
-- default /run/secrets. It is a SEPARATE TABLE rather than a column on the mounts above because the
-- directory is a property of the APP and never of a key (ADR-0089 §2): a per-key path forces a
-- subPath mount, which is the one form of Secret volume the kubelet does not update in place, and it
-- can shadow a file in the app's own image. A column beside filename would have made a per-key
-- directory expressible, and the schema is the cheapest place to rule that out.
--
-- An app with no row here uses the default, so the ordinary case writes nothing.
CREATE TABLE app_secret_dirs (
    app         TEXT NOT NULL,
    environment TEXT NOT NULL,
    directory   TEXT NOT NULL,
    updated_at  TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (app, environment)
);

-- +goose Down
DROP TABLE app_secret_dirs;
DROP TABLE app_secret_mounts;

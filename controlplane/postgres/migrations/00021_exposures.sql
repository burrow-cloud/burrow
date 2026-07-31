-- +goose Up
-- exposures records what Burrow was told to make reachable: the host, the port, and whether a
-- certificate was asked for (ADR-0074 §6). Until now that intent lived only in the Ingress itself,
-- which means deleting the Ingress deleted the evidence that it was ever supposed to exist — and an
-- exposure whose Ingress is gone looks, from the cluster side, exactly like an app that was never
-- exposed. This is the row that tells them apart.
--
-- It records INTENT only. Whether the host currently resolves, whether the ingress controller has
-- assigned an address, and whether the certificate has been issued are all live reads of the cluster
-- and are deliberately not cached here (ADR-0074 §1): a cached answer is stalest during the incident
-- it exists to help.
--
-- Keyed by (app, environment) because the same app in two environments is exposed at two different
-- hosts, and one being down says nothing about the other.
CREATE TABLE exposures (
    app         TEXT NOT NULL,
    environment TEXT NOT NULL,
    host        TEXT NOT NULL,
    port        INTEGER NOT NULL DEFAULT 0,
    tls         BOOLEAN NOT NULL DEFAULT FALSE,
    created_at  TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (app, environment)
);

-- +goose Down
DROP TABLE exposures;

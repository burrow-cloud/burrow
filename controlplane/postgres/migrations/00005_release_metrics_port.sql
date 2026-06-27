-- +goose Up
-- metrics_port records the container port an app serves Prometheus metrics on, when set at
-- deploy time. The deploy annotates the pod (prometheus.io/scrape, /port, /path) so the metrics
-- add-on's scraper discovers it (ADR-0026). Zero means no metrics annotations — the default, so
-- existing releases stay unchanged.
ALTER TABLE releases ADD COLUMN metrics_port INTEGER NOT NULL DEFAULT 0;

-- +goose Down
ALTER TABLE releases DROP COLUMN metrics_port;

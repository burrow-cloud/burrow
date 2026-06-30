-- +goose Up
-- environments registers named app environments for namespace-per-environment (ADR-0035 phase 2):
-- one cluster, several app namespaces, one per environment. Each row maps an environment name to
-- the Kubernetes namespace its apps deploy into. `burrow env add` creates that namespace and grants
-- burrowd a Role there kubeconfig-side (least privilege — burrowd cannot create namespaces or RBAC
-- itself), then registers the row here, so the registry of environments is control-plane state in
-- Postgres (not a ConfigMap or local file).
--
-- The implicit "default" environment is NOT stored here: it is the app namespace burrowd already
-- runs against (BURROW_NAMESPACE), behaving exactly like today, and is synthesized by the engine and
-- listed first. This table holds only the additional, operator-created environments.
CREATE TABLE environments (
    name       TEXT PRIMARY KEY,
    namespace  TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- +goose Down
DROP TABLE environments;

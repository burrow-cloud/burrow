// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/burrow-cloud/burrow/controlplane"
)

const addonColumns = `name, type, label, environment, mode, backend, image, endpoint, capabilities, secret_key, created_at`

// SaveAddon upserts an add-on in the registry by name (ADR-0025). It records the non-secret
// registry entry — type, mode, backend, where it lives, and the capabilities it serves. Ready is
// a live property of the cluster and is never persisted; it is probed at list time.
func (s *Store) SaveAddon(ctx context.Context, a controlplane.AddonInfo) error {
	if a.Name == "" {
		return fmt.Errorf("postgres: save addon: empty name")
	}
	caps := a.Capabilities
	if caps == nil {
		caps = []string{}
	}
	capsJSON, err := json.Marshal(caps)
	if err != nil {
		return fmt.Errorf("postgres: save addon %s: encoding capabilities: %w", a.Name, err)
	}
	// An add-on row always names its environment: the registry key is the instance name, which is
	// derived FROM the environment, so the environment is the fact and the name is what follows from
	// it (ADR-0067 §1). A caller that leaves it empty means the default environment (ADR-0067 §2).
	env := a.Environment
	if env == "" {
		env = controlplane.DefaultEnvironment
	}
	// A row always carries a label, and a caller that names none means the instance is addressed by
	// its own name — which is exactly what an environment's first instance is (ADR-0091 §2) and what
	// every row written before labels existed was backfilled to. Defaulting here rather than
	// refusing keeps a connected backend, which has no notion of an instance label, a valid row.
	label := a.Label
	if label == "" {
		label = a.Name
	}
	const q = `
INSERT INTO addons (name, type, label, environment, mode, backend, image, endpoint, capabilities, secret_key, created_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9::jsonb, $10, $11)
ON CONFLICT (name) DO UPDATE SET
    type = EXCLUDED.type,
    label = EXCLUDED.label,
    environment = EXCLUDED.environment,
    mode = EXCLUDED.mode,
    backend = EXCLUDED.backend,
    image = EXCLUDED.image,
    endpoint = EXCLUDED.endpoint,
    capabilities = EXCLUDED.capabilities,
    secret_key = EXCLUDED.secret_key,
    created_at = EXCLUDED.created_at`
	if _, err := s.db.ExecContext(ctx, q, a.Name, string(a.Type), label, env, a.Mode, a.Backend, a.Image, a.Endpoint, string(capsJSON), a.SecretKey, a.CreatedAt); err != nil {
		return fmt.Errorf("postgres: save addon %s: %w", a.Name, err)
	}
	return nil
}

// AddonByLabel returns the instance labelled label in environment env, or ErrNotFound. It is the
// registry's half of ADR-0091 §2: an instance's name in the cluster is looked up, never derived, and
// the label is what a person types.
//
// A label is unique WITHIN an environment (migration 00035), which is what makes this a single-row
// answer without a type argument — and the same granularity ADR-0085's `<env>.<name>.<code>`
// guardrail key already assumes.
func (s *Store) AddonByLabel(ctx context.Context, env, label string) (controlplane.AddonInfo, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+addonColumns+` FROM addons WHERE environment = $1 AND label = $2`, env, label)
	a, err := scanAddon(row)
	if errors.Is(err, sql.ErrNoRows) {
		return controlplane.AddonInfo{}, fmt.Errorf("postgres: addon %q in environment %q: %w", label, env, controlplane.ErrNotFound)
	}
	if err != nil {
		return controlplane.AddonInfo{}, fmt.Errorf("postgres: addon %q in environment %q: %w", label, env, err)
	}
	return a, nil
}

// AddonsInEnvironment returns the registered instances of add-on type t serving env, label order.
// None yields an empty slice and no error.
//
// It exists because an environment may hold more than one (ADR-0091 §1), so a question about "the"
// instance of a type in an environment is now a question about a set: which ones a listing shows,
// and which ones an operation that must consider every instance — a dependency check asking whether
// an app holds a database anywhere in this environment — has to look at.
func (s *Store) AddonsInEnvironment(ctx context.Context, t controlplane.AddonType, env string) ([]controlplane.AddonInfo, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+addonColumns+` FROM addons WHERE type = $1 AND environment = $2 ORDER BY label`, string(t), env)
	if err != nil {
		return nil, fmt.Errorf("postgres: addons of type %q in environment %q: %w", t, env, err)
	}
	defer rows.Close()

	out := []controlplane.AddonInfo{}
	for rows.Next() {
		a, err := scanAddon(rows)
		if err != nil {
			return nil, fmt.Errorf("postgres: addons of type %q in environment %q: %w", t, env, err)
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("postgres: addons of type %q in environment %q: %w", t, env, err)
	}
	return out, nil
}

// Addon returns the add-on with the given name, or ErrNotFound. The returned info has Ready
// false — readiness is probed live, never read from the registry.
func (s *Store) Addon(ctx context.Context, name string) (controlplane.AddonInfo, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+addonColumns+` FROM addons WHERE name = $1`, name)
	a, err := scanAddon(row)
	if errors.Is(err, sql.ErrNoRows) {
		return controlplane.AddonInfo{}, fmt.Errorf("postgres: addon %q: %w", name, controlplane.ErrNotFound)
	}
	if err != nil {
		return controlplane.AddonInfo{}, fmt.Errorf("postgres: addon %q: %w", name, err)
	}
	return a, nil
}

// Addons returns all registered add-ons, name order. Each row has Ready false — readiness is a
// live property, probed by the caller, not stored.
func (s *Store) Addons(ctx context.Context) ([]controlplane.AddonInfo, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+addonColumns+` FROM addons ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("postgres: addons: %w", err)
	}
	defer rows.Close()

	out := []controlplane.AddonInfo{}
	for rows.Next() {
		a, err := scanAddon(rows)
		if err != nil {
			return nil, fmt.Errorf("postgres: addons: %w", err)
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("postgres: addons: %w", err)
	}
	return out, nil
}

// DeleteAddon removes the add-on row with the given name, or ErrNotFound if no such row exists.
func (s *Store) DeleteAddon(ctx context.Context, name string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM addons WHERE name = $1`, name)
	if err != nil {
		return fmt.Errorf("postgres: delete addon %q: %w", name, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("postgres: delete addon %q: %w", name, err)
	}
	if n == 0 {
		return fmt.Errorf("postgres: addon %q: %w", name, controlplane.ErrNotFound)
	}
	return nil
}

func scanAddon(sc scanner) (controlplane.AddonInfo, error) {
	var (
		a        controlplane.AddonInfo
		typ      string
		capsJSON []byte
	)
	if err := sc.Scan(&a.Name, &typ, &a.Label, &a.Environment, &a.Mode, &a.Backend, &a.Image, &a.Endpoint, &capsJSON, &a.SecretKey, &a.CreatedAt); err != nil {
		return controlplane.AddonInfo{}, err
	}
	a.Type = controlplane.AddonType(typ)
	if err := json.Unmarshal(capsJSON, &a.Capabilities); err != nil {
		return controlplane.AddonInfo{}, fmt.Errorf("decoding capabilities: %w", err)
	}
	return a, nil
}

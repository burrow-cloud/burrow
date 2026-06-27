// SPDX-License-Identifier: FSL-1.1-ALv2
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

const addonColumns = `name, type, mode, backend, image, endpoint, capabilities, secret_key, created_at`

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
	const q = `
INSERT INTO addons (name, type, mode, backend, image, endpoint, capabilities, secret_key, created_at)
VALUES ($1, $2, $3, $4, $5, $6, $7::jsonb, $8, $9)
ON CONFLICT (name) DO UPDATE SET
    type = EXCLUDED.type,
    mode = EXCLUDED.mode,
    backend = EXCLUDED.backend,
    image = EXCLUDED.image,
    endpoint = EXCLUDED.endpoint,
    capabilities = EXCLUDED.capabilities,
    secret_key = EXCLUDED.secret_key,
    created_at = EXCLUDED.created_at`
	if _, err := s.db.ExecContext(ctx, q, a.Name, string(a.Type), a.Mode, a.Backend, a.Image, a.Endpoint, string(capsJSON), a.SecretKey, a.CreatedAt); err != nil {
		return fmt.Errorf("postgres: save addon %s: %w", a.Name, err)
	}
	return nil
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
	if err := sc.Scan(&a.Name, &typ, &a.Mode, &a.Backend, &a.Image, &a.Endpoint, &capsJSON, &a.SecretKey, &a.CreatedAt); err != nil {
		return controlplane.AddonInfo{}, err
	}
	a.Type = controlplane.AddonType(typ)
	if err := json.Unmarshal(capsJSON, &a.Capabilities); err != nil {
		return controlplane.AddonInfo{}, fmt.Errorf("decoding capabilities: %w", err)
	}
	return a, nil
}

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

const providerColumns = `name, type, capabilities, secret_key, created_at`

// SaveProvider upserts a provider in the registry by name (ADR-0023). It records only the
// non-secret registry entry; the token lives in the burrow-credentials Secret.
func (s *Store) SaveProvider(ctx context.Context, p controlplane.Provider) error {
	if p.Name == "" {
		return fmt.Errorf("postgres: save provider: empty name")
	}
	caps := p.Capabilities
	if caps == nil {
		caps = []controlplane.Capability{}
	}
	capsJSON, err := json.Marshal(caps)
	if err != nil {
		return fmt.Errorf("postgres: save provider %s: encoding capabilities: %w", p.Name, err)
	}
	const q = `
INSERT INTO providers (name, type, capabilities, secret_key, created_at)
VALUES ($1, $2, $3::jsonb, $4, $5)
ON CONFLICT (name) DO UPDATE SET
    type = EXCLUDED.type,
    capabilities = EXCLUDED.capabilities,
    secret_key = EXCLUDED.secret_key,
    created_at = EXCLUDED.created_at`
	if _, err := s.db.ExecContext(ctx, q, p.Name, string(p.Type), string(capsJSON), p.SecretKey, p.CreatedAt); err != nil {
		return fmt.Errorf("postgres: save provider %s: %w", p.Name, err)
	}
	return nil
}

// Provider returns the provider with the given name, or ErrNotFound.
func (s *Store) Provider(ctx context.Context, name string) (controlplane.Provider, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+providerColumns+` FROM providers WHERE name = $1`, name)
	p, err := scanProvider(row)
	if errors.Is(err, sql.ErrNoRows) {
		return controlplane.Provider{}, fmt.Errorf("postgres: provider %q: %w", name, controlplane.ErrNotFound)
	}
	if err != nil {
		return controlplane.Provider{}, fmt.Errorf("postgres: provider %q: %w", name, err)
	}
	return p, nil
}

// Providers returns all configured providers, name order.
func (s *Store) Providers(ctx context.Context) ([]controlplane.Provider, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+providerColumns+` FROM providers ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("postgres: providers: %w", err)
	}
	defer rows.Close()

	out := []controlplane.Provider{}
	for rows.Next() {
		p, err := scanProvider(rows)
		if err != nil {
			return nil, fmt.Errorf("postgres: providers: %w", err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("postgres: providers: %w", err)
	}
	return out, nil
}

func scanProvider(sc scanner) (controlplane.Provider, error) {
	var (
		p        controlplane.Provider
		typ      string
		capsJSON []byte
	)
	if err := sc.Scan(&p.Name, &typ, &capsJSON, &p.SecretKey, &p.CreatedAt); err != nil {
		return controlplane.Provider{}, err
	}
	p.Type = controlplane.ProviderType(typ)
	if err := json.Unmarshal(capsJSON, &p.Capabilities); err != nil {
		return controlplane.Provider{}, fmt.Errorf("decoding capabilities: %w", err)
	}
	return p, nil
}

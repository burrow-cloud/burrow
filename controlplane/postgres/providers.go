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

const providerColumns = `name, type, capabilities, secret_key, created_at, ` +
	`endpoint, region, bucket, bucket_created, access_key_id_key, secret_access_key_key, retention_days`

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
	// The object-storage configuration is the non-secret half of an ADR-0063 registration: the
	// destination and the NAMES of the two burrow-credentials keys holding the credential pair.
	// A provider that serves no object storage stores empty values, which is what the columns
	// default to.
	var os controlplane.ObjectStoreConfig
	if p.ObjectStore != nil {
		os = *p.ObjectStore
	}
	const q = `
INSERT INTO providers (name, type, capabilities, secret_key, created_at,
                       endpoint, region, bucket, bucket_created, access_key_id_key,
                       secret_access_key_key, retention_days)
VALUES ($1, $2, $3::jsonb, $4, $5, $6, $7, $8, $9, $10, $11, $12)
ON CONFLICT (name) DO UPDATE SET
    type = EXCLUDED.type,
    capabilities = EXCLUDED.capabilities,
    secret_key = EXCLUDED.secret_key,
    created_at = EXCLUDED.created_at,
    endpoint = EXCLUDED.endpoint,
    region = EXCLUDED.region,
    bucket = EXCLUDED.bucket,
    bucket_created = EXCLUDED.bucket_created,
    access_key_id_key = EXCLUDED.access_key_id_key,
    secret_access_key_key = EXCLUDED.secret_access_key_key,
    retention_days = EXCLUDED.retention_days`
	if _, err := s.db.ExecContext(ctx, q, p.Name, string(p.Type), string(capsJSON), p.SecretKey, p.CreatedAt,
		os.Endpoint, os.Region, os.Bucket, os.Created, os.AccessKeyIDKey, os.SecretAccessKeyKey, os.RetentionDays); err != nil {
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
		os       controlplane.ObjectStoreConfig
	)
	if err := sc.Scan(&p.Name, &typ, &capsJSON, &p.SecretKey, &p.CreatedAt,
		&os.Endpoint, &os.Region, &os.Bucket, &os.Created, &os.AccessKeyIDKey,
		&os.SecretAccessKeyKey, &os.RetentionDays); err != nil {
		return controlplane.Provider{}, err
	}
	p.Type = controlplane.ProviderType(typ)
	if err := json.Unmarshal(capsJSON, &p.Capabilities); err != nil {
		return controlplane.Provider{}, fmt.Errorf("decoding capabilities: %w", err)
	}
	// An endpoint is what distinguishes a recorded destination from the empty columns every other
	// provider type leaves behind, so the pointer is set only when there is one.
	if os.Endpoint != "" {
		p.ObjectStore = &os
	}
	return p, nil
}

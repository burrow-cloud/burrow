// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/burrow-cloud/burrow/controlplane"
)

const backupColumns = `id, app, created_at, path, size_bytes, status`

// RecordBackup persists a new backup row (ADR-0032). burrowd records it pending before starting the
// backup Job, then SetBackupStatus moves it to completed/failed when the Job finishes. An existing
// row with the same id is overwritten. The row names the app, the on-PVC path, and the status —
// never a credential.
func (s *Store) RecordBackup(ctx context.Context, b controlplane.Backup) error {
	if b.ID == "" {
		return fmt.Errorf("postgres: record backup: empty ID")
	}
	const q = `
INSERT INTO postgres_backups (id, app, created_at, path, size_bytes, status)
VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (id) DO UPDATE SET
    app = EXCLUDED.app, created_at = EXCLUDED.created_at, path = EXCLUDED.path,
    size_bytes = EXCLUDED.size_bytes, status = EXCLUDED.status`
	if _, err := s.db.ExecContext(ctx, q, b.ID, b.App, b.CreatedAt, b.Path, b.SizeBytes, string(b.Status)); err != nil {
		return fmt.Errorf("postgres: record backup %s: %w", b.ID, err)
	}
	return nil
}

// SetBackupStatus updates a recorded backup's status and size (the Job-finished transition). Setting
// the status of an unknown backup id returns ErrNotFound.
func (s *Store) SetBackupStatus(ctx context.Context, id string, status controlplane.BackupStatus, sizeBytes int64) error {
	const q = `UPDATE postgres_backups SET status = $2, size_bytes = $3 WHERE id = $1`
	res, err := s.db.ExecContext(ctx, q, id, string(status), sizeBytes)
	if err != nil {
		return fmt.Errorf("postgres: set backup status %s: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("postgres: set backup status %s: %w", id, err)
	}
	if n == 0 {
		return fmt.Errorf("postgres: backup %q: %w", id, controlplane.ErrNotFound)
	}
	return nil
}

// ListBackups returns recorded backups, newest first. An empty app lists every app's backups; a
// non-empty app restricts to that app. No matches yields an empty slice and no error.
func (s *Store) ListBackups(ctx context.Context, app string) ([]controlplane.Backup, error) {
	q := `SELECT ` + backupColumns + ` FROM postgres_backups`
	var args []any
	if app != "" {
		q += ` WHERE app = $1`
		args = append(args, app)
	}
	q += ` ORDER BY created_at DESC, id DESC`
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("postgres: list backups: %w", err)
	}
	defer rows.Close()

	out := make([]controlplane.Backup, 0)
	for rows.Next() {
		b, err := scanBackup(rows)
		if err != nil {
			return nil, fmt.Errorf("postgres: list backups: %w", err)
		}
		out = append(out, b)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("postgres: list backups: %w", err)
	}
	return out, nil
}

// GetBackup returns the backup with the given id, or ErrNotFound.
func (s *Store) GetBackup(ctx context.Context, id string) (controlplane.Backup, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+backupColumns+` FROM postgres_backups WHERE id = $1`, id)
	b, err := scanBackup(row)
	if errors.Is(err, sql.ErrNoRows) {
		return controlplane.Backup{}, fmt.Errorf("postgres: backup %q: %w", id, controlplane.ErrNotFound)
	}
	if err != nil {
		return controlplane.Backup{}, fmt.Errorf("postgres: backup %q: %w", id, err)
	}
	return b, nil
}

func scanBackup(sc scanner) (controlplane.Backup, error) {
	var (
		b       controlplane.Backup
		created time.Time
		status  string
	)
	if err := sc.Scan(&b.ID, &b.App, &created, &b.Path, &b.SizeBytes, &status); err != nil {
		return controlplane.Backup{}, err
	}
	b.CreatedAt = created
	b.Status = controlplane.BackupStatus(status)
	return b, nil
}

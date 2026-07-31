// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/burrow-cloud/burrow/controlplane"
)

const backupColumns = `id, app, environment, created_at, path, volume, size_bytes, status, ` +
	`destination, provider, object_key, failure_reason, failure_detail`

// RecordBackup persists a new backup row (ADR-0032). burrowd records it pending before starting the
// backup Job, then SetBackupStatus moves it to completed or FailBackup to failed when the Job
// finishes. An existing row with the same id is overwritten. The row names the app, the claim and
// the path within it, the destination it is being written to, and the status — never a credential.
func (s *Store) RecordBackup(ctx context.Context, b controlplane.Backup) error {
	if b.ID == "" {
		return fmt.Errorf("postgres: record backup: empty ID")
	}
	const q = `
INSERT INTO postgres_backups (id, app, environment, created_at, path, volume, size_bytes, status,
                              destination, provider, object_key, failure_reason, failure_detail)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
ON CONFLICT (id) DO UPDATE SET
    app = EXCLUDED.app, environment = EXCLUDED.environment, created_at = EXCLUDED.created_at,
    path = EXCLUDED.path, volume = EXCLUDED.volume, size_bytes = EXCLUDED.size_bytes, status = EXCLUDED.status,
    destination = EXCLUDED.destination, provider = EXCLUDED.provider, object_key = EXCLUDED.object_key,
    failure_reason = EXCLUDED.failure_reason, failure_detail = EXCLUDED.failure_detail`
	env := b.Environment
	if env == "" {
		env = controlplane.DefaultEnvironment
	}
	vol := b.Volume
	if vol == "" {
		// A row arriving with no claim named can only mean the shared claim that predates
		// per-environment backups; writing it blank would leave the restore unable to say which
		// volume the dump is on (ADR-0067 §1).
		vol = controlplane.PostgresBackupVolume
	}
	dest := b.Destination
	if dest == "" {
		// A row with no destination named reached the cluster and nowhere else; recording it as blank
		// would leave the listing unable to say which backups survive losing the cluster.
		dest = controlplane.BackupDestinationCluster
	}
	if _, err := s.db.ExecContext(ctx, q, b.ID, b.App, env, b.CreatedAt, b.Path, vol, b.SizeBytes, string(b.Status),
		string(dest), b.Provider, b.ObjectKey, b.FailureReason, b.FailureDetail); err != nil {
		return fmt.Errorf("postgres: record backup %s: %w", b.ID, err)
	}
	return nil
}

// SetBackupStatus updates a recorded backup's status and size (the Job-finished transition). It
// CLEARS any recorded failure, so a row that reaches completed cannot keep a stale reason from an
// earlier attempt beside it. Setting the status of an unknown backup id returns ErrNotFound.
func (s *Store) SetBackupStatus(ctx context.Context, id string, status controlplane.BackupStatus, sizeBytes int64) error {
	const q = `UPDATE postgres_backups SET status = $2, size_bytes = $3, failure_reason = '', failure_detail = '' WHERE id = $1`
	res, err := s.db.ExecContext(ctx, q, id, string(status), sizeBytes)
	if err != nil {
		return fmt.Errorf("postgres: set backup status %s: %w", id, err)
	}
	return affectedOne(res, id, "set backup status")
}

// FailBackup marks a recorded backup failed and records the closed reason and the Burrow-authored
// detail beside it (ADR-0063 §7). The size is zeroed: a failed backup has no bytes anybody can
// restore, and leaving a length on the row would make it read like a partial success.
//
// Neither reason nor detail may carry a secret value or a vendor response body — the caller's
// contract, restated here because this is the write that makes them durable.
func (s *Store) FailBackup(ctx context.Context, id, reason, detail string) error {
	const q = `UPDATE postgres_backups SET status = $2, size_bytes = 0, failure_reason = $3, failure_detail = $4 WHERE id = $1`
	res, err := s.db.ExecContext(ctx, q, id, string(controlplane.BackupFailed), reason, detail)
	if err != nil {
		return fmt.Errorf("postgres: fail backup %s: %w", id, err)
	}
	return affectedOne(res, id, "fail backup")
}

// affectedOne turns "no row was updated" into ErrNotFound for the backup writes, so an unknown id is
// reported as absent rather than as a silent no-op.
func affectedOne(res sql.Result, id, op string) error {
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("postgres: %s %s: %w", op, id, err)
	}
	if n == 0 {
		return fmt.Errorf("postgres: backup %q: %w", id, controlplane.ErrNotFound)
	}
	return nil
}

// ListBackups returns recorded backups, newest first. An empty app lists every app's backups and an
// empty env every environment's; a non-empty value restricts to that app or environment (ADR-0067
// §1). No matches yields an empty slice and no error.
func (s *Store) ListBackups(ctx context.Context, app, env string) ([]controlplane.Backup, error) {
	q := `SELECT ` + backupColumns + ` FROM postgres_backups`
	var (
		args   []any
		wheres []string
	)
	if app != "" {
		args = append(args, app)
		wheres = append(wheres, fmt.Sprintf("app = $%d", len(args)))
	}
	if env != "" {
		args = append(args, env)
		wheres = append(wheres, fmt.Sprintf("environment = $%d", len(args)))
	}
	if len(wheres) > 0 {
		q += ` WHERE ` + strings.Join(wheres, " AND ")
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
		dest    string
	)
	if err := sc.Scan(&b.ID, &b.App, &b.Environment, &created, &b.Path, &b.Volume, &b.SizeBytes, &status,
		&dest, &b.Provider, &b.ObjectKey, &b.FailureReason, &b.FailureDetail); err != nil {
		return controlplane.Backup{}, err
	}
	b.CreatedAt = created
	b.Status = controlplane.BackupStatus(status)
	b.Destination = controlplane.BackupDestinationKind(dest)
	return b, nil
}

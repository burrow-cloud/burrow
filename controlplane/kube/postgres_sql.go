// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package kube

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/burrow-cloud/burrow/controlplane"
)

var _ controlplane.DatabaseQuerier = (*PostgresProvisioner)(nil)

// sqlApplicationName is what an `addon sql` connection calls itself, so a statement of Burrow's
// shows up as Burrow's in `pg_stat_activity` rather than as another connection from the app.
const sqlApplicationName = "burrow-addon-sql"

// QueryAppDatabase runs one statement against app's database on environment env's instance and
// returns its columns and rows (ADR-0087). It is the production controlplane.DatabaseQuerier.
//
// IT CONNECTS AS THE APP'S OWN ROLE, NOT AS THE SUPERUSER, and everything about the shape of this
// method follows from that. It reads the connection string burrowd wrote into the app's Secret at
// attach — the credential it already minted, so no new secret and no new grant — and dials with it.
// The role is `app_<app>`, which holds CONNECT on its own database and on no other (EnsureAppDatabase
// revokes CONNECT from PUBLIC), so what the statement may touch is what the application itself may
// touch. The DATABASE IS CHOSEN BY THE CREDENTIAL rather than by the caller: there is no argument
// that names one, so no form of this call reaches the instance, `template1`, or another app's
// database (ADR-0087 §1).
//
// Note what is deliberately NOT here: no `SET ROLE` and no `SET SESSION AUTHORIZATION` off a
// superuser connection. Both look equivalent and are not — a session that authenticated as a
// superuser can `RESET` its way back, so the statement would be one line away from the whole
// instance.
//
// The credential is read here and spent here. It is never returned, logged, wrapped into an error,
// or handed back across the seam; the caller names the Secret's KEY and never sees its value
// (ADR-0029/0031).
//
// Three bounds, from ADR-0087 §7. ONE connection, closed when the statement finishes. A STATEMENT
// TIMEOUT, applied as the connection's own `statement_timeout` so Postgres enforces it rather than a
// client that could decide not to wait. And a ROW CAP, at which the connection is CLOSED rather than
// drained — which is what makes the cap a bound on the work the instance does and not merely on the
// size of the answer.
func (p *PostgresProvisioner) QueryAppDatabase(ctx context.Context, q controlplane.AppStatement) (controlplane.SQLResult, error) {
	if err := validateAppIdentifier(q.App); err != nil {
		return controlplane.SQLResult{}, err
	}
	// The environment is resolved to an instance BEFORE anything else, exactly as it is on every
	// other method here: an empty or malformed environment is ErrInvalid, never a silent fallback to
	// whichever instance exists (ADR-0067 §1).
	if _, err := p.targetFor(q.Env, q.Instance); err != nil {
		return controlplane.SQLResult{}, err
	}
	if q.Namespace == "" {
		return controlplane.SQLResult{}, fmt.Errorf("kube: running a statement for %s needs the namespace its Secret is in: %w", q.App, controlplane.ErrInvalid)
	}
	if q.Statement == "" {
		return controlplane.SQLResult{}, fmt.Errorf("kube: running a statement for %s: the statement is empty: %w", q.App, controlplane.ErrInvalid)
	}
	// The bounds are REQUIRED, not defaulted, and the timeout is the reason to be strict about it:
	// Postgres reads `statement_timeout = 0` as "no timeout at all", so an unset bound arriving here
	// would silently be the opposite of a bound. A caller that has not decided is refused rather than
	// given the permissive reading (ADR-0087 §7).
	if q.Timeout <= 0 {
		return controlplane.SQLResult{}, fmt.Errorf("kube: running a statement for %s needs a statement timeout; 0 would mean no timeout at all: %w", q.App, controlplane.ErrInvalid)
	}
	if q.MaxRows < 1 {
		return controlplane.SQLResult{}, fmt.Errorf("kube: running a statement for %s needs a row cap of at least 1: %w", q.App, controlplane.ErrInvalid)
	}

	dsn, err := p.appDatabaseURL(ctx, q)
	if err != nil {
		return controlplane.SQLResult{}, err
	}

	cfg, err := sqlConnConfig(dsn, q.Timeout)
	if err != nil {
		// The DSN is a secret value, so the error says which key held it and never what it said.
		return controlplane.SQLResult{}, fmt.Errorf("kube: the connection string %s's Secret holds under %q is not a valid one: %w", q.App, q.SecretKey, controlplane.ErrInvalid)
	}

	connectCtx, cancelConnect := context.WithTimeout(ctx, controlplane.AddonSQLConnectTimeout)
	defer cancelConnect()
	conn, err := pgconn.ConnectConfig(connectCtx, cfg)
	if err != nil {
		return controlplane.SQLResult{}, fmt.Errorf("kube: connecting to %s's database on environment %q's postgres instance as its own role: %w", q.App, q.Env, err)
	}
	// The one connection, closed however this returns — including at the row cap, where the reader is
	// abandoned mid-result on purpose. Close gets a context of its own because ctx may already be
	// past its deadline by the time we get here, and a connection nobody terminates is one the
	// instance keeps.
	defer func() {
		closeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_ = conn.Close(closeCtx)
	}()

	// The statement's own budget, on top of the server-side statement_timeout: the two bound
	// different failures — the server aborting a long query, and a server that stops answering.
	runCtx, cancelRun := context.WithTimeout(ctx, q.Timeout+controlplane.AddonSQLConnectTimeout)
	defer cancelRun()

	// ExecParams, not Exec, and the difference is a bound rather than a style. Exec uses the simple
	// protocol, which accepts a semicolon-separated SCRIPT — several statements, of which only the
	// last would be reported. ExecParams is the extended protocol and takes exactly ONE statement, so
	// `addon sql` runs the statement it audited and a second one smuggled in behind a semicolon is a
	// syntax error from the server rather than a silent extra command.
	rr := conn.ExecParams(runCtx, q.Statement, nil, nil, nil, nil)

	var res controlplane.SQLResult
	res.Rows = [][]*string{}
	for rr.NextRow() {
		if len(res.Rows) >= q.MaxRows {
			// One row past the cap exists, so the result is short and the caller is told so. The
			// connection is closed by the deferred Close rather than drained: reading rows nobody
			// asked for to reach a command tag nobody reads would spend the instance's time on the
			// output we already decided not to return.
			res.Truncated = true
			res.RowCount = len(res.Rows)
			res.Columns = fieldNames(rr.FieldDescriptions())
			return res, nil
		}
		res.Rows = append(res.Rows, copyRow(rr.Values()))
	}
	tag, err := rr.Close()
	res.Columns = fieldNames(rr.FieldDescriptions())
	res.RowCount = len(res.Rows)
	if err != nil {
		// A statement the database refused is an OUTCOME, not a failure of this call: it comes back
		// with Postgres's own message and SQLSTATE, unmodified (ADR-0087 §4). Anything that is not a
		// server error — the connection dropping, the context expiring — is a real error.
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) {
			res.Error = &controlplane.SQLError{
				Message:  pgErr.Message,
				SQLState: pgErr.Code,
				Detail:   pgErr.Detail,
				Hint:     pgErr.Hint,
				Position: pgErr.Position,
			}
			return res, nil
		}
		return controlplane.SQLResult{}, fmt.Errorf("kube: running a statement against %s's database on environment %q's postgres instance: %w", q.App, q.Env, err)
	}
	res.Command = tag.String()
	res.RowsAffected = tag.RowsAffected()
	return res, nil
}

// sqlConnConfig turns the app's own connection string into the configuration one statement runs on:
// the role, host and database come from the DSN — so the CREDENTIAL decides which database is
// reached, not the caller — and the bounds are added as connection parameters.
//
// statement_timeout is a connection parameter rather than SQL Burrow prepends, which is the
// difference between a bound and a suggestion: it is in force before the caller's statement is read,
// so there is no leading `SET` for anything to sit in front of and no statement to concatenate with.
func sqlConnConfig(dsn string, timeout time.Duration) (*pgconn.Config, error) {
	cfg, err := pgconn.ParseConfig(dsn)
	if err != nil {
		return nil, err
	}
	if cfg.RuntimeParams == nil {
		cfg.RuntimeParams = map[string]string{}
	}
	cfg.RuntimeParams["statement_timeout"] = strconv.FormatInt(timeout.Milliseconds(), 10)
	cfg.RuntimeParams["application_name"] = sqlApplicationName
	return cfg, nil
}

// appDatabaseURL reads the connection string the attach wrote into app's per-app Secret, under the
// key the attachment recorded. The value is returned to exactly one caller, which spends it opening
// one connection; it appears in no error and no log.
//
// An absent Secret or an absent key is an app that is not attached, and the error says how to attach
// it — the alternative is a connection refusal that reads like the instance being down.
func (p *PostgresProvisioner) appDatabaseURL(ctx context.Context, q controlplane.AppStatement) (string, error) {
	name := controlplane.AppSecretName(q.App)
	s, err := p.client.CoreV1().Secrets(q.Namespace).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return "", fmt.Errorf("kube: %s has no database on environment %q's postgres instance — attach it with `burrow addon attach postgres %s --env %s`: %w", q.App, q.Env, q.App, q.Env, controlplane.ErrNotFound)
	}
	if err != nil {
		return "", fmt.Errorf("kube: reading %s's secret in %s: %w", q.App, q.Namespace, err)
	}
	v, ok := s.Data[q.SecretKey]
	if !ok || len(v) == 0 {
		return "", fmt.Errorf("kube: %s's secret holds no connection string under %q — attach it with `burrow addon attach postgres %s --env %s`: %w", q.App, q.SecretKey, q.App, q.Env, controlplane.ErrNotFound)
	}
	return string(v), nil
}

// fieldNames is the result's column names in order. A statement that returned no row set — an
// UPDATE, a DDL statement — described no fields, and yields an empty slice rather than nil so the
// JSON carries `[]` and a caller never has to tell an absent list from an empty one.
func fieldNames(fds []pgconn.FieldDescription) []string {
	out := make([]string, len(fds))
	for i, fd := range fds {
		out[i] = fd.Name
	}
	return out
}

// copyRow copies one row's values out of the reader's buffer, which pgconn reuses for the next row.
// A NULL arrives as a nil []byte and stays nil, so it survives into the result as JSON null rather
// than becoming an empty string.
func copyRow(values [][]byte) []*string {
	row := make([]*string, len(values))
	for i, v := range values {
		if v == nil {
			continue
		}
		s := string(v)
		row[i] = &s
	}
	return row
}

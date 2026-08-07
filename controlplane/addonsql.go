// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package controlplane

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// SQLRequest is one statement to run against an app's database on a relational add-on (ADR-0087 §1).
// The add-on and the app together name the database — the same pair `addon attach` and
// `addon detach` already take.
//
// BOTH ARE LOAD-BEARING, which is worth stating because one of them looks redundant. `Addon` names a
// TYPE (`postgres`), not an instance and not a database: `addon install postgres` provisions one
// instance per environment and `addon attach postgres web` creates a database and a role per app on
// it, so an instance holds as many databases as it has attached apps. Dropping `App` would leave the
// command to pick a database itself — which means connecting as a superuser and letting the
// statement choose, the one shape ADR-0087 §1 rules out.
type SQLRequest struct {
	// Addon is the add-on TYPE the statement is aimed at. Only a relational type takes one
	// (ADR-0087 §2).
	Addon AddonType `json:"addon"`
	// App names the database on that type's instance, and the role the statement runs as.
	App string `json:"app"`
	// Env is the environment whose instance holds it (ADR-0067 §1): empty targets the default
	// environment, and is refused when more than one environment is registered.
	Env string `json:"env,omitempty"`
	// Statement is the caller's SQL, verbatim. Burrow does not parse it, does not branch on its
	// leading keyword, and does not report it as a read or a write (ADR-0087 §6).
	Statement string `json:"statement"`
	// Confirm acknowledges an addon.sql guardrail an operator has set to confirm, letting the
	// statement proceed past it (ADR-0020). The default disposition is DENY, which no confirmation
	// opens.
	Confirm bool `json:"confirm,omitempty"`
}

// SQLResult is what a statement produced: columns and rows an agent can compose on, rather than a
// rendering of them (ADR-0087 §4). The CLI draws a table from it; `--json` hands back the rows.
//
// A statement that returned no row set — an `UPDATE`, a `CREATE TABLE` — carries its command tag and
// the number of rows it affected instead, so "it worked, and it changed three rows" is expressible
// without an empty table standing in for it.
type SQLResult struct {
	// Addon, App and Environment name the database the statement ran against, so a result read on its
	// own says which one it is about.
	Addon       string `json:"addon"`
	App         string `json:"app"`
	Environment string `json:"environment"`
	// Columns are the result's column names in order, empty for a statement that returned no row set.
	Columns []string `json:"columns"`
	// Rows are the result's rows, each aligned with Columns. A value is rendered in Postgres's own
	// text form; a NULL is a nil entry, and marshals as JSON null — the one distinction a
	// string-per-cell shape would otherwise lose, and the one an agent branching on "is it set" needs.
	Rows [][]*string `json:"rows"`
	// RowCount is how many rows Rows holds. It is the length of what was RETURNED, which is not the
	// size of the result when Truncated is true.
	RowCount int `json:"row_count"`
	// Truncated reports that the result had more rows than the cap and the rest were not read. It is
	// reported rather than silent (ADR-0087 §7): a short answer nobody was told about is worse than a
	// refusal.
	Truncated bool `json:"truncated"`
	// RowLimit is the cap in force, so a truncated result says what to raise
	// (`burrow cluster config set --env <env> addon.sql_rows <n>`) rather than only that it was cut.
	RowLimit int `json:"row_limit"`
	// Command is Postgres's own command tag for the statement ("SELECT 3", "UPDATE 2", "CREATE
	// TABLE"), empty for a truncated read, where the tag never arrived because the connection was
	// closed at the cap rather than drained.
	Command string `json:"command,omitempty"`
	// RowsAffected is the row count the command tag carried — what an `INSERT`, `UPDATE` or `DELETE`
	// changed.
	RowsAffected int64 `json:"rows_affected"`
	// Error is the database's own refusal of the statement, when it raised one. A database error is
	// an OUTCOME, not a CLI failure: the call succeeded, the statement did not, and the caller reads
	// which from here (ADR-0087 §4).
	Error *SQLError `json:"error,omitempty"`
}

// SQLError is a Postgres error, unmodified (ADR-0087 §4). The SQLSTATE is what makes it something an
// agent can branch on — `42P01` is a missing table whatever the server's locale renders the message
// in — so it is carried alongside the prose rather than in place of it.
type SQLError struct {
	Message  string `json:"message"`
	SQLState string `json:"sqlstate,omitempty"`
	Detail   string `json:"detail,omitempty"`
	Hint     string `json:"hint,omitempty"`
	// Position is the one-based character offset in the statement the server pointed at, 0 when it
	// pointed at nothing.
	Position int32 `json:"position,omitempty"`
}

// addonQueryVerb names the verb an add-on type's own query surface is spelled with, for a refusal
// that teaches the shape instead of merely reporting a mismatch (ADR-0087 §2).
//
// QUERY SURFACES ARE PER-ADD-ON-TYPE, and that is the point the refusal has to carry. `addon logs`
// speaks LogsQL, `addon metrics` speaks PromQL, and `sql` is the relational member of that set
// rather than a general facility that happens to work on one type. A future relational add-on gains
// `sql` by being relational.
func addonQueryVerb(t AddonType) string {
	switch t {
	case AddonLogs:
		return "`burrow addon logs [query]` (burrow-agent: `logs-query`), which speaks LogsQL"
	case AddonMetrics:
		return "`burrow addon metrics <query>` (burrow-agent: `metrics-query`), which speaks PromQL"
	case AddonCache:
		// ValKey is a backing service the app connects to, not one the agent queries, so it has no
		// query seam at all — and saying that is more use than naming a verb that does not exist.
		return "no query verb at all — it is a cache the app connects to directly, wired by reading its endpoint from `burrow addon list`"
	default:
		return ""
	}
}

// AddonSQL runs one caller-supplied statement against app's database on the named environment's
// relational add-on instance and returns columns and rows (ADR-0087).
//
// burrowd runs it, with the credential it already minted for that app: it opens ONE connection to
// the instance as the app's own role — never a superuser — runs the statement, and closes it. Two
// things follow, and both are the point. No connection to the database ever leaves the cluster,
// so no path turns an operator's kubeconfig into a credential for tenant data (ADR-0087 §3). And
// the statement runs INDEPENDENTLY of the application, so a database whose app is crash-looping is
// still queryable — which is the case `app run` with `psql` cannot serve, and often the state
// somebody most wants to query it in.
//
// It is gated by the addon.sql guardrail, DENIED by default (ADR-0087 §5). The guardrail gates
// whether the statement runs, not what it does: there is one code, Burrow does not parse the
// statement, and it does not report it as a read or a write (§6). A `--read-only` mode is the one
// credible path to a softer default — Postgres refuses a write in a `READ ONLY` transaction, so the
// enforcement would belong to the engine rather than to us — and ADR-0087 §6 defers it deliberately;
// it is not this function's to anticipate.
//
// Every run — held, denied, or executed — is recorded in the audit log WITH THE STATEMENT TEXT
// (ADR-0027). That is what makes the capability accountable, and it means a literal in a `WHERE`
// clause is written to the audit table. ADR-0087 states this as a cost rather than mitigating it:
// redacting literals means parsing the statement, which §6 refuses to do.
func (e *Engine) AddonSQL(ctx context.Context, req SQLRequest) (SQLResult, error) {
	if err := (App{Name: req.App}).Validate(); err != nil {
		return SQLResult{}, fmt.Errorf("addon sql: %w: %w", ErrInvalid, err)
	}
	// A non-relational add-on is refused by naming the verb it DOES take, before anything else is
	// resolved: the caller reached for the wrong query surface, and which one is right does not
	// depend on the environment or on whether anything is installed.
	if req.Addon != AddonPostgres {
		if verb := addonQueryVerb(req.Addon); verb != "" {
			return SQLResult{}, fmt.Errorf("addon sql %s: the %s add-on takes no statement — it is queried with %s. `sql` is the relational add-on's query verb, and today that is postgres: %w",
				req.Addon, req.Addon, verb, ErrInvalid)
		}
		return SQLResult{}, fmt.Errorf("addon sql %s: %q is not an add-on Burrow installs; `sql` is the relational add-on's query verb, and today that is postgres: %w",
			req.Addon, req.Addon, ErrInvalid)
	}
	statement := strings.TrimSpace(req.Statement)
	if statement == "" {
		return SQLResult{}, fmt.Errorf("addon sql %s for %s: a statement is required: %w", req.Addon, req.App, ErrInvalid)
	}
	targetEnv, ns, err := e.resolveMutatingEnvironment(ctx, req.Env)
	if err != nil {
		return SQLResult{}, fmt.Errorf("addon sql %s for %s: %w", req.Addon, req.App, err)
	}
	// An optional seam: a nil provisioner fails the assertion too, so one check covers both "no
	// Postgres path is wired" and "the one that is does not run statements".
	querier, ok := e.dbProvisioner.(DatabaseQuerier)
	if !ok {
		return SQLResult{}, fmt.Errorf("addon sql %s for %s: this control plane cannot run statements against an add-on database: %w", req.Addon, req.App, ErrNotImplemented)
	}
	instance, err := AddonInstanceName(req.Addon, targetEnv)
	if err != nil {
		return SQLResult{}, fmt.Errorf("addon sql %s for %s: %w", req.Addon, req.App, err)
	}
	// The add-on has to be installed in this environment before the guardrail is evaluated, so a
	// typo'd environment reads as ErrNotFound rather than as a refusal the caller might try to get
	// relaxed (mirrors RestoreAddon resolving the backup first).
	if _, err := e.db.Addon(ctx, instance); err != nil {
		if errors.Is(err, ErrNotFound) {
			return SQLResult{}, fmt.Errorf("addon sql %s for %s: no %s add-on is installed in environment %s (install one with `burrow addon install %s --env %s`): %w",
				req.Addon, req.App, req.Addon, targetEnv, req.Addon, targetEnv, ErrNotFound)
		}
		return SQLResult{}, fmt.Errorf("addon sql %s for %s: reading the %s add-on: %w", req.Addon, req.App, instance, err)
	}
	// The variable the attachment was written under. WHETHER a database is attached is derived from
	// the instance; what the variable is CALLED is recorded, because no derivation can produce a name
	// somebody chose (issue #462), and a statement has to follow the same name detach, rotation and a
	// restore's cutover follow.
	key, err := e.db.AddonEnvKey(ctx, string(req.Addon), req.App, targetEnv)
	if err != nil {
		return SQLResult{}, fmt.Errorf("addon sql %s for %s: reading the attachment's variable name: %w", req.Addon, req.App, err)
	}

	pol, err := e.db.Policy(ctx)
	if err != nil {
		return SQLResult{}, fmt.Errorf("addon sql %s for %s: loading guardrail policy: %w", req.Addon, req.App, err)
	}
	// The audit args carry the STATEMENT (ADR-0087's stated cost) and never a credential: the
	// connection string is read inside the adapter and never reaches here at all.
	args := map[string]string{"addon": string(req.Addon), "app": req.App, "env": targetEnv, "statement": statement}
	if err := e.recordDecision(ctx, auditOpAddonSQL, req.App, args, GuardrailAddonSQL,
		// Scoped by the INSTANCE like every other addon.* code, and by the ENVIRONMENT as well, which
		// this one alone declares — ADR-0087 §5 asks for the gradient (allow in development, deny in
		// production) that the environment tier is what expresses.
		pol.evaluateGuardrail(GuardrailScope{Env: targetEnv, Name: instance}, "addon sql", GuardrailAddonSQL, req.Confirm,
			fmt.Sprintf("running a statement against %q's database in environment %s", req.App, targetEnv))); err != nil {
		return SQLResult{}, err
	}

	// The bounds. They are operational limits, not guardrails: there is no disposition on them and
	// `guard set` does not reach them (ADR-0068 §2, ADR-0087 §7).
	cfg, err := e.db.OperationalConfig(ctx)
	if err != nil {
		return SQLResult{}, fmt.Errorf("addon sql %s for %s: reading operational configuration: %w", req.Addon, req.App, err)
	}
	timeout, _ := cfg.Duration(targetEnv, LimitAddonSQLTimeout)
	maxRows, _ := cfg.Count(targetEnv, LimitAddonSQLRows)

	res, err := querier.QueryAppDatabase(ctx, AppStatement{
		App:       req.App,
		Env:       targetEnv,
		Namespace: ns,
		SecretKey: key,
		Statement: statement,
		Timeout:   timeout,
		MaxRows:   int(maxRows),
	})
	if err != nil {
		e.recordExecution(ctx, auditOpAddonSQL, req.App, args, err)
		return SQLResult{}, fmt.Errorf("addon sql %s for %s: %w", req.Addon, req.App, err)
	}
	res.Addon, res.App, res.Environment = string(req.Addon), req.App, targetEnv
	res.RowLimit = int(maxRows)
	// A statement the database refused RAN — the connection opened, the server read it, and the
	// server answered. The execution row records that, with the SQLSTATE so the trail says which
	// statements the database rejected without anyone re-deriving it from the text.
	if res.Error != nil && res.Error.SQLState != "" {
		args["sqlstate"] = res.Error.SQLState
	}
	e.recordExecution(ctx, auditOpAddonSQL, req.App, args, nil)
	return res, nil
}

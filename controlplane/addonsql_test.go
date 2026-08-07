// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package controlplane_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	cp "github.com/burrow-cloud/burrow/controlplane"
	"github.com/burrow-cloud/burrow/controlplane/internal/fake"
)

// sqlEngine builds an engine with the Postgres add-on installed in the default environment and `web`
// attached to it — the state `addon sql` is defined against — and returns the seams to arrange and
// inspect. The policy is the DEFAULT one, so addon.sql reads as its built-in disposition rather than
// as whatever a permissive fixture set (that is the subject of the first test below).
func sqlEngine(t *testing.T) (*cp.Engine, *fake.Database, *fake.Provisioner) {
	t.Helper()
	e, _, d, prov := newPostgresEngine(t)
	ctx := context.Background()
	instance, err := cp.AddonInstanceName(cp.AddonPostgres, cp.DefaultEnvironment)
	if err != nil {
		t.Fatalf("AddonInstanceName: %v", err)
	}
	if err := d.SaveAddon(ctx, cp.AddonInfo{
		Name: instance, Type: cp.AddonPostgres, Environment: cp.DefaultEnvironment, Mode: "installed",
	}); err != nil {
		t.Fatalf("seed add-on: %v", err)
	}
	if _, err := e.AttachAddon(ctx, cp.AddonPostgres, "web", "", ""); err != nil {
		t.Fatalf("AttachAddon: %v", err)
	}
	return e, d, prov
}

// allowSQL opens the addon.sql guardrail on d's policy, which every test past the deny one needs
// because the built-in default is deny.
func allowSQL(t *testing.T, d *fake.Database) {
	t.Helper()
	d.SetPolicy(permissive().With(cp.GuardrailAddonSQL, cp.DispositionAllow))
}

// TestAddonSQLDeniedByDefault pins ADR-0087 §5's disposition. The default is DENY rather than
// confirm, because there is no upper bound on what a statement does and a human reading a
// hundred-line statement is not meaningfully approving it.
//
// It also pins the SHAPE of the refusal, which is the half that matters to an agent: a guardrail
// error, not a transport failure, carrying the code so the agent branches on the cause rather than
// on prose — and naming the per-environment command that would open it, so the agent relays
// something the human can act on rather than reaching for a shell.
func TestAddonSQLDeniedByDefault(t *testing.T) {
	ctx := context.Background()
	e, _, prov := sqlEngine(t)

	_, err := e.AddonSQL(ctx, cp.SQLRequest{Addon: cp.AddonPostgres, App: "web", Statement: "select 1"})
	g, ok := cp.AsGuardrail(err)
	if !ok {
		t.Fatalf("AddonSQL with the default policy = %v, want a guardrail refusal an agent can read", err)
	}
	if g.Code != cp.GuardrailAddonSQL {
		t.Errorf("code = %q, want %q", g.Code, cp.GuardrailAddonSQL)
	}
	if g.NeedsConfirmation {
		t.Error("the default disposition holds for confirmation; ADR-0087 §5 says deny, because a confirmation on a statement cannot be an informed one")
	}
	if !strings.Contains(g.Message, "guard set") || !strings.Contains(g.Message, string(cp.GuardrailAddonSQL)) {
		t.Errorf("refusal %q does not name the operator command that would relax it", g.Message)
	}
	// Nothing ran. A denied statement is one the database never saw.
	if got := prov.Statements(); len(got) != 0 {
		t.Errorf("a denied statement reached the database: %+v", got)
	}
}

// TestAddonSQLGuardrailIsEnvScopable pins the gradient ADR-0087 §5 asks for: allow in development,
// where the database is disposable and the agent inspecting it is the whole value, and deny in
// production. It is the one addon.* code that carries an environment tier, so this asserts the tier
// is actually consulted rather than merely declared.
func TestAddonSQLGuardrailIsEnvScopable(t *testing.T) {
	if !cp.EnvScopable(cp.GuardrailAddonSQL) {
		t.Fatal("addon.sql is not env-scopable, so `guard set --env dev addon.sql allow` would promise an override that is never read")
	}
	ctx := context.Background()
	e, d, prov := sqlEngine(t)
	// dev is open; the default environment keeps the deny.
	d.SetPolicy(permissive().With(cp.GuardrailCode("dev."+string(cp.GuardrailAddonSQL)), cp.DispositionAllow))
	if err := d.CreateEnvironment(ctx, "dev", "burrow-apps-dev"); err != nil {
		t.Fatalf("seed environment: %v", err)
	}
	instance, err := cp.AddonInstanceName(cp.AddonPostgres, "dev")
	if err != nil {
		t.Fatalf("AddonInstanceName: %v", err)
	}
	if err := d.SaveAddon(ctx, cp.AddonInfo{Name: instance, Type: cp.AddonPostgres, Environment: "dev", Mode: "installed"}); err != nil {
		t.Fatalf("seed dev add-on: %v", err)
	}
	if _, err := e.AttachAddon(ctx, cp.AddonPostgres, "web", "dev", ""); err != nil {
		t.Fatalf("AttachAddon in dev: %v", err)
	}

	if _, err := e.AddonSQL(ctx, cp.SQLRequest{Addon: cp.AddonPostgres, App: "web", Env: "dev", Statement: "select 1"}); err != nil {
		t.Fatalf("AddonSQL in dev, where the guardrail is allowed = %v", err)
	}
	// Named explicitly: with two environments registered, an unnamed one is refused before any
	// guardrail is consulted (ADR-0047 §1), and the point here is the disposition rather than that.
	if _, err := e.AddonSQL(ctx, cp.SQLRequest{Addon: cp.AddonPostgres, App: "web", Env: cp.DefaultEnvironment, Statement: "select 1"}); !isGuardrailCode(err, cp.GuardrailAddonSQL) {
		t.Fatalf("AddonSQL in the default environment = %v, want the deny that dev opted out of", err)
	}
	if got := prov.Statements(); len(got) != 1 || got[0].Env != "dev" {
		t.Errorf("statements = %+v, want exactly one, in dev", got)
	}
}

// TestAddonSQLRefusesNonRelationalAddon pins ADR-0087 §2. The refusal NAMES the verb the add-on does
// take, so it teaches that query surfaces belong to the add-on type rather than merely reporting a
// mismatch — and it happens before anything is resolved, because which query surface is right does
// not depend on what is installed.
func TestAddonSQLRefusesNonRelationalAddon(t *testing.T) {
	ctx := context.Background()
	e, d, prov := sqlEngine(t)
	allowSQL(t, d)

	cases := []struct {
		addon cp.AddonType
		want  string // a fragment of the verb that add-on does take
	}{
		{cp.AddonLogs, "addon logs"},
		{cp.AddonMetrics, "addon metrics"},
		// Cache has no query verb at all, so the refusal says that rather than inventing one.
		{cp.AddonCache, "addon list"},
	}
	for _, c := range cases {
		t.Run(string(c.addon), func(t *testing.T) {
			_, err := e.AddonSQL(ctx, cp.SQLRequest{Addon: c.addon, App: "web", Statement: "select 1"})
			if !errors.Is(err, cp.ErrInvalid) {
				t.Fatalf("AddonSQL(%s) = %v, want ErrInvalid", c.addon, err)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("refusal %q does not name %s's own query verb (%q)", err, c.addon, c.want)
			}
		})
	}
	if got := prov.Statements(); len(got) != 0 {
		t.Errorf("a non-relational add-on reached the database: %+v", got)
	}
}

// TestAddonSQLTargetsOneAppsDatabase pins ADR-0087 §1's boundary. The statement is aimed at ONE
// app's database and there is no argument that names anything else: the request carries an add-on
// TYPE and an app, and what reaches the seam is that app in that environment. There is no instance
// form, no `template1` form, and no way to name another app's database — the database-per-app
// boundary (ADR-0031) is the boundary here too.
func TestAddonSQLTargetsOneAppsDatabase(t *testing.T) {
	ctx := context.Background()
	e, d, prov := sqlEngine(t)
	allowSQL(t, d)
	// A second attached app on the SAME instance, so "it reached web's database" is a claim with
	// something to be wrong about.
	if _, err := e.AttachAddon(ctx, cp.AddonPostgres, "api", "", ""); err != nil {
		t.Fatalf("AttachAddon api: %v", err)
	}

	if _, err := e.AddonSQL(ctx, cp.SQLRequest{Addon: cp.AddonPostgres, App: "web", Statement: "select 1"}); err != nil {
		t.Fatalf("AddonSQL: %v", err)
	}
	got := prov.Statements()
	if len(got) != 1 {
		t.Fatalf("statements = %+v, want exactly one", got)
	}
	if got[0].App != "web" {
		t.Errorf("the statement ran against %q's database, want web's", got[0].App)
	}
	if got[0].Env != cp.DefaultEnvironment {
		t.Errorf("environment = %q, want %q — the environment selects the instance (ADR-0067 §1)", got[0].Env, cp.DefaultEnvironment)
	}
	// The credential named is the one ATTACH wrote for this app, which is what chooses the database:
	// the seam is handed a key into web's own Secret and never a database name to connect to.
	if got[0].SecretKey != cp.AppDatabaseURLKey {
		t.Errorf("secret key = %q, want %q — the app's own credential is what selects the database", got[0].SecretKey, cp.AppDatabaseURLKey)
	}
	// An app with no database on the instance cannot be reached at all: the fake refuses it exactly
	// as the real provisioner does, because there is no credential to connect with.
	if _, err := e.AddonSQL(ctx, cp.SQLRequest{Addon: cp.AddonPostgres, App: "unattached", Statement: "select 1"}); !errors.Is(err, cp.ErrNotFound) {
		t.Errorf("AddonSQL against an unattached app = %v, want ErrNotFound", err)
	}
}

// TestAddonSQLFollowsTheAttachmentsVariableName asserts the statement connects with the credential
// under the name THIS attachment recorded, not under a hardcoded DATABASE_URL. An app attached with
// `--as DB_URL` is reachable; assuming the default would connect with nothing.
func TestAddonSQLFollowsTheAttachmentsVariableName(t *testing.T) {
	ctx := context.Background()
	e, d, prov := sqlEngine(t)
	allowSQL(t, d)
	if _, err := e.AttachAddon(ctx, cp.AddonPostgres, "web", "", "DB_URL"); err != nil {
		t.Fatalf("AttachAddon --as DB_URL: %v", err)
	}

	if _, err := e.AddonSQL(ctx, cp.SQLRequest{Addon: cp.AddonPostgres, App: "web", Statement: "select 1"}); err != nil {
		t.Fatalf("AddonSQL: %v", err)
	}
	got := prov.Statements()
	if len(got) != 1 || got[0].SecretKey != "DB_URL" {
		t.Fatalf("secret key = %+v, want the name this attachment recorded (DB_URL)", got)
	}
}

// TestAddonSQLPassesTheStatementThrough pins ADR-0087 §6 at the level it is easiest to break: the
// statement reaches the database VERBATIM. Nothing parses it, prepends to it, or rewrites it, so a
// statement whose leading keyword says one thing and whose effect is another is not treated
// differently from any other.
func TestAddonSQLPassesTheStatementThrough(t *testing.T) {
	ctx := context.Background()
	e, d, prov := sqlEngine(t)
	allowSQL(t, d)

	// A SELECT that deletes. If anything ever starts classifying, this is the case that catches it.
	const stmt = "WITH deleted AS (DELETE FROM users RETURNING *) SELECT * FROM deleted"
	if _, err := e.AddonSQL(ctx, cp.SQLRequest{Addon: cp.AddonPostgres, App: "web", Statement: "  " + stmt + "\n"}); err != nil {
		t.Fatalf("AddonSQL: %v", err)
	}
	got := prov.Statements()
	if len(got) != 1 || got[0].Statement != stmt {
		t.Fatalf("statement = %q, want it passed through unmodified (surrounding whitespace aside)", got[0].Statement)
	}
}

// TestAddonSQLBounds pins ADR-0087 §7's limits: they come from the operational-limit catalogue, so
// an operator sets them with `cluster config set` and `guard set` cannot reach them — and they are
// resolved for the environment the statement runs in.
func TestAddonSQLBounds(t *testing.T) {
	ctx := context.Background()
	e, d, prov := sqlEngine(t)
	allowSQL(t, d)

	// Defaults first: an install that sets nothing still bounds every run.
	if _, err := e.AddonSQL(ctx, cp.SQLRequest{Addon: cp.AddonPostgres, App: "web", Statement: "select 1"}); err != nil {
		t.Fatalf("AddonSQL: %v", err)
	}
	got := prov.Statements()
	if len(got) != 1 {
		t.Fatalf("statements = %+v, want one", got)
	}
	if got[0].Timeout <= 0 {
		t.Error("the statement ran with no timeout, so a query could sit on the instance holding locks")
	}
	if got[0].MaxRows <= 0 {
		t.Error("the statement ran with no row cap")
	}

	// And an operator's own values are honoured.
	d.SetLimits(cp.OperationalConfig{}.
		With(cp.LimitAddonSQLTimeout, "90s").
		With(cp.LimitAddonSQLRows, "7"))
	if _, err := e.AddonSQL(ctx, cp.SQLRequest{Addon: cp.AddonPostgres, App: "web", Statement: "select 1"}); err != nil {
		t.Fatalf("AddonSQL: %v", err)
	}
	got = prov.Statements()
	last := got[len(got)-1]
	if last.Timeout.String() != "1m30s" {
		t.Errorf("timeout = %s, want the configured 90s", last.Timeout)
	}
	if last.MaxRows != 7 {
		t.Errorf("row cap = %d, want the configured 7", last.MaxRows)
	}
	// A limit is not a guardrail: `guard set` has no code for either of them (ADR-0068 §2).
	for _, code := range []cp.GuardrailCode{"addon.sql_timeout", "addon.sql_rows"} {
		if cp.KnownGuardrail(code) {
			t.Errorf("%q is a guardrail, so `guard set` could dispose of a bound away", code)
		}
	}
}

// TestAddonSQLTruncationIsReported pins ADR-0087 §7's truncation: a result cut short says so, and
// says what the cap was, so a caller is never handed a silently short answer.
func TestAddonSQLTruncationIsReported(t *testing.T) {
	ctx := context.Background()
	e, d, prov := sqlEngine(t)
	allowSQL(t, d)
	d.SetLimits(cp.OperationalConfig{}.With(cp.LimitAddonSQLRows, "2"))
	prov.SetQueryResult(cp.SQLResult{
		Columns:   []string{"id"},
		Rows:      [][]*string{{strptr("1")}, {strptr("2")}},
		RowCount:  2,
		Truncated: true,
	})

	res, err := e.AddonSQL(ctx, cp.SQLRequest{Addon: cp.AddonPostgres, App: "web", Statement: "select id from users"})
	if err != nil {
		t.Fatalf("AddonSQL: %v", err)
	}
	if !res.Truncated {
		t.Fatal("a truncated result did not report itself as truncated")
	}
	if res.RowLimit != 2 {
		t.Errorf("row limit = %d, want the cap that cut it (2), so a caller knows what to raise", res.RowLimit)
	}
	if res.RowCount != 2 {
		t.Errorf("row count = %d, want the number of rows RETURNED", res.RowCount)
	}
}

// TestAddonSQLDatabaseErrorIsAnOutcome pins ADR-0087 §4: a statement Postgres refuses comes back as
// a RESULT carrying the message and the SQLSTATE, not as an error from the call. The distinction is
// what lets an agent tell "the table is not there" (42P01, which it can act on) from "the instance
// would not answer" — and it is the same treatment ADR-0048 §3 gives a non-zero exit code.
func TestAddonSQLDatabaseErrorIsAnOutcome(t *testing.T) {
	ctx := context.Background()
	e, d, prov := sqlEngine(t)
	allowSQL(t, d)
	prov.SetQueryResult(cp.SQLResult{
		Error: &cp.SQLError{Message: `relation "users" does not exist`, SQLState: "42P01", Position: 15},
	})

	res, err := e.AddonSQL(ctx, cp.SQLRequest{Addon: cp.AddonPostgres, App: "web", Statement: "select * from users"})
	if err != nil {
		t.Fatalf("a database error became a call failure: %v", err)
	}
	if res.Error == nil {
		t.Fatal("the result carries no error, so the refusal was lost")
	}
	if res.Error.SQLState != "42P01" {
		t.Errorf("SQLSTATE = %q, want 42P01 intact", res.Error.SQLState)
	}
	if res.Error.Message != `relation "users" does not exist` {
		t.Errorf("message = %q, want Postgres's own words unmodified", res.Error.Message)
	}
	// The result still says which database it is about, so an error read on its own is attributable.
	if res.App != "web" || res.Addon != string(cp.AddonPostgres) || res.Environment != cp.DefaultEnvironment {
		t.Errorf("result identity = %+v, want web/postgres/%s", res, cp.DefaultEnvironment)
	}
}

// TestAddonSQLAuditsTheStatement pins ADR-0087's stated cost and the accountability that buys it:
// every run — denied or executed — is recorded WITH THE STATEMENT TEXT. Redacting a literal would
// mean parsing the statement, which §6 refuses to do, so this asserts the text is there rather than
// that it is not.
func TestAddonSQLAuditsTheStatement(t *testing.T) {
	ctx := context.Background()
	e, d, _ := sqlEngine(t)

	const stmt = "select id from users where email = 'a@example.com'"
	// Denied first: a refused statement is recorded too, which is what makes an attempt visible.
	if _, err := e.AddonSQL(ctx, cp.SQLRequest{Addon: cp.AddonPostgres, App: "web", Statement: stmt}); err == nil {
		t.Fatal("AddonSQL with the default policy should be denied")
	}
	allowSQL(t, d)
	if _, err := e.AddonSQL(ctx, cp.SQLRequest{Addon: cp.AddonPostgres, App: "web", Statement: stmt}); err != nil {
		t.Fatalf("AddonSQL: %v", err)
	}

	var denied, executed bool
	for _, row := range d.AuditRows() {
		if row.Operation != "addon_sql" {
			continue
		}
		if row.Args["statement"] != stmt {
			t.Errorf("audit row %q recorded statement %q, want the statement as run", row.Outcome, row.Args["statement"])
		}
		switch row.Outcome {
		case "denied":
			denied = true
		case "executed":
			executed = true
		}
	}
	if !denied {
		t.Error("the denied statement was not recorded")
	}
	if !executed {
		t.Error("the executed statement was not recorded")
	}
}

// TestAddonSQLRejectsAnEmptyStatement asserts nothing is audited or run for a request with no
// statement in it: it is a malformed request, not a guardrail question.
func TestAddonSQLRejectsAnEmptyStatement(t *testing.T) {
	ctx := context.Background()
	e, d, prov := sqlEngine(t)
	allowSQL(t, d)

	if _, err := e.AddonSQL(ctx, cp.SQLRequest{Addon: cp.AddonPostgres, App: "web", Statement: "   \n"}); !errors.Is(err, cp.ErrInvalid) {
		t.Fatalf("AddonSQL with a blank statement = %v, want ErrInvalid", err)
	}
	if got := prov.Statements(); len(got) != 0 {
		t.Errorf("a blank statement reached the database: %+v", got)
	}
}

// TestAddonSQLRefusesAnUninstalledAddon asserts an environment with no Postgres add-on is
// ErrNotFound naming the install, resolved BEFORE the guardrail — so a typo'd environment reads as
// "nothing is there" rather than as a refusal somebody would try to get relaxed.
func TestAddonSQLRefusesAnUninstalledAddon(t *testing.T) {
	ctx := context.Background()
	e, _, d, _ := newPostgresEngine(t)
	d.SetPolicy(permissive().With(cp.GuardrailAddonSQL, cp.DispositionAllow))

	_, err := e.AddonSQL(ctx, cp.SQLRequest{Addon: cp.AddonPostgres, App: "web", Statement: "select 1"})
	if !errors.Is(err, cp.ErrNotFound) {
		t.Fatalf("AddonSQL with no add-on installed = %v, want ErrNotFound", err)
	}
	if !strings.Contains(err.Error(), "addon install postgres") {
		t.Errorf("refusal %q does not name the command that would install one", err)
	}
}

// isGuardrailCode reports whether err is a guardrail refusal carrying code.
func isGuardrailCode(err error, code cp.GuardrailCode) bool {
	g, ok := cp.AsGuardrail(err)
	return ok && g.Code == code
}

func strptr(s string) *string { return &s }

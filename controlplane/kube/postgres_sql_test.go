// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package kube

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/burrow-cloud/burrow/controlplane"
)

// attachedSecret is the per-app Secret an attach leaves behind: the connection string burrowd
// generated, under the key the attachment recorded. It is what `addon sql` connects with.
func attachedSecret(app, ns, key, dsn string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: controlplane.AppSecretName(app), Namespace: ns},
		Data:       map[string][]byte{key: []byte(dsn)},
	}
}

// TestSQLConnectsAsTheAppsOwnRole is the load-bearing assertion of ADR-0087 §3. The connection is
// built from the credential ATTACH minted for the app — role `app_<app>`, database `<app>` — and not
// from the instance's superuser Secret, which this provisioner also holds and which every other
// method here uses.
//
// The database is therefore chosen by the CREDENTIAL and not by the caller, which is what makes
// ADR-0087 §1's boundary structural rather than a check: there is no argument to point at
// `template1`, at the instance, or at another app's database, and the role has CONNECT on its own
// database and no other (EnsureAppDatabase revokes it from PUBLIC).
func TestSQLConnectsAsTheAppsOwnRole(t *testing.T) {
	const dsn = "postgres://app_web:secretpw@burrow-postgres.burrow-addons.svc:5432/web?sslmode=disable"
	cfg, err := sqlConnConfig(dsn, 30*time.Second)
	if err != nil {
		t.Fatalf("sqlConnConfig: %v", err)
	}
	if cfg.User != roleName("web") {
		t.Errorf("connecting as %q, want the app's own role %q — never the instance superuser %q",
			cfg.User, roleName("web"), PostgresSuperuser)
	}
	if cfg.User == PostgresSuperuser {
		t.Error("the statement would run as the superuser, which raises what the app itself may touch")
	}
	if cfg.Database != "web" {
		t.Errorf("connecting to database %q, want the app's own (web): the credential chooses the database, not the caller", cfg.Database)
	}
}

// TestSQLConnectionIsBounded asserts the statement timeout is applied as a CONNECTION parameter
// (ADR-0087 §7). That is the difference between a bound and a suggestion: it is in force before the
// caller's statement is read, so there is nothing for a leading `SET` to sit in front of.
func TestSQLConnectionIsBounded(t *testing.T) {
	cfg, err := sqlConnConfig("postgres://app_web:pw@host:5432/web", 90*time.Second)
	if err != nil {
		t.Fatalf("sqlConnConfig: %v", err)
	}
	if got := cfg.RuntimeParams["statement_timeout"]; got != "90000" {
		t.Errorf("statement_timeout = %q, want 90000 (milliseconds)", got)
	}
	if got := cfg.RuntimeParams["application_name"]; got != sqlApplicationName {
		t.Errorf("application_name = %q, want %q so the connection is identifiable in pg_stat_activity", got, sqlApplicationName)
	}
}

// TestSQLRefusesBeforeConnecting asserts every malformed request is refused BEFORE a Secret is read
// or a connection opened — the same posture the provisioning methods take. An unnamed environment in
// particular is ErrInvalid rather than a silent fall back to whichever instance exists (ADR-0067 §1).
func TestSQLRefusesBeforeConnecting(t *testing.T) {
	ctx := context.Background()
	client := fake.NewSimpleClientset(attachedSecret("web", "burrow-apps", controlplane.AppDatabaseURLKey,
		"postgres://app_web:pw@burrow-postgres.burrow-addons.svc:5432/web"))
	p := NewPostgresProvisioner(client, addonNS)

	base := controlplane.AppStatement{
		App: "web", Env: controlplane.DefaultEnvironment, Namespace: "burrow-apps",
		SecretKey: controlplane.AppDatabaseURLKey, Statement: "select 1",
		Timeout: time.Second, MaxRows: 10,
	}
	cases := []struct {
		name   string
		mutate func(controlplane.AppStatement) controlplane.AppStatement
	}{
		{"no environment", func(q controlplane.AppStatement) controlplane.AppStatement { q.Env = ""; return q }},
		{"malformed environment", func(q controlplane.AppStatement) controlplane.AppStatement { q.Env = "not a label"; return q }},
		{"no app", func(q controlplane.AppStatement) controlplane.AppStatement { q.App = ""; return q }},
		{"an app name that is not an identifier", func(q controlplane.AppStatement) controlplane.AppStatement { q.App = "web; drop"; return q }},
		{"no namespace", func(q controlplane.AppStatement) controlplane.AppStatement { q.Namespace = ""; return q }},
		{"no statement", func(q controlplane.AppStatement) controlplane.AppStatement { q.Statement = ""; return q }},
		// Postgres reads statement_timeout = 0 as "no timeout at all", so an unset bound is refused
		// rather than given the permissive reading (ADR-0087 §7).
		{"no statement timeout", func(q controlplane.AppStatement) controlplane.AppStatement { q.Timeout = 0; return q }},
		{"no row cap", func(q controlplane.AppStatement) controlplane.AppStatement { q.MaxRows = 0; return q }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := p.QueryAppDatabase(ctx, c.mutate(base)); !errors.Is(err, controlplane.ErrInvalid) {
				t.Errorf("QueryAppDatabase = %v, want ErrInvalid", err)
			}
		})
	}
}

// TestSQLUnattachedAppIsNotFound asserts an app with no credential is reported as unattached, naming
// the attach that would give it one — rather than as a connection that failed, which reads like the
// instance being down.
func TestSQLUnattachedAppIsNotFound(t *testing.T) {
	ctx := context.Background()
	q := controlplane.AppStatement{
		App: "web", Env: controlplane.DefaultEnvironment, Namespace: "burrow-apps",
		SecretKey: controlplane.AppDatabaseURLKey, Statement: "select 1",
		Timeout: time.Second, MaxRows: 10,
	}

	t.Run("no Secret at all", func(t *testing.T) {
		p := NewPostgresProvisioner(fake.NewSimpleClientset(), addonNS)
		_, err := p.QueryAppDatabase(ctx, q)
		if !errors.Is(err, controlplane.ErrNotFound) {
			t.Fatalf("QueryAppDatabase = %v, want ErrNotFound", err)
		}
		if !strings.Contains(err.Error(), "addon attach postgres web") {
			t.Errorf("error %q does not name the attach that would give it a database", err)
		}
	})

	t.Run("a Secret with no connection string under the recorded key", func(t *testing.T) {
		client := fake.NewSimpleClientset(attachedSecret("web", "burrow-apps", "SOMETHING_ELSE", "x"))
		p := NewPostgresProvisioner(client, addonNS)
		if _, err := p.QueryAppDatabase(ctx, q); !errors.Is(err, controlplane.ErrNotFound) {
			t.Fatalf("QueryAppDatabase = %v, want ErrNotFound", err)
		}
	})
}

// TestSQLDoesNotLeakTheConnectionString asserts a credential Burrow cannot parse is reported by the
// KEY that held it and never by its value. The DSN carries the app's role password, and an error is
// the easiest place for a secret to end up in a log.
func TestSQLDoesNotLeakTheConnectionString(t *testing.T) {
	ctx := context.Background()
	const garbage = "not-a-dsn://app_web:supersecretpassword@nowhere"
	client := fake.NewSimpleClientset(attachedSecret("web", "burrow-apps", controlplane.AppDatabaseURLKey, garbage))
	p := NewPostgresProvisioner(client, addonNS)

	_, err := p.QueryAppDatabase(ctx, controlplane.AppStatement{
		App: "web", Env: controlplane.DefaultEnvironment, Namespace: "burrow-apps",
		SecretKey: controlplane.AppDatabaseURLKey, Statement: "select 1",
		Timeout: time.Second, MaxRows: 10,
	})
	if err == nil {
		t.Fatal("an unparseable connection string was accepted")
	}
	if strings.Contains(err.Error(), "supersecretpassword") || strings.Contains(err.Error(), garbage) {
		t.Errorf("the error carries the credential: %q", err)
	}
	if !strings.Contains(err.Error(), controlplane.AppDatabaseURLKey) {
		t.Errorf("error %q does not name the key that held it", err)
	}
}

// TestCopyRowKeepsNullDistinct asserts a NULL survives as a nil entry rather than collapsing into an
// empty string. They are different answers, and an agent branching on "is this set" needs to be able
// to tell them apart (ADR-0087 §4).
func TestCopyRowKeepsNullDistinct(t *testing.T) {
	row := copyRow([][]byte{nil, []byte(""), []byte("value")})
	if len(row) != 3 {
		t.Fatalf("copyRow returned %d cells, want 3", len(row))
	}
	if row[0] != nil {
		t.Errorf("a NULL became %q, want nil so it marshals as JSON null", *row[0])
	}
	if row[1] == nil || *row[1] != "" {
		t.Error("an empty string became NULL")
	}
	if row[2] == nil || *row[2] != "value" {
		t.Error("a value did not survive the copy")
	}
}

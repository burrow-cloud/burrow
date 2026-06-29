// SPDX-License-Identifier: FSL-1.1-ALv2
// Copyright 2026 Nicholas Phillips

package kube

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"regexp"
	"strings"

	_ "github.com/jackc/pgx/v5/stdlib" // registers the "pgx" database/sql driver

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/burrow-cloud/burrow/controlplane"
)

var _ controlplane.DatabaseProvisioner = (*PostgresProvisioner)(nil)

// appIdentifier is the strict pattern an app (and thus its database/role) name must match before
// any SQL is built: a lowercase letter followed by lowercase letters, digits, or hyphens
// (ADR-0031). App names already satisfy this (they are DNS-1123 labels); validating again here is
// defense-in-depth so a name can never carry SQL into an admin statement.
var appIdentifier = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)

// validateAppIdentifier rejects any name that is not a strict identifier, BEFORE it reaches SQL.
func validateAppIdentifier(app string) error {
	if app == "" {
		return fmt.Errorf("app name is empty: %w", controlplane.ErrInvalid)
	}
	if !appIdentifier.MatchString(app) {
		return fmt.Errorf("app name %q is not a valid identifier (want %s): %w", app, appIdentifier.String(), controlplane.ErrInvalid)
	}
	return nil
}

// quoteIdent renders s as a quoted SQL identifier: wrapped in double quotes with any embedded
// double quote doubled. The caller has already validated s against appIdentifier (which admits no
// double quote), so this is belt-and-braces — every identifier reaches Postgres quoted.
func quoteIdent(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

// quoteLiteral renders s as a single-quoted SQL string literal with embedded single quotes doubled
// — used for the generated role password, which cannot be a bind parameter in CREATE/ALTER ROLE.
// The password is base64url (no quotes), so this too is defensive.
func quoteLiteral(s string) string {
	return `'` + strings.ReplaceAll(s, `'`, `''`) + `'`
}

// PostgresProvisioner is the production controlplane.DatabaseProvisioner: it connects to the
// installed Postgres add-on as the burrow_admin superuser and gives each app its own database and
// login role (ADR-0031). It reads the superuser password from the burrow-postgres Secret in the
// add-on namespace through a Kubernetes client (a pod can only mount a Secret in its own namespace,
// so the password lives there), and reaches the instance in-cluster at
// burrow-postgres.<addon-ns>.svc:5432. It holds no long-lived database handle — it opens a
// short-lived connection per operation so a rotated superuser password is always picked up.
type PostgresProvisioner struct {
	client         kubernetes.Interface
	addonNamespace string
}

// NewPostgresProvisioner returns a provisioner over the given clientset and add-on namespace.
func NewPostgresProvisioner(client kubernetes.Interface, addonNamespace string) *PostgresProvisioner {
	if addonNamespace == "" {
		addonNamespace = defaultAddonNamespace
	}
	return &PostgresProvisioner{client: client, addonNamespace: addonNamespace}
}

// NewPostgresProvisionerFromConfig builds a provisioner from a REST config.
func NewPostgresProvisionerFromConfig(cfg *rest.Config, addonNamespace string) (*PostgresProvisioner, error) {
	client, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("kube: building clientset: %w", err)
	}
	return NewPostgresProvisioner(client, addonNamespace), nil
}

// instanceHost is the in-cluster host the add-on Postgres instance is reached at.
func (p *PostgresProvisioner) instanceHost() string {
	return fmt.Sprintf("%s.%s.svc", PostgresSecretName, p.addonNamespace)
}

// superuserPassword reads the generated superuser password from the burrow-postgres Secret. The
// value is used only to open the admin connection; it is never logged or returned.
func (p *PostgresProvisioner) superuserPassword(ctx context.Context) (string, error) {
	s, err := p.client.CoreV1().Secrets(p.addonNamespace).Get(ctx, PostgresSecretName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return "", fmt.Errorf("kube: postgres superuser secret %s/%s not found — is the postgres add-on installed?: %w", p.addonNamespace, PostgresSecretName, controlplane.ErrNotFound)
	}
	if err != nil {
		return "", fmt.Errorf("kube: reading postgres superuser secret %s/%s: %w", p.addonNamespace, PostgresSecretName, err)
	}
	pw, ok := s.Data[PostgresPasswordKey]
	if !ok {
		return "", fmt.Errorf("kube: postgres superuser secret %s/%s has no %q key: %w", p.addonNamespace, PostgresSecretName, PostgresPasswordKey, controlplane.ErrNotFound)
	}
	return string(pw), nil
}

// adminDSN composes the superuser connection string for the named maintenance database. The
// password is URL-encoded into the userinfo; this string is never logged or returned.
func (p *PostgresProvisioner) adminDSN(password, database string) string {
	u := &url.URL{
		Scheme:   "postgres",
		User:     url.UserPassword(PostgresSuperuser, password),
		Host:     p.instanceHost() + ":5432",
		Path:     "/" + database,
		RawQuery: "sslmode=disable",
	}
	return u.String()
}

// connectAdmin opens a short-lived superuser connection to the named maintenance database.
func (p *PostgresProvisioner) connectAdmin(ctx context.Context, database string) (*sql.DB, error) {
	pw, err := p.superuserPassword(ctx)
	if err != nil {
		return nil, err
	}
	db, err := sql.Open("pgx", p.adminDSN(pw, database))
	if err != nil {
		// sql.Open does not carry the DSN into the error, but be explicit: name no value.
		return nil, fmt.Errorf("kube: opening admin connection to %s: %w", p.instanceHost(), err)
	}
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("kube: connecting to %s: %w", p.instanceHost(), err)
	}
	return db, nil
}

// roleName is the login role for app: app_<app>. app is already validated.
func roleName(app string) string { return "app_" + app }

// EnsureAppDatabase provisions (idempotently) an isolated database and login role for app and
// returns its DATABASE_URL with a freshly generated password (ADR-0031). It validates app against
// the strict identifier pattern and quotes every identifier BEFORE any SQL runs. On a fresh attach
// it CREATEs the role and database and locks the database down to that role; on a re-attach (role
// or database already present) it ALTERs the role's password to rotate, so the returned URL is
// always current. The returned connection string is a SECRET value — the caller writes it straight
// into the app's Secret and never logs, audits, or returns it.
func (p *PostgresProvisioner) EnsureAppDatabase(ctx context.Context, app string) (string, error) {
	if err := validateAppIdentifier(app); err != nil {
		return "", err
	}
	role := roleName(app)
	password, err := generatePassword()
	if err != nil {
		return "", err
	}

	db, err := p.connectAdmin(ctx, "postgres")
	if err != nil {
		return "", err
	}
	defer db.Close()

	// Role: create with the generated password, or rotate the password if it already exists. Both
	// quote the identifier and inline the password as a quoted literal (it cannot be a bind param
	// in CREATE/ALTER ROLE). The password value never appears in any error or log.
	var roleExists bool
	if err := db.QueryRowContext(ctx, "SELECT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = $1)", role).Scan(&roleExists); err != nil {
		return "", fmt.Errorf("kube: checking role for %s: %w", app, err)
	}
	if roleExists {
		if _, err := db.ExecContext(ctx, fmt.Sprintf("ALTER ROLE %s WITH LOGIN PASSWORD %s", quoteIdent(role), quoteLiteral(password))); err != nil {
			return "", fmt.Errorf("kube: rotating role password for %s: %w", app, err)
		}
	} else {
		if _, err := db.ExecContext(ctx, fmt.Sprintf("CREATE ROLE %s WITH LOGIN PASSWORD %s", quoteIdent(role), quoteLiteral(password))); err != nil {
			return "", fmt.Errorf("kube: creating role for %s: %w", app, err)
		}
	}

	// Database: create owned by the role if absent (CREATE DATABASE cannot run in a transaction and
	// has no IF NOT EXISTS, so guard it with an existence check).
	var dbExists bool
	if err := db.QueryRowContext(ctx, "SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname = $1)", app).Scan(&dbExists); err != nil {
		return "", fmt.Errorf("kube: checking database for %s: %w", app, err)
	}
	if !dbExists {
		if _, err := db.ExecContext(ctx, fmt.Sprintf("CREATE DATABASE %s OWNER %s", quoteIdent(app), quoteIdent(role))); err != nil {
			return "", fmt.Errorf("kube: creating database for %s: %w", app, err)
		}
	}

	// Lock the database down to this app's role: revoke CONNECT from PUBLIC, grant it to the role.
	// Idempotent — re-running is a no-op.
	if _, err := db.ExecContext(ctx, fmt.Sprintf("REVOKE CONNECT ON DATABASE %s FROM PUBLIC", quoteIdent(app))); err != nil {
		return "", fmt.Errorf("kube: revoking public connect for %s: %w", app, err)
	}
	if _, err := db.ExecContext(ctx, fmt.Sprintf("GRANT CONNECT ON DATABASE %s TO %s", quoteIdent(app), quoteIdent(role))); err != nil {
		return "", fmt.Errorf("kube: granting connect for %s: %w", app, err)
	}

	// Compose the app's connection string. Built with net/url so the password is correctly
	// percent-encoded into the userinfo. This is the secret value the caller writes into the Secret.
	appURL := &url.URL{
		Scheme:   "postgres",
		User:     url.UserPassword(role, password),
		Host:     p.instanceHost() + ":5432",
		Path:     "/" + app,
		RawQuery: "sslmode=disable",
	}
	return appURL.String(), nil
}

// DropAppDatabase drops app's database and login role from the shared instance (ADR-0031). It
// validates app and quotes identifiers before any SQL. Dropping an already-absent database or role
// is a no-op (IF EXISTS), not an error. The database is dropped WITH (FORCE) so live sessions do
// not block teardown.
func (p *PostgresProvisioner) DropAppDatabase(ctx context.Context, app string) error {
	if err := validateAppIdentifier(app); err != nil {
		return err
	}
	role := roleName(app)

	db, err := p.connectAdmin(ctx, "postgres")
	if err != nil {
		return err
	}
	defer db.Close()

	if _, err := db.ExecContext(ctx, fmt.Sprintf("DROP DATABASE IF EXISTS %s WITH (FORCE)", quoteIdent(app))); err != nil {
		return fmt.Errorf("kube: dropping database for %s: %w", app, err)
	}
	if _, err := db.ExecContext(ctx, fmt.Sprintf("DROP ROLE IF EXISTS %s", quoteIdent(role))); err != nil {
		return fmt.Errorf("kube: dropping role for %s: %w", app, err)
	}
	return nil
}

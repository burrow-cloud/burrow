// SPDX-License-Identifier: Apache-2.0
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

var (
	_ controlplane.DatabaseProvisioner = (*PostgresProvisioner)(nil)
	_ controlplane.AppDatabaseLister   = (*PostgresProvisioner)(nil)
)

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

// PostgresTarget is one Postgres instance a provisioner may act on: the instance's name and the
// namespace it runs in. Everything the provisioner needs in order to reach a server is composed from
// this pair — the Service it dials and the Secret it reads that server's superuser password from,
// both named after the instance (ADR-0067 §1) — so the pair is the whole answer to "which database
// is this".
type PostgresTarget struct {
	// Instance is the add-on instance's name, which is also the name of its Service and of its
	// superuser Secret.
	Instance string
	// Namespace is the namespace that instance runs in, within the cluster the provisioner's
	// Kubernetes client is pointed at.
	Namespace string
}

// Host is the in-cluster DNS name of the target's Postgres Service. It is what goes into the
// DATABASE_URL of an app attached to this instance: apps are pods and resolve it through cluster DNS.
func (t PostgresTarget) Host() string {
	return fmt.Sprintf("%s.%s.svc", t.Instance, t.Namespace)
}

// PostgresTargetFunc resolves an environment to the instance a provisioner acts on for it. A
// provisioner is GIVEN one at construction and has no other way to name an instance.
//
// That is the point of the seam: A NAME THAT HAS TO BE CONFIGURED CANNOT SILENTLY BE THE WRONG ONE.
// Deriving the Service name inside the provisioner was correct exactly as long as burrowd and the
// database it provisions share a cluster, where `burrow-postgres.burrow-addons.svc` has no way to
// mean anything else. The moment they did not, the same derivation resolved in whatever cluster the
// caller happened to be running in and reached a DIFFERENT database of that name — with nothing but
// a password mismatch between it and a write (issue #519). Resolving is not the hazard; resolving a
// name nobody chose is. A caller whose instance is somewhere else must now say where, and a caller
// that says nothing at all reaches no instance rather than the nearest one.
type PostgresTargetFunc func(env string) (PostgresTarget, error)

// AddonInstanceTarget is the target of a single-tenant install — the one an existing install already
// has, and the value burrowd is wired with: environment env's add-on instance (AddonInstanceName,
// the single derivation shared with the installer and the registry — ADR-0067 §1) in the add-on
// namespace burrowd was installed with. An empty namespace means the default one, so an install that
// never set BURROW_ADDON_NAMESPACE names exactly the instance, Secret, and host it has always used.
func AddonInstanceTarget(addonNamespace string) PostgresTargetFunc {
	if addonNamespace == "" {
		addonNamespace = defaultAddonNamespace
	}
	return func(env string) (PostgresTarget, error) {
		instance, err := postgresSecretName(env)
		if err != nil {
			return PostgresTarget{}, err
		}
		return PostgresTarget{Instance: instance, Namespace: addonNamespace}, nil
	}
}

// PostgresProvisioner is the production controlplane.DatabaseProvisioner: it connects to an
// environment's Postgres add-on instance as the burrow_admin superuser and gives each app its own
// database and login role (ADR-0031). It reads that instance's superuser password from the Secret of
// the same name in the instance's namespace through a Kubernetes client (a pod can only mount a
// Secret in its own namespace, so the password lives there), and reaches the instance at its Service
// on port 5432. It holds no long-lived database handle — it opens a short-lived connection per
// operation so a rotated superuser password is always picked up.
//
// EVERY OPERATION IS SCOPED TO AN ENVIRONMENT (ADR-0067 §1). The provisioner has no notion of "the"
// instance: the environment argument selects the host and the credential together, so a call cannot
// reach an instance other than the named environment's, and a call that names no environment reaches
// none at all. That is what makes the issue #339 collision unrepresentable rather than merely
// avoided — `web` in staging and `web` in production are databases with the same name on different
// servers, and no code path can resolve one to the other.
//
// WHICH instances those are is configuration, not derivation (PostgresTargetFunc, issue #519): the
// environment picks one out of the set the provisioner was given, and the provisioner can name no
// instance outside it.
type PostgresProvisioner struct {
	client kubernetes.Interface
	// target is the instance set this provisioner acts on, given at construction. There is no
	// fallback if it is unset — see targetFor.
	target PostgresTargetFunc
	// adminEndpoint, when non-empty, overrides the host:port the provisioner DIALS to run admin
	// SQL. In production it is empty: burrowd is an in-cluster pod and reaches the instance at its
	// Service DNS name (instanceHost). It exists only so an out-of-cluster test (which cannot
	// resolve a .svc name) can point the admin connection at a port-forwarded local address. It
	// never affects the app's DATABASE_URL, which is always the target's in-cluster Service name.
	adminEndpoint string
}

// NewPostgresProvisioner returns a provisioner over the given clientset that acts on the instances
// target names. The target is a required argument rather than something the provisioner works out
// for itself: there is no value meaning "whichever instance this cluster happens to have"
// (PostgresTargetFunc). An in-cluster single-tenant install passes AddonInstanceTarget.
func NewPostgresProvisioner(client kubernetes.Interface, target PostgresTargetFunc) *PostgresProvisioner {
	return &PostgresProvisioner{client: client, target: target}
}

// WithAdminEndpoint overrides the host:port the provisioner dials for admin SQL (see adminEndpoint).
// It is for tests that reach the instance through a port-forward; production leaves it unset.
func (p *PostgresProvisioner) WithAdminEndpoint(hostPort string) *PostgresProvisioner {
	p.adminEndpoint = hostPort
	return p
}

// NewPostgresProvisionerFromConfig builds a provisioner from a REST config, acting on the instances
// target names.
func NewPostgresProvisionerFromConfig(cfg *rest.Config, target PostgresTargetFunc) (*PostgresProvisioner, error) {
	client, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("kube: building clientset: %w", err)
	}
	return NewPostgresProvisioner(client, target), nil
}

// targetFor resolves environment env to the instance this provisioner was configured to act on. A
// provisioner built without a target resolves nothing: it fails closed here rather than falling back
// to a name derived from wherever it is running, which is the failure this seam exists to remove.
func (p *PostgresProvisioner) targetFor(env string) (PostgresTarget, error) {
	if p.target == nil {
		return PostgresTarget{}, fmt.Errorf("kube: the postgres provisioner was given no instance to act on: %w", controlplane.ErrInvalid)
	}
	return p.target(env)
}

// instanceHost is the host environment env's Postgres instance is reached at. This is what goes into
// that environment's apps' DATABASE_URLs — apps are pods and resolve it through cluster DNS. The
// environment selects one of the instances the provisioner was configured with, so the URL an app is
// handed names the server its own environment runs.
func (p *PostgresProvisioner) instanceHost(env string) (string, error) {
	target, err := p.targetFor(env)
	if err != nil {
		return "", err
	}
	return target.Host(), nil
}

// adminHostPort is the host:port the provisioner DIALS to run admin SQL for env: the test override
// if set, otherwise the in-cluster Service address. Distinct from instanceHost so the override never
// leaks into an app's DATABASE_URL.
func (p *PostgresProvisioner) adminHostPort(env string) (string, error) {
	if p.adminEndpoint != "" {
		return p.adminEndpoint, nil
	}
	host, err := p.instanceHost(env)
	if err != nil {
		return "", err
	}
	return host + ":5432", nil
}

// superuserPassword reads the generated superuser password from environment env's instance Secret.
// Each instance has its own credential (ADR-0067 §1), so reading it is also how a wrong environment
// fails closed: an environment with no instance installed has no Secret, and the operation stops
// with ErrNotFound before any connection is opened. The Secret is looked up in the TARGET's
// namespace under the target's name, so the credential comes from the same place the host does and
// the two cannot disagree. The value is used only to open the admin connection; it is never logged
// or returned.
func (p *PostgresProvisioner) superuserPassword(ctx context.Context, env string) (string, error) {
	target, err := p.targetFor(env)
	if err != nil {
		return "", err
	}
	ns, name := target.Namespace, target.Instance
	s, err := p.client.CoreV1().Secrets(ns).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return "", fmt.Errorf("kube: postgres superuser secret %s/%s not found — is the postgres add-on installed in environment %q?: %w", ns, name, env, controlplane.ErrNotFound)
	}
	if err != nil {
		return "", fmt.Errorf("kube: reading postgres superuser secret %s/%s: %w", ns, name, err)
	}
	pw, ok := s.Data[PostgresPasswordKey]
	if !ok {
		return "", fmt.Errorf("kube: postgres superuser secret %s/%s has no %q key: %w", ns, name, PostgresPasswordKey, controlplane.ErrNotFound)
	}
	return string(pw), nil
}

// adminDSN composes the superuser connection string for the named maintenance database on env's
// instance. The password is URL-encoded into the userinfo; this string is never logged or returned.
func (p *PostgresProvisioner) adminDSN(password, database, hostPort string) string {
	u := &url.URL{
		Scheme:   "postgres",
		User:     url.UserPassword(PostgresSuperuser, password),
		Host:     hostPort,
		Path:     "/" + database,
		RawQuery: "sslmode=disable",
	}
	return u.String()
}

// connectAdmin opens a short-lived superuser connection to the named maintenance database on
// environment env's instance. Host and credential are resolved from env together, so there is no
// state on the provisioner that could point one operation at a different server than another.
func (p *PostgresProvisioner) connectAdmin(ctx context.Context, env, database string) (*sql.DB, error) {
	hostPort, err := p.adminHostPort(env)
	if err != nil {
		return nil, err
	}
	pw, err := p.superuserPassword(ctx, env)
	if err != nil {
		return nil, err
	}
	db, err := sql.Open("pgx", p.adminDSN(pw, database, hostPort))
	if err != nil {
		// sql.Open does not carry the DSN into the error, but be explicit: name no value.
		return nil, fmt.Errorf("kube: opening admin connection for environment %q: %w", env, err)
	}
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("kube: connecting to the %q environment's postgres instance: %w", env, err)
	}
	return db, nil
}

// roleName is the login role for app: app_<app>. app is already validated.
func roleName(app string) string { return "app_" + app }

// EnsureAppDatabase provisions (idempotently) an isolated database and login role for app on
// environment env's instance and returns its DATABASE_URL with a freshly generated password
// (ADR-0031). It validates env and app against the strict identifier patterns and quotes every
// identifier BEFORE any SQL runs. On a fresh attach it CREATEs the role and database and locks the
// database down to that role; on a re-attach (role or database already present) it ALTERs the role's
// password to rotate, so the returned URL is always current. The returned connection string is a
// SECRET value — the caller writes it straight into the app's Secret and never logs, audits, or
// returns it.
//
// Idempotence is what made the missing environment dangerous rather than merely wrong (issue #339):
// finding an existing database is the NORMAL case of a re-attach, so a second environment's attach
// did not fail — it adopted the first environment's database and rotated its password. With the
// environment selecting the instance, the only database this can find is one on that environment's
// own server, and adopting it is again exactly what a re-attach should do.
func (p *PostgresProvisioner) EnsureAppDatabase(ctx context.Context, app, env string) (string, error) {
	if err := validateAppIdentifier(app); err != nil {
		return "", err
	}
	// The environment is resolved to an instance BEFORE any connection or SQL: an empty or malformed
	// environment is ErrInvalid here, never a silent fallback to whichever instance exists.
	target, err := p.targetFor(env)
	if err != nil {
		return "", err
	}
	role := roleName(app)
	password, err := generatePassword()
	if err != nil {
		return "", err
	}

	db, err := p.connectAdmin(ctx, env, "postgres")
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
		Host:     target.Host() + ":5432",
		Path:     "/" + app,
		RawQuery: "sslmode=disable",
	}
	return appURL.String(), nil
}

// ListAppDatabases returns the apps that hold a Burrow-provisioned database on environment env's
// instance, sorted (ADR-0031). It asks the instance itself rather than any registry, because the
// instance is
// the only place that knows: attach records the FACT of attachment nowhere but the app's own Secret
// and the databases on this server, and it is these databases — not a row somewhere — that a
// data-deleting add-on removal destroys.
//
// The set is derived from ownership, not from names: a database whose owner is one of the app_<app>
// login roles attach creates is a provisioned app database, and its name is the app's name. That
// excludes the maintenance databases (postgres, template0/template1, all owned by the superuser) and
// anything a human created by hand as the superuser, without needing a naming convention to hold.
func (p *PostgresProvisioner) ListAppDatabases(ctx context.Context, env string) ([]string, error) {
	db, err := p.connectAdmin(ctx, env, "postgres")
	if err != nil {
		return nil, err
	}
	defer db.Close()

	// The role prefix is matched with an explicit ESCAPE so the underscore in "app_" is a literal
	// underscore rather than LIKE's single-character wildcard.
	const q = `SELECT d.datname
FROM pg_database d
JOIN pg_roles r ON d.datdba = r.oid
WHERE r.rolname LIKE 'app\_%' ESCAPE '\' AND NOT d.datistemplate
ORDER BY d.datname`
	rows, err := db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("kube: listing app databases: %w", err)
	}
	defer rows.Close()

	apps := []string{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("kube: listing app databases: %w", err)
		}
		apps = append(apps, name)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("kube: listing app databases: %w", err)
	}
	return apps, nil
}

// DropAppDatabase drops app's database and login role from environment env's instance (ADR-0031).
// It validates env and app and quotes identifiers before any SQL. Dropping an already-absent
// database or role is a no-op (IF EXISTS), not an error. The database is dropped WITH (FORCE) so
// live sessions do not block teardown. The environment is required and unvalidated values are
// refused before the connection is opened: this is the destructive half of the pair, so reaching
// another environment's server here would drop a database that is still in use (ADR-0067 §1).
func (p *PostgresProvisioner) DropAppDatabase(ctx context.Context, app, env string) error {
	if err := validateAppIdentifier(app); err != nil {
		return err
	}
	if _, err := p.targetFor(env); err != nil {
		return err
	}
	role := roleName(app)

	db, err := p.connectAdmin(ctx, env, "postgres")
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

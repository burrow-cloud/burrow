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
	"time"

	_ "github.com/jackc/pgx/v5/stdlib" // registers the "pgx" database/sql driver

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/burrow-cloud/burrow/controlplane"
)

var (
	_ controlplane.DatabaseProvisioner = (*PostgresProvisioner)(nil)
	_ controlplane.AppDatabaseLister   = (*PostgresProvisioner)(nil)
)

// appIdentifier is the strict pattern an app (and thus its database/role) name must match before
// anything is built from it: a lowercase letter followed by lowercase letters, digits, or hyphens
// (ADR-0031). App names already satisfy this (they are DNS-1123 labels); validating again here is
// defense-in-depth.
//
// It now bounds TWO things rather than one. A name still cannot carry SQL into a statement — the one
// statement left interpolates it — and it also cannot carry anything into an object name: admitting
// no dot is what lets provisioningObjectName split an instance from an app again, and admitting no
// uppercase or underscore is what keeps the result a legal Kubernetes name.
var appIdentifier = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)

// validateAppIdentifier rejects any name that is not a strict identifier, BEFORE it reaches SQL or
// an object name.
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

// PostgresTarget is one Postgres instance a provisioner may act on: the instance's name and the
// namespace it runs in. Everything the provisioner needs in order to reach a server is composed from
// this pair — the CloudNativePG `Cluster` an attachment's objects name, the Service it dials, and
// the Secret it reads that server's superuser password from, all three named after the instance
// (ADR-0067 §1) — so the pair is the whole answer to "which database is this".
//
// ONE FIELD STILL NAMES ALL THREE, and that is worth stating because declarative provisioning was
// the moment it might have stopped being true. The `Database` and `DatabaseRole` objects reference
// their instance by `cluster.name`, which is the `Cluster` Burrow wrote at install under exactly the
// name below, and the additional managed Service and the superuser Secret already carry it
// (cnpg_cluster.go). Nothing here needs splitting; if a future instance is ever reached under one
// name and credentialed under another, this is the type that grows a second field rather than the
// place that works around it.
type PostgresTarget struct {
	// Instance is the add-on instance's name, which is also the name of its CloudNativePG `Cluster`,
	// of its Service, and of its superuser Secret.
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

// PostgresProvisioner is the production controlplane.DatabaseProvisioner: it gives each app its own
// database and login role on an environment's Postgres add-on instance (ADR-0031).
//
// IT PROVISIONS BY WRITING OBJECTS, NOT BY RUNNING ADMIN SQL (ADR-0066 §2). An attach writes a
// CloudNativePG `Database` and a `DatabaseRole` and waits for the operator to report them applied;
// the operator runs the SQL, on the instance it already manages, with the credential it already
// holds. Three things follow, and they are the reason for the change rather than a side effect of
// it:
//
//   - **burrowd holds no superuser DSN for a provision.** The credential stays where the database
//     is. Nothing on this path reads the superuser Secret, and no generated password is ever
//     interpolated into a statement.
//   - **A provision survives a restart.** The object is the record that the database was asked for,
//     so a control plane that dies mid-attach leaves the operator finishing rather than an app
//     half-provisioned with nothing recording that it was ever asked for.
//   - **Deleting the objects does not destroy the data.** Both carry a `retain` reclaim policy, so a
//     detach releases the description and keeps the database (ADR-0064).
//
// TWO STATEMENTS SURVIVE, both deliberately, and neither is a superuser one on the attach path.
// A `Database` cannot express a grant, so the `REVOKE CONNECT ... FROM PUBLIC` that keeps one app's
// role out of another app's database runs on a connection opened as the database's own OWNER
// (lockDownDatabase). And ListAppDatabases still asks the server, because the question it answers —
// whose data is in this volume — is one the objects cannot answer about a database a detach retained.
//
// EVERY OPERATION IS SCOPED TO AN ENVIRONMENT (ADR-0067 §1). The provisioner has no notion of "the"
// instance: the environment argument selects the instance and the credential together, so a call
// cannot reach an instance other than the named environment's, and a call that names no environment
// reaches none at all. That is what makes the issue #339 collision unrepresentable rather than merely
// avoided — `web` in staging and `web` in production are databases with the same name on different
// servers, and no code path can resolve one to the other.
//
// WHICH instances those are is configuration, not derivation (PostgresTargetFunc, issue #519): the
// environment picks one out of the set the provisioner was given, and the provisioner can name no
// instance outside it.
type PostgresProvisioner struct {
	client kubernetes.Interface
	// dynamic addresses the `Database` and `DatabaseRole` custom resources an attach provisions
	// through. It is REQUIRED, not optional: there is no admin-SQL path to fall back to, so a
	// provisioner without one refuses to provision rather than reaching for the superuser.
	dynamic dynamic.Interface
	// target is the instance set this provisioner acts on, given at construction. There is no
	// fallback if it is unset — see targetFor.
	target PostgresTargetFunc
	// instanceEndpoint, when non-empty, overrides the host:port the provisioner DIALS for the two
	// statements it still runs. In production it is empty: burrowd is an in-cluster pod and reaches
	// the instance at its Service DNS name (instanceHost). It exists only so an out-of-cluster test
	// (which cannot resolve a .svc name) can point those connections at a port-forwarded local
	// address. It never affects the app's DATABASE_URL, which is always the target's in-cluster
	// Service name.
	instanceEndpoint string
	// open is how a connection is opened, so a test can substitute one and assert what was run and
	// as whom. Nil means openPostgres — see connect.
	open sqlOpenFunc
	// The bounds on waiting for somebody else's work: how long the operator has to apply an object,
	// how long a rotated password has to reach the running server, and how often each is re-read.
	// Fields rather than constants so a test does not have to spend them.
	applyTimeout      time.Duration
	credentialTimeout time.Duration
	pollInterval      time.Duration
}

// The default bounds on the two waits an attach performs. Both are generous: an object is applied in
// well under a second on a healthy instance, and the numbers are sized for an instance that is still
// starting rather than for the ordinary case.
const (
	defaultCNPGApplyTimeout      = 2 * time.Minute
	defaultCNPGCredentialTimeout = 90 * time.Second
	defaultCNPGPollInterval      = time.Second
)

// NewPostgresProvisioner returns a provisioner over the given clients that acts on the instances
// target names.
//
// Both the dynamic client and the target are required arguments rather than things the provisioner
// works out for itself. The target because there is no value meaning "whichever instance this
// cluster happens to have" (PostgresTargetFunc); the dynamic client because provisioning IS writing
// custom resources, so a caller that has not wired one has not wired a provisioner — and a missing
// argument is a better place to find that out than a failed attach. An in-cluster single-tenant
// install passes AddonInstanceTarget.
func NewPostgresProvisioner(client kubernetes.Interface, dyn dynamic.Interface, target PostgresTargetFunc) *PostgresProvisioner {
	return &PostgresProvisioner{
		client:            client,
		dynamic:           dyn,
		target:            target,
		applyTimeout:      defaultCNPGApplyTimeout,
		credentialTimeout: defaultCNPGCredentialTimeout,
		pollInterval:      defaultCNPGPollInterval,
	}
}

// WithInstanceEndpoint overrides the host:port the provisioner dials (see instanceEndpoint). It is
// for tests that reach the instance through a port-forward; production leaves it unset.
func (p *PostgresProvisioner) WithInstanceEndpoint(hostPort string) *PostgresProvisioner {
	p.instanceEndpoint = hostPort
	return p
}

// NewPostgresProvisionerFromConfig builds a provisioner from a REST config, acting on the instances
// target names. It wires both clients from the one config, so burrowd cannot end up with a typed
// client pointed at one cluster and a dynamic client pointed at another.
func NewPostgresProvisionerFromConfig(cfg *rest.Config, target PostgresTargetFunc) (*PostgresProvisioner, error) {
	client, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("kube: building clientset: %w", err)
	}
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("kube: building dynamic client: %w", err)
	}
	return NewPostgresProvisioner(client, dyn, target), nil
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

// dialHostPort is the host:port the provisioner DIALS for env: the test override if set, otherwise
// the in-cluster Service address. Distinct from instanceHost so the override never leaks into an
// app's DATABASE_URL.
func (p *PostgresProvisioner) dialHostPort(env string) (string, error) {
	if p.instanceEndpoint != "" {
		return p.instanceEndpoint, nil
	}
	host, err := p.instanceHost(env)
	if err != nil {
		return "", err
	}
	return host + ":5432", nil
}

// sqlConn is the part of *sql.DB the provisioner uses. It is an interface so a test can substitute a
// recorder and assert WHAT was run and, more to the point, AS WHOM — "this attach opened no
// superuser connection" is a claim about a connection string, and a claim about a connection string
// is only testable if something can see it.
type sqlConn interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	Close() error
}

// sqlOpenFunc opens a live connection to dsn, or reports why it could not. A returned connection has
// already been reached — an opener that only parsed the DSN would defer every failure to the first
// statement, and one caller (the credential wait) is asking precisely whether the server accepts
// this credential yet.
type sqlOpenFunc func(ctx context.Context, dsn string) (sqlConn, error)

// openPostgres is the production opener: the pgx driver through database/sql, pinged so the returned
// handle is known to be usable.
func openPostgres(ctx context.Context, dsn string) (sqlConn, error) {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, err
	}
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

// connect opens a connection to dsn through the provisioner's opener. The DSN is a secret value, so
// it appears in no error raised here — the caller adds the context that names the app and the
// environment instead.
func (p *PostgresProvisioner) connect(ctx context.Context, dsn string) (sqlConn, error) {
	open := p.open
	if open == nil {
		open = openPostgres
	}
	return open(ctx, dsn)
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
//
// EXACTLY ONE CALLER REMAINS: ListAppDatabases. Provisioning does not use it any more (ADR-0066 §2),
// and a second caller appearing here is worth arguing about rather than adding — every one of them
// is a superuser DSN burrowd holds.
func (p *PostgresProvisioner) connectAdmin(ctx context.Context, env, database string) (sqlConn, error) {
	hostPort, err := p.dialHostPort(env)
	if err != nil {
		return nil, err
	}
	pw, err := p.superuserPassword(ctx, env)
	if err != nil {
		return nil, err
	}
	db, err := p.connect(ctx, p.adminDSN(pw, database, hostPort))
	if err != nil {
		// The error never carries the DSN: name the environment, not the value.
		return nil, fmt.Errorf("kube: connecting to the %q environment's postgres instance: %w", env, err)
	}
	return db, nil
}

// roleName is the login role for app: app_<app>. app is already validated.
func roleName(app string) string { return "app_" + app }

// appDSN composes app's connection string against hostPort. Built with net/url so the password is
// correctly percent-encoded into the userinfo. This is a SECRET value.
func appDSN(role, password, hostPort, app string) string {
	u := &url.URL{
		Scheme:   "postgres",
		User:     url.UserPassword(role, password),
		Host:     hostPort,
		Path:     "/" + app,
		RawQuery: "sslmode=disable",
	}
	return u.String()
}

// EnsureAppDatabase provisions (idempotently) an isolated database and login role for app on
// environment env's instance and returns its DATABASE_URL with a freshly generated password
// (ADR-0031). The returned connection string is a SECRET value — the caller writes it straight into
// the app's Secret and never logs, audits, or returns it.
//
// IT OPENS NO SUPERUSER CONNECTION. The database and the role are a `Database` and a `DatabaseRole`
// object, and CloudNativePG runs the SQL for them on the instance it already manages (ADR-0066 §2).
// The password is written into a Secret the operator reads, so it never reaches a statement either.
//
// The sequence is four steps and the order is load-bearing:
//
//  1. The role's password Secret, BEFORE the role that references it — a `DatabaseRole` pointing at
//     a Secret that does not exist yet is the operator racing the control plane into a failure.
//  2. The `DatabaseRole`, before the `Database` that names it as owner.
//  3. The `Database`.
//  4. A wait on both, because provisioning is asynchronous now: `status.applied` is the only thing
//     that says the server has actually got them.
//
// AN EXISTING INSTALL IS ADOPTED, NOT RE-PROVISIONED. An app attached before this existed has its
// database and role already; the objects written here describe them rather than ask for them again,
// and CloudNativePG reconciles what is there. That is the same idempotence a re-attach has always
// relied on, moved from an existence check in this function into the operator (applyCNPGObject).
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
	// The environment is resolved to an instance BEFORE anything is written: an empty or malformed
	// environment is ErrInvalid here, never a silent fallback to whichever instance exists.
	target, err := p.targetFor(env)
	if err != nil {
		return "", err
	}
	databases, roles, err := p.cnpgObjects(target)
	if err != nil {
		return "", err
	}
	role := roleName(app)
	password, err := generatePassword()
	if err != nil {
		return "", err
	}
	name := provisioningObjectName(target.Instance, app)
	// The descriptive labels an add-on's resources carry, so an attachment's objects are legible in
	// the namespace as this add-on's, for this environment. Not the selectable `managed-by` one:
	// AddonVolumes uses that to attribute claims, and nothing should select these by sweeping.
	labels := map[string]string{
		addonLabel:    string(controlplane.AddonPostgres),
		addonEnvLabel: env,
	}
	// The Secret carries one label the objects do not, and it is functional rather than descriptive:
	// it is what makes CloudNativePG notice a rewritten password.
	secretLabels := map[string]string{cnpgReloadLabel: "true"}
	for k, v := range labels {
		secretLabels[k] = v
	}

	if err := p.ensureRolePasswordSecret(ctx, target, app, password, secretLabels); err != nil {
		return "", err
	}
	if err := applyCNPGObject(ctx, roles, cnpgDatabaseRoleKind, name, target.Namespace, databaseRoleSpec(target, app), labels); err != nil {
		return "", err
	}
	if err := applyCNPGObject(ctx, databases, cnpgDatabaseKind, name, target.Namespace, databaseSpec(target, app), labels); err != nil {
		return "", err
	}
	if err := awaitCNPGApplied(ctx, roles, cnpgDatabaseRoleKind, name, p.applyTimeout, p.pollInterval); err != nil {
		return "", fmt.Errorf("kube: provisioning the login role for %s in environment %q: %w", app, env, err)
	}
	if err := awaitCNPGApplied(ctx, databases, cnpgDatabaseKind, name, p.applyTimeout, p.pollInterval); err != nil {
		return "", fmt.Errorf("kube: provisioning the database for %s in environment %q: %w", app, env, err)
	}

	// The address this process can actually reach the instance at, which in production is the same
	// in-cluster Service name the app is handed and out of a test's port-forward is not.
	dial, err := p.dialHostPort(env)
	if err != nil {
		return "", err
	}
	if err := p.lockDownDatabase(ctx, appDSN(role, password, dial, app), app, env); err != nil {
		return "", err
	}
	return appDSN(role, password, target.Host()+":5432", app), nil
}

// lockDownDatabase runs the one statement provisioning cannot express as an object:
//
//	REVOKE CONNECT ON DATABASE <app> FROM PUBLIC
//
// WITHOUT IT THE DATABASE-PER-APP BOUNDARY IS NOT A BOUNDARY. PostgreSQL grants CONNECT on a new
// database to PUBLIC by default, and PUBLIC includes every login role on the server — so every other
// app's role could open every other app's database. That is ADR-0031's isolation, and it is also the
// whole safety argument of `burrow addon sql`, which connects as the app's own role precisely
// because that role reaches its own database and no other (ADR-0087 §3). The `Database` CRD has no
// grant field of any kind, so the statement has to run somewhere, and dropping it silently would
// take a load-bearing property out of the product without failing anything.
//
// IT RUNS AS THE DATABASE'S OWNER, NOT AS A SUPERUSER, and that is what keeps the superuser DSN gone.
// A database's owner holds its privileges with grant option implicitly, so the owner can revoke
// PUBLIC's default CONNECT on its own database — the same credential the app itself is about to be
// handed, spent here for one statement and closed. No `GRANT CONNECT ... TO <role>` follows it: the
// owner's own access is not a grant that can be revoked away, and the object now declares that
// ownership rather than a statement asserting it.
//
// THE CONNECTION IS ALSO THE PROOF. Reaching the server with this credential is what says the
// password written into the Secret has actually been applied to the role — a rotation the operator
// reads out of a labelled Secret rather than out of the object's spec, and therefore the one part of
// an attach whose completion no `status.applied` covers. So a refusal is retried for a bounded
// window rather than failed on: a re-attach hands back a URL that has been used, or it fails loudly,
// and never hands back one that merely ought to work.
func (p *PostgresProvisioner) lockDownDatabase(ctx context.Context, dsn, app, env string) error {
	deadline := time.Now().Add(p.credentialTimeout)
	for {
		db, err := p.connect(ctx, dsn)
		if err == nil {
			// Idempotent: re-running it on a database already locked down is a no-op, which is what
			// makes it safe on every attach rather than only on the first.
			_, execErr := db.ExecContext(ctx, fmt.Sprintf("REVOKE CONNECT ON DATABASE %s FROM PUBLIC", quoteIdent(app)))
			db.Close()
			if execErr != nil {
				return fmt.Errorf("kube: revoking public connect on %s's database in environment %q: %w", app, env, execErr)
			}
			return nil
		}
		if time.Now().After(deadline) {
			// The error names the app and the environment; the DSN it could not open appears nowhere.
			return fmt.Errorf("kube: %s's own credential was not accepted by the %q environment's postgres instance within %s, so the connection string was not handed out — CloudNativePG applies a rewritten password from the Secret it is labelled %s on, and this is where that not happening surfaces: %w",
				app, env, p.credentialTimeout, cnpgReloadLabel, err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(p.pollInterval):
		}
	}
}

// ListAppDatabases returns the apps that hold a Burrow-provisioned database on environment env's
// instance, sorted (ADR-0031). It asks the instance itself rather than any registry, because the
// instance is
// the only place that knows: attach records the FACT of attachment nowhere but the app's own Secret
// and the databases on this server, and it is these databases — not a row somewhere — that a
// data-deleting add-on removal destroys.
//
// THIS IS THE ONE PATH THAT STILL OPENS A SUPERUSER CONNECTION, and it is worth saying why it was not
// moved onto the `Database` objects with the rest of provisioning (ADR-0066 §2). The objects would
// be a cheaper and more available answer to "who is attached", but they are a different answer to the
// question this method is actually asked: `addon remove --delete-data` names the apps whose data the
// volume is about to take with it (ADR-0064 §3), and a database a DETACH retained has no object left
// describing it. Listing objects would under-report exactly the databases a destructive confirmation
// most needs to name, and under-reporting is the dangerous direction for that message. Closing this
// last superuser read therefore needs the enumeration itself decided, not just re-plumbed.
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

// DropAppDatabase releases app's database and login role on environment env's instance: it deletes
// the two provisioning objects and the role's password Secret. Releasing something already absent is
// a no-op, not an error, so a detach of a half-finished attach can still finish.
//
// IT KEEPS THE DATA, AND IT RUNS NO SQL AT ALL. Both objects were written with a `retain` reclaim
// policy, so CloudNativePG releases them and leaves the database and the role exactly where they
// are; the app stops holding a credential for them (the engine removes the connection-string
// variable first) and a later attach of the same app adopts them again. There is no DROP anywhere on
// this path — not because nobody wrote one, but because expressing removal at all would mean either
// an `ensure: absent` on the object or a superuser connection, and ADR-0064 is a decision that the
// data outlives the thing that describes it.
//
// The credential does NOT survive: the password Secret goes, so the role's password is no longer
// anywhere in the cluster. A re-attach mints a new one and the adopted role takes it.
//
// The environment is required and unvalidated values are refused before anything is deleted, for the
// reason they always were: reaching another environment's server here would release a database that
// is still in use (ADR-0067 §1).
func (p *PostgresProvisioner) DropAppDatabase(ctx context.Context, app, env string) error {
	if err := validateAppIdentifier(app); err != nil {
		return err
	}
	target, err := p.targetFor(env)
	if err != nil {
		return err
	}
	databases, roles, err := p.cnpgObjects(target)
	if err != nil {
		return err
	}
	name := provisioningObjectName(target.Instance, app)

	// The `Database` first: it names the role as its owner, so releasing the role's description while
	// a database still points at it is the ordering with a window in it.
	if err := deleteCNPGObject(ctx, databases, cnpgDatabaseKind, name); err != nil {
		return err
	}
	if err := deleteCNPGObject(ctx, roles, cnpgDatabaseRoleKind, name); err != nil {
		return err
	}
	if err := p.client.CoreV1().Secrets(target.Namespace).Delete(ctx, name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("kube: deleting the role password secret %s/%s: %w", target.Namespace, name, err)
	}
	return nil
}

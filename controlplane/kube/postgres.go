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

// maxIdentifierLen is PostgreSQL's limit on an identifier: NAMEDATALEN (64) less the terminator.
// Exceeding it is NOT an error on the server — the name is silently TRUNCATED to this many bytes,
// which is what turns a length into a correctness problem instead of a cosmetic one.
const maxIdentifierLen = 63

// The two decorations an attach puts around an app name to derive the roles below. They are named
// rather than inlined because MaxPostgresAppNameLen is composed from them: a budget written out of
// the same constants as the names it budgets for cannot drift away from them.
const (
	rolePrefix     = "app_"
	dataRoleSuffix = "_data"
)

// MaxPostgresAppNameLen is the longest app name that an attach can serve: 63 − len("app_") −
// len("_data") = 54. It is the length at which EVERY identifier an attach derives still fits inside
// maxIdentifierLen, so nothing the server is handed is ever truncated.
//
// THE BINDING NAME IS THE DATA ROLE, not the database and not the login role. One app name yields a
// database called <app>, a login role called app_<app>, and a login-less data role called
// app_<app>_data, and the last is nine bytes longer than the first — so budgeting against either of
// the other two admits names whose data role silently truncates (issue #534).
//
// Truncation there is not a cosmetic clash. Past 58 bytes of app_<app> the suffix is the part that
// gets cut, and once app_<app> itself reaches 63 the data role's name truncates onto it EXACTLY: the
// two roles the whole of ADR-0090 §1 rests on become one role, which must then be both able and
// unable to log in.
// Whichever disposition the operator reconciles last is the one that sticks, so either the app's
// credential stops authenticating at attach or the role that holds the data after a detach can log
// in with the password that detach was supposed to revoke.
//
// So the budget FAILS CLOSED and is checked at the point of naming, never worked around by
// truncating or hashing the derived name. A name that has to be shortened is a collision that
// depends on how long two names are rather than on what they are, which is materially harder to
// diagnose than one refused at creation — and shortening only moves the collision, since two apps
// whose names share a long enough prefix would then share a data role. Refusing is also what keeps
// the derived names of every name that IS admitted exactly what they have always been.
const MaxPostgresAppNameLen = maxIdentifierLen - len(rolePrefix) - len(dataRoleSuffix)

// validatePostgresAppName is validateAppIdentifier plus the identifier budget above: the check an
// app name must pass before a database is provisioned FOR it.
//
// It gates the ATTACH PATH ONLY, and that asymmetry is deliberate. An attachment provisioned by an
// earlier release under a longer name still has its database, its roles and its objects under
// exactly the names they were created with, and a detach — plain or `--delete-data` — has to be able
// to reach them. Refusing a length on the teardown path would strand precisely the attachments this
// check exists to stop anyone from making, with no way left to take them apart.
func validatePostgresAppName(app string) error {
	if err := validateAppIdentifier(app); err != nil {
		return err
	}
	if len(app) > MaxPostgresAppNameLen {
		return fmt.Errorf("app name %q is %d characters, and a postgres attachment can serve at most %d: it derives a login role %s<app> and a login-less %s<app>%s that holds the data across a detach, and PostgreSQL truncates an identifier at %d bytes — past this length those two names truncate onto one role, which cannot be both able and unable to log in. Attach the database to an app with a shorter name: %w",
			app, len(app), MaxPostgresAppNameLen, rolePrefix, rolePrefix, dataRoleSuffix, maxIdentifierLen, controlplane.ErrInvalid)
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

// ReadHost is the in-cluster DNS name of the target's read-only Service — CloudNativePG's `-ro`,
// which selects the STANDBYS and nothing else (ADR-0081 §2). It is what goes into the read address
// of an app attached to an instance that has one.
//
// `-ro` rather than `-r`, and the difference is the whole point of splitting reads: `-r` selects any
// instance, so a query down it may land on the primary. `-ro` resolves to no endpoint at all on a
// standby-less instance, which is why nothing writes this address unless a standby exists.
//
// It is composed from the target rather than from the `Cluster`'s status, so it names a Service in
// the same namespace, under the same instance name, as Host() does — the two cannot come to describe
// different servers.
func (t PostgresTarget) ReadHost() string {
	return fmt.Sprintf("%s-ro.%s.svc", t.Instance, t.Namespace)
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
//
// IT TAKES THE INSTANCE AS WELL AS THE ENVIRONMENT (ADR-0091 §4). An environment may hold more than
// one instance, so the environment alone no longer picks a server: the instance is the name the
// registry holds for the one this operation acts on, and the environment is what says whose data it
// is. Both are required.
type PostgresTargetFunc func(env, instance string) (PostgresTarget, error)

// AddonInstanceTarget is the target of a single-tenant install — the one an existing install already
// has, and the value burrowd is wired with: the named instance, in the add-on namespace burrowd was
// installed with. An empty namespace means the default one, so an install that never set
// BURROW_ADDON_NAMESPACE names exactly the instance, Secret, and host it has always used.
//
// It composes NO name of its own. The instance arrives from the caller, which resolved it out of the
// registry (ADR-0091 §2); what this adds is the namespace, which is configuration rather than
// derivation (issue #519).
func AddonInstanceTarget(addonNamespace string) PostgresTargetFunc {
	if addonNamespace == "" {
		addonNamespace = defaultAddonNamespace
	}
	return func(env, instance string) (PostgresTarget, error) {
		if env == "" {
			return PostgresTarget{}, fmt.Errorf("kube: postgres target: no environment named: %w", controlplane.ErrInvalid)
		}
		// The INSTANCE is given, not derived. It is the name the registry holds (ADR-0091 §2), and a
		// caller that names none reaches no instance rather than the one a derivation would have
		// picked — which is the same failure issue #519 closed, one level down.
		if instance == "" {
			return PostgresTarget{}, fmt.Errorf("kube: postgres target for environment %q: no instance named: %w", env, controlplane.ErrInvalid)
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
// A FEW STATEMENTS SURVIVE, all deliberately, and none of them is a superuser one on the attach or
// the detach path. A `Database` cannot express a grant, so the `REVOKE CONNECT ... FROM PUBLIC` that
// keeps one app's role out of another app's database runs on a connection opened as the database's
// own OWNER (lockDownDatabase); and it cannot express who owns the TABLES inside it either, so the
// reassignment that lets a detach drop the app's role while keeping its rows runs on the same
// connection (releaseWhatTheRoleOwns). ListAppDatabases still asks the server, because the question
// it answers — whose data is in this volume — is one the objects cannot answer about a database a
// detach retained.
//
// THE TWO THAT PROVISION ARE A SEAM, NOT A REQUIREMENT TO SHARE A NETWORK WITH THE DATABASE (issue
// #532). Each is described as a PostgresConnectedStep and handed to a runner that by default opens
// the connection here and that an embedder may replace — so a control plane which reaches its
// instances only through an API server runs them as a Job inside that cluster, while this file
// remains the single answer to WHAT an attachment consists of. A self-hosted install configures
// nothing and is unaffected.
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
	// connected is how the steps that need a SQL connection are performed. Nil — which is every
	// self-hosted install — means opening one here (runConnectedStep). It is the seam an embedder
	// whose control plane has no route to the instance substitutes; see WithConnectedStep.
	connected PostgresConnectedStepFunc
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
func (p *PostgresProvisioner) targetFor(env, instance string) (PostgresTarget, error) {
	if p.target == nil {
		return PostgresTarget{}, fmt.Errorf("kube: the postgres provisioner was given no instance to act on: %w", controlplane.ErrInvalid)
	}
	return p.target(env, instance)
}

// instanceHost is the host environment env's Postgres instance is reached at. This is what goes into
// that environment's apps' DATABASE_URLs — apps are pods and resolve it through cluster DNS. The
// environment selects one of the instances the provisioner was configured with, so the URL an app is
// handed names the server its own environment runs.
func (p *PostgresProvisioner) instanceHost(env, instance string) (string, error) {
	target, err := p.targetFor(env, instance)
	if err != nil {
		return "", err
	}
	return target.Host(), nil
}

// dialHostPort is the host:port the provisioner DIALS for env: the test override if set, otherwise
// the in-cluster Service address. Distinct from instanceHost so the override never leaks into an
// app's DATABASE_URL.
func (p *PostgresProvisioner) dialHostPort(env, instance string) (string, error) {
	if p.instanceEndpoint != "" {
		return p.instanceEndpoint, nil
	}
	host, err := p.instanceHost(env, instance)
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
func (p *PostgresProvisioner) superuserPassword(ctx context.Context, env, instance string) (string, error) {
	target, err := p.targetFor(env, instance)
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
func (p *PostgresProvisioner) connectAdmin(ctx context.Context, env, instance, database string) (sqlConn, error) {
	hostPort, err := p.dialHostPort(env, instance)
	if err != nil {
		return nil, err
	}
	pw, err := p.superuserPassword(ctx, env, instance)
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
func roleName(app string) string { return rolePrefix + app }

// dataRoleName is the login-less role that OWNS app's data: app_<app>_data. It is derived from the
// login role's name rather than composed separately, so the pair reads as one app's two roles in a
// role listing and in ListAppDatabases' ownership query, which matches both.
//
// It is the LONGEST identifier an attachment has, five bytes past the login role's, and that is what
// MaxPostgresAppNameLen is budgeted against — see there for what a truncated one costs. Nothing here
// shortens: an app name the attach path admits produces a name that fits, and a name that does not
// fit was refused before any of this was derived. The teardown paths still derive both names for an
// app the attach path would now refuse, which is the point: they name whatever an earlier release
// created, and the server truncates it exactly as it did then.
func dataRoleName(app string) string { return roleName(app) + dataRoleSuffix }

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
// The sequence is five steps and the order is load-bearing:
//
//  1. The role's password Secret, BEFORE the role that references it — a `DatabaseRole` pointing at
//     a Secret that does not exist yet is the operator racing the control plane into a failure.
//  2. The DATA ROLE, before the login role that names it in `inRoles` — a membership can only be
//     granted in a role the server already has.
//  3. The login `DatabaseRole`, before the `Database` that names it as owner.
//  4. The `Database`.
//  5. A wait on all three, because provisioning is asynchronous now: `status.applied` is the only
//     thing that says the server has actually got them.
//
// AN EXISTING INSTALL IS ADOPTED, NOT RE-PROVISIONED. An app attached before this existed has its
// database and role already; the objects written here describe them rather than ask for them again,
// and CloudNativePG reconciles what is there. That is the same idempotence a re-attach has always
// relied on, moved from an existence check in this function into the operator (applyCNPGObject).
//
// A DATABASE A PREVIOUS DETACH RETAINED IS ADOPTED BY THE SAME MECHANISM, which is what makes detach
// reversible (ADR-0090 §3). That database is there, holding its rows, owned by the data role; this
// call hands the ownership back to a freshly created login role and the app reads what it left. It
// does not matter that the old role's tables outlived the old role: they are the data role's now,
// and the new login role is a member of it, so PostgreSQL resolves every owner check in its favour.
//
// Idempotence is what made the missing environment dangerous rather than merely wrong (issue #339):
// finding an existing database is the NORMAL case of a re-attach, so a second environment's attach
// did not fail — it adopted the first environment's database and rotated its password. With the
// environment selecting the instance, the only database this can find is one on that environment's
// own server, and adopting it is again exactly what a re-attach should do.
//
// A NAME TOO LONG TO SERVE IS REFUSED HERE, before anything is written (MaxPostgresAppNameLen). This
// is the only place it can be refused honestly: the length is not a property of the app, it is a
// property of the identifiers an attach derives, so the verb that derives them is the verb that
// knows. Refusing at detach instead would mean the attach succeeded, the two roles collapsed into
// one, and the failure surfaced only when the app's access was supposed to end.
func (p *PostgresProvisioner) EnsureAppDatabase(ctx context.Context, app, env, instance string) (string, error) {
	if err := validatePostgresAppName(app); err != nil {
		return "", err
	}
	// The environment is resolved to an instance BEFORE anything is written: an empty or malformed
	// environment is ErrInvalid here, never a silent fallback to whichever instance exists.
	target, err := p.targetFor(env, instance)
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
	labels := attachmentLabels(env)
	// The Secret carries one label the objects do not, and it is functional rather than descriptive:
	// it is what makes CloudNativePG notice a rewritten password.
	secretLabels := map[string]string{cnpgReloadLabel: "true"}
	for k, v := range labels {
		secretLabels[k] = v
	}

	if err := p.ensureRolePasswordSecret(ctx, target, app, password, secretLabels); err != nil {
		return "", err
	}
	dataName := dataRoleObjectName(target.Instance, app)
	if err := applyCNPGObject(ctx, roles, cnpgDatabaseRoleKind, dataName, target.Namespace, dataRoleSpec(target, app), labels); err != nil {
		return "", err
	}
	// Waited on before the login role is even written: `inRoles` becomes a `GRANT`, and a grant of a
	// role the server does not have yet is an object the operator refuses and retries.
	if err := awaitCNPGApplied(ctx, roles, cnpgDatabaseRoleKind, dataName, p.applyTimeout, p.pollInterval); err != nil {
		return "", fmt.Errorf("kube: provisioning the data role for %s in environment %q: %w", app, env, err)
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

	if err := p.lockDownDatabase(ctx, target, app, env, password); err != nil {
		return "", err
	}
	return appDSN(role, password, target.Host()+":5432", app), nil
}

// AppReadURL composes app's READ-ONLY connection string on environment env's instance — ADR-0081
// §2's read address, pointing at the `-ro` Service that selects standbys and nothing else.
//
// IT PROVISIONS NOTHING AND ROTATES NOTHING, which is the difference between it and a second call to
// EnsureAppDatabase. The role, its password and the database already exist; the only thing that
// differs from the string the attach handed out is the host. Rotating here would break every running
// app the moment a standby was added, which is the operation this exists to serve.
//
// The password is read back out of the Secret this provisioner wrote for the operator to apply, so
// there is no second credential and nothing new to keep in step: the read address stops working at
// exactly the moment the primary one does, which is what it should do. The returned string is a
// SECRET value and appears in no error raised here.
//
// It does not check whether a standby exists. Whether an instance should have a read address is the
// engine's decision (ADR-0081 §2); an implementation that second-guessed it would be a second
// opinion about a fact the engine has already read off the `Cluster`.
func (p *PostgresProvisioner) AppReadURL(ctx context.Context, app, env, instance string) (string, error) {
	if err := validatePostgresAppName(app); err != nil {
		return "", err
	}
	target, err := p.targetFor(env, instance)
	if err != nil {
		return "", err
	}
	name := provisioningObjectName(target.Instance, app)
	s, err := p.client.CoreV1().Secrets(target.Namespace).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return "", fmt.Errorf("kube: %s has no provisioned credential on environment %q's instance, so it has no read address either — attach it first: %w", app, env, controlplane.ErrNotFound)
	}
	if err != nil {
		return "", fmt.Errorf("kube: reading the role password secret %s/%s: %w", target.Namespace, name, err)
	}
	password, ok := s.Data[PostgresPasswordKey]
	if !ok {
		return "", fmt.Errorf("kube: the role password secret %s/%s has no %q key: %w", target.Namespace, name, PostgresPasswordKey, controlplane.ErrNotFound)
	}
	return appDSN(roleName(app), string(password), target.ReadHost()+":5432", app), nil
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
//
// IT DESCRIBES THE STEP AND HANDS IT TO A RUNNER rather than opening the connection itself, which is
// what lets an embedder with no route to the instance perform it somewhere else without owning any
// of the above (PostgresConnectedStep). The statement, the credential it runs as, and the moment it
// runs at stay here; only the mechanics move.
func (p *PostgresProvisioner) lockDownDatabase(ctx context.Context, target PostgresTarget, app, env, password string) error {
	// Idempotent: re-running it on a database already locked down is a no-op, which is what makes it
	// safe on every attach rather than only on the first.
	step := p.connectedStep(PostgresRevokePublicConnect, target, app, env, password,
		fmt.Sprintf("REVOKE CONNECT ON DATABASE %s FROM PUBLIC", quoteIdent(app)))
	if err := p.runConnectedStep(ctx, step); err != nil {
		return fmt.Errorf("kube: revoking public connect on %s's database in environment %q: %w", app, env, err)
	}
	return nil
}

// connectAsApp opens a connection with the app's OWN credential, retrying until the server accepts
// it or credentialTimeout expires. It serves the default step runner, and every step it runs holds a
// password just written into the Secret CloudNativePG applies to the role, so reaching the server is
// the only proof it landed — see lockDownDatabase for why no `status.applied` covers that. A
// substituted runner owes the same wait, which is why the step carries the window
// (PostgresConnectedStep.CredentialTimeout). The DSN is a secret value and appears in no error
// raised here.
func (p *PostgresProvisioner) connectAsApp(ctx context.Context, dsn, app, env string) (sqlConn, error) {
	deadline := time.Now().Add(p.credentialTimeout)
	for {
		db, err := p.connect(ctx, dsn)
		if err == nil {
			return db, nil
		}
		if time.Now().After(deadline) {
			// The error names the app and the environment; the DSN it could not open appears nowhere.
			return nil, fmt.Errorf("kube: %s's own credential was not accepted by the %q environment's postgres instance within %s — CloudNativePG applies a rewritten password from the Secret it is labelled %s on, and this is where that not happening surfaces: %w",
				app, env, p.credentialTimeout, cnpgReloadLabel, err)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
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
// roles an attach creates is a provisioned app database, and its name is the app's name. That
// excludes the maintenance databases (postgres, template0/template1, all owned by the superuser) and
// anything a human created by hand as the superuser, without needing a naming convention to hold.
//
// THE PREFIX MATCHES BOTH OF AN APP'S ROLES, and that is why a database a plain detach retained is
// still reported. A detach hands the database to the app's DATA role (app_<app>_data) so that the
// login role can be dropped, and the prefix covers it — which keeps a retained database visible to
// the confirmation that is about to destroy it. Handing it to the superuser instead would have made
// exactly the unattached databases invisible here.
func (p *PostgresProvisioner) ListAppDatabases(ctx context.Context, env, instance string) ([]string, error) {
	db, err := p.connectAdmin(ctx, env, instance, "postgres")
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

// RevokeAppDatabase drops app's login role on environment env's instance and KEEPS its database,
// with its rows (ADR-0090 §1). It is what a plain `addon detach` performs: the app's access ends and
// its data does not, so a re-attach adopts what is there (EnsureAppDatabase) and the pair is
// reversible. Revoking something already absent is a no-op, not an error.
//
// WHAT IT HAS TO SOLVE IS AN OWNERSHIP PROBLEM, not a deletion one. `DROP ROLE` refuses while the
// role owns anything, and the app's login role owns its database and every table in it — so "drop
// the role, keep the data" is a statement PostgreSQL rejects until somebody else is holding the
// ownership. The data role (dataRoleSpec) is that somebody, and this is the sequence that hands over:
//
//  1. The DATA ROLE, so there is a role to hand the ownership to.
//  2. The LOGIN ROLE, described one last time with a fresh password and its membership of the data
//     role restated, holding a `delete` reclaim policy — the credential this call is about to spend,
//     and the policy that makes step 6 a `DROP ROLE`.
//  3. The DATABASE, re-owned by the data role and left on `retain`. It goes BEFORE the reassignment
//     below: leave the object naming the login role and the operator reconciles the ownership
//     straight back, and the drop in step 6 fails on a database the role owns again.
//  4. A connection as the app's OWN role — the same credential and the same proof-of-rotation wait
//     the attach path uses, and the same reason no superuser appears on either.
//  5. `REASSIGN OWNED` then `DROP OWNED`, which is PostgreSQL's own recipe for emptying a role before
//     dropping it: the first moves every object the app created to the data role, the second releases
//     the privileges and default ACLs that a `DROP ROLE` would otherwise still trip over. Both are
//     run as the app itself, which is permitted because it holds the privileges of both roles — its
//     own, and the data role's through the membership.
//  6. The login role's object, deleted and waited on. The finalizer being released is the only
//     evidence the `DROP ROLE` ran, which is the whole claim this call makes.
//  7. The password Secret, once there is no role left for it to open anything as.
//
// The environment is required and unvalidated values are refused before anything is written, for the
// reason they always are: a detach that reached another environment's server would take an app's
// access to a database that is still in use (ADR-0067 §1).
func (p *PostgresProvisioner) RevokeAppDatabase(ctx context.Context, app, env, instance string) error {
	if err := validateAppIdentifier(app); err != nil {
		return err
	}
	target, err := p.targetFor(env, instance)
	if err != nil {
		return err
	}
	databases, roles, err := p.cnpgObjects(target)
	if err != nil {
		return err
	}
	password, err := generatePassword()
	if err != nil {
		return err
	}
	name := provisioningObjectName(target.Instance, app)
	dataName := dataRoleObjectName(target.Instance, app)
	labels := attachmentLabels(env)
	secretLabels := map[string]string{cnpgReloadLabel: "true"}
	for k, v := range labels {
		secretLabels[k] = v
	}

	if err := applyCNPGObject(ctx, roles, cnpgDatabaseRoleKind, dataName, target.Namespace, dataRoleSpec(target, app), labels); err != nil {
		return err
	}
	if err := awaitCNPGApplied(ctx, roles, cnpgDatabaseRoleKind, dataName, p.applyTimeout, p.pollInterval); err != nil {
		return fmt.Errorf("kube: provisioning the role that keeps %s's data in environment %q: %w", app, env, err)
	}
	// The Secret before the object that references it, exactly as on the attach path.
	if err := p.ensureRolePasswordSecret(ctx, target, app, password, secretLabels); err != nil {
		return err
	}
	if err := applyCNPGObject(ctx, roles, cnpgDatabaseRoleKind, name, target.Namespace, databaseRoleRevokeSpec(target, app), labels); err != nil {
		return err
	}
	if err := awaitCNPGApplied(ctx, roles, cnpgDatabaseRoleKind, name, p.applyTimeout, p.pollInterval); err != nil {
		return fmt.Errorf("kube: describing %s's login role in environment %q before dropping it: %w", app, env, err)
	}
	if err := applyCNPGObject(ctx, databases, cnpgDatabaseKind, name, target.Namespace, databaseRetainedSpec(target, app), labels); err != nil {
		return err
	}
	if err := awaitCNPGApplied(ctx, databases, cnpgDatabaseKind, name, p.applyTimeout, p.pollInterval); err != nil {
		return fmt.Errorf("kube: handing %s's database in environment %q to the role that keeps it: %w", app, env, err)
	}

	if err := p.releaseWhatTheRoleOwns(ctx, target, app, env, password); err != nil {
		return err
	}
	if err := deleteCNPGObject(ctx, roles, cnpgDatabaseRoleKind, name, p.applyTimeout, p.pollInterval); err != nil {
		return fmt.Errorf("kube: dropping the login role for %s in environment %q: %w", app, env, err)
	}
	if err := p.client.CoreV1().Secrets(target.Namespace).Delete(ctx, name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("kube: deleting the role password secret %s/%s: %w", target.Namespace, name, err)
	}
	return nil
}

// releaseWhatTheRoleOwns empties app's login role so that dropping it succeeds and its data survives
// — the two statements a plain detach runs, over the app's own credential (ADR-0090 §1).
//
// NEITHER IS EXPRESSIBLE AS AN OBJECT. The `Database` CRD carries an owner for the database and
// nothing at all for the tables inside it, and `DatabaseRole` has no notion of what a role owns. So
// these run where the `REVOKE CONNECT` runs, as the app's own role, for the same reason: it is the
// least-privileged credential that can issue them, and the alternative is a superuser DSN on the
// teardown path that the attach path no longer needs (ADR-0066 §2).
//
// `REASSIGN OWNED` moves every object the app created to the data role. `DROP OWNED` then releases
// what reassignment does not touch — the privileges granted to the role and the default ACLs it
// carries — and it is safe to run second precisely because the first left the role owning nothing:
// run alone it would DESTROY the tables rather than re-home them. Both are idempotent, so a detach
// re-run after a failure finishes rather than compounding.
//
// THE ORDER OF THE TWO IS PART OF THE STEP, not of whoever performs it. They are handed over as one
// step for that reason: an embedder that received them separately would be the one deciding that
// `DROP OWNED` follows `REASSIGN OWNED`, and getting that wrong destroys the rows a plain detach
// exists to keep.
func (p *PostgresProvisioner) releaseWhatTheRoleOwns(ctx context.Context, target PostgresTarget, app, env, password string) error {
	role, data := quoteIdent(roleName(app)), quoteIdent(dataRoleName(app))
	step := p.connectedStep(PostgresReleaseOwnedObjects, target, app, env, password,
		fmt.Sprintf("REASSIGN OWNED BY %s TO %s", role, data),
		fmt.Sprintf("DROP OWNED BY %s", role))
	if err := p.runConnectedStep(ctx, step); err != nil {
		return fmt.Errorf("kube: releasing what %s's login role owns in environment %q, so that dropping it keeps the data: %w", app, env, err)
	}
	return nil
}

// DropAppDatabase drops app's database and both of its roles from environment env's instance — what
// `addon detach --delete-data` performs, and the ONLY path in the product that destroys an app's rows
// (ADR-0090 §2). Dropping something already absent is a no-op, not an error, so a detach of a
// half-finished attach can still finish.
//
// IT DESTROYS THE DATA THROUGH THE OBJECTS. Each is described one last time with a `delete` reclaim
// policy and then deleted; CloudNativePG runs the DROPs. There is no superuser connection here any
// more than there is on the attach path, and no statement of Burrow's at all.
//
// The reclaim policy is FLIPPED HERE rather than written at attach, and that asymmetry is the safety
// property: an object that goes away for any reason other than this call — a namespace teardown, a
// collector, somebody tidying with kubectl — takes nothing with it, because the policy it is holding
// at that moment says `retain`. Only an explicit ask to destroy the data ever writes `delete` on the
// database, immediately before deleting the thing it wrote it on.
//
// The objects are ENSURED before they are destroyed, and that is not redundant. An app attached
// before provisioning became declarative has a database and a role and no objects describing them;
// without this, destroying it would delete nothing and report success, leaving the data of an app
// somebody believed they had scrubbed. Describing it first is what makes the destructive path reach
// exactly the databases the old admin SQL did.
//
// THE ORDER IS DICTATED BY POSTGRESQL. `allowConnections: false` goes on before anything is deleted,
// because the operator issues a plain `DROP DATABASE IF EXISTS` and a single live session refuses it
// — where the admin SQL this replaced used `WITH (FORCE)`. Then the database, then the roles, each
// waited on: PostgreSQL will not drop a role that still owns a database, so releasing either role's
// description before the database is gone is a DROP ROLE that fails forever.
//
// The environment is required and unvalidated values are refused before anything is written, for the
// reason they always were: this is the destructive half of the pair, so reaching another
// environment's server here would drop a database that is still in use (ADR-0067 §1).
func (p *PostgresProvisioner) DropAppDatabase(ctx context.Context, app, env, instance string) error {
	if err := validateAppIdentifier(app); err != nil {
		return err
	}
	target, err := p.targetFor(env, instance)
	if err != nil {
		return err
	}
	databases, roles, err := p.cnpgObjects(target)
	if err != nil {
		return err
	}
	name := provisioningObjectName(target.Instance, app)
	labels := attachmentLabels(env)

	// The role's description first and waited on, because the database below names it as owner: a
	// `Database` whose owner the operator has not reconciled yet is an object it refuses and retries,
	// which turns an ordering detail into seconds of an attach-shaped error in the log.
	if err := applyCNPGObject(ctx, roles, cnpgDatabaseRoleKind, name, target.Namespace, databaseRoleTeardownSpec(target, app), labels); err != nil {
		return err
	}
	if err := awaitCNPGApplied(ctx, roles, cnpgDatabaseRoleKind, name, p.applyTimeout, p.pollInterval); err != nil {
		return fmt.Errorf("kube: describing %s's login role in environment %q before dropping it: %w", app, env, err)
	}
	if err := applyCNPGObject(ctx, databases, cnpgDatabaseKind, name, target.Namespace, databaseTeardownSpec(target, app), labels); err != nil {
		return err
	}
	// Waiting for the closed door specifically: until the operator has applied it, a reconnect is
	// still possible and the drop below would be racing whatever did it.
	if err := awaitCNPGApplied(ctx, databases, cnpgDatabaseKind, name, p.applyTimeout, p.pollInterval); err != nil {
		return fmt.Errorf("kube: closing %s's database to new connections in environment %q before dropping it: %w", app, env, err)
	}
	if err := deleteCNPGObject(ctx, databases, cnpgDatabaseKind, name, p.applyTimeout, p.pollInterval); err != nil {
		return fmt.Errorf("kube: dropping the database for %s in environment %q: %w", app, env, err)
	}
	if err := deleteCNPGObject(ctx, roles, cnpgDatabaseRoleKind, name, p.applyTimeout, p.pollInterval); err != nil {
		return fmt.Errorf("kube: dropping the login role for %s in environment %q: %w", app, env, err)
	}
	// The data role goes with the database it existed to hold, and only if something ever created it:
	// an attachment made before it existed has no such role, and writing one here would create a role
	// for the sole purpose of dropping it. It goes LAST for the same reason the login role does not
	// go first — a detach may have left it owning the database that has just been dropped.
	if err := destroyCNPGObjectIfPresent(ctx, roles, cnpgDatabaseRoleKind, dataRoleObjectName(target.Instance, app), target.Namespace, dataRoleTeardownSpec(target, app), labels, p.applyTimeout, p.pollInterval); err != nil {
		return fmt.Errorf("kube: dropping the role that kept %s's data in environment %q: %w", app, env, err)
	}
	// The credential last, once there is nothing left for it to open.
	if err := p.client.CoreV1().Secrets(target.Namespace).Delete(ctx, name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("kube: deleting the role password secret %s/%s: %w", target.Namespace, name, err)
	}
	return nil
}

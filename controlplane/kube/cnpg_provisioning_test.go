// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package kube

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/burrow-cloud/burrow/controlplane"
)

// sqlRecorder is the substituted connection opener. It records every DSN opened and every statement
// run, which is what lets these tests assert the two things this change is about: that provisioning
// opens NO connection at all, and that the one statement an attach still runs is run as the app's
// own role rather than as the superuser.
type sqlRecorder struct {
	mu sync.Mutex
	// dsns is every connection string opened, in order.
	dsns []string
	// statements is every statement executed, in order.
	statements []string
	// refusals is how many opens still fail before one succeeds, modelling a rotated password the
	// operator has not applied to the running server yet.
	refusals int
}

func (r *sqlRecorder) open(_ context.Context, dsn string) (sqlConn, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.dsns = append(r.dsns, dsn)
	if r.refusals > 0 {
		r.refusals--
		return nil, errors.New(`password authentication failed for user "app_web"`)
	}
	return &recordedConn{rec: r}, nil
}

// users returns the role each recorded connection authenticated as, so a test can say "no superuser
// connection was opened" about a value rather than about an absence of code.
func (r *sqlRecorder) users() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, len(r.dsns))
	for _, dsn := range r.dsns {
		u, err := url.Parse(dsn)
		if err != nil || u.User == nil {
			out = append(out, "")
			continue
		}
		out = append(out, u.User.Username())
	}
	return out
}

type recordedConn struct{ rec *sqlRecorder }

func (c *recordedConn) ExecContext(_ context.Context, query string, _ ...any) (sql.Result, error) {
	c.rec.mu.Lock()
	defer c.rec.mu.Unlock()
	c.rec.statements = append(c.rec.statements, query)
	return nil, nil
}

func (c *recordedConn) QueryContext(_ context.Context, query string, _ ...any) (*sql.Rows, error) {
	c.rec.mu.Lock()
	c.rec.statements = append(c.rec.statements, query)
	c.rec.mu.Unlock()
	return nil, errors.New("this test's opener returns no rows")
}

func (c *recordedConn) Close() error { return nil }

// provisioningGVRs is the two resources the provisioner writes, spelled again here rather than
// exported: a test that names the resource independently is what notices the day the provisioner
// starts writing a different one.
var provisioningGVRs = map[schema.GroupVersionResource]string{
	{Group: CNPGAPIGroup, Version: "v1", Resource: "databases"}:     "DatabaseList",
	{Group: CNPGAPIGroup, Version: "v1", Resource: "databaseroles"}: "DatabaseRoleList",
}

// provisionerFor builds a provisioner over fake clients, with the waits short enough that a test
// does not spend them and a recorded opener in place of a real database.
//
// The dynamic client stamps `status.applied: true` onto every read, which is CloudNativePG's job on
// a real cluster. Without it the fakes model an operator that never reconciles anything, and every
// attach would (correctly) time out.
func provisionerFor(t *testing.T, ns string, objects ...runtime.Object) (*PostgresProvisioner, *dynamicfake.FakeDynamicClient, *sqlRecorder) {
	t.Helper()
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), provisioningGVRs)
	for gvr := range provisioningGVRs {
		gvr := gvr
		dyn.PrependReactor("get", gvr.Resource, func(action k8stesting.Action) (bool, runtime.Object, error) {
			get := action.(k8stesting.GetAction)
			obj, err := dyn.Tracker().Get(gvr, get.GetNamespace(), get.GetName())
			if err != nil {
				return true, nil, err
			}
			u := obj.(*unstructured.Unstructured).DeepCopy()
			if err := unstructured.SetNestedField(u.Object, true, "status", "applied"); err != nil {
				return true, nil, err
			}
			return true, u, nil
		})
	}
	rec := &sqlRecorder{}
	p := NewPostgresProvisioner(fake.NewSimpleClientset(objects...), dyn, AddonInstanceTarget(ns))
	p.open = rec.open
	p.applyTimeout = time.Second
	p.credentialTimeout = time.Second
	p.pollInterval = time.Millisecond
	return p, dyn, rec
}

// deletionRecorder captures what each provisioning object SAID at the moment it was deleted, and in
// what order. A teardown's whole effect is carried by the spec the object is holding when it goes —
// after which there is nothing left to read — so it has to be observed on the way past.
type deletionRecorder struct {
	mu    sync.Mutex
	specs map[schema.GroupVersionResource]map[string]any
	order []schema.GroupVersionResource
}

func (d *deletionRecorder) spec(gvr schema.GroupVersionResource) (map[string]any, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	s, ok := d.specs[gvr]
	return s, ok
}

// recordDeletions installs a reactor per provisioning resource that reads the object out of the
// tracker before the delete goes through, then lets the delete proceed.
func recordDeletions(dyn *dynamicfake.FakeDynamicClient) *deletionRecorder {
	rec := &deletionRecorder{specs: map[schema.GroupVersionResource]map[string]any{}}
	for gvr := range provisioningGVRs {
		gvr := gvr
		dyn.PrependReactor("delete", gvr.Resource, func(action k8stesting.Action) (bool, runtime.Object, error) {
			del := action.(k8stesting.DeleteAction)
			if obj, err := dyn.Tracker().Get(gvr, del.GetNamespace(), del.GetName()); err == nil {
				u := obj.(*unstructured.Unstructured)
				rec.mu.Lock()
				rec.specs[gvr] = u.DeepCopy().Object
				rec.order = append(rec.order, gvr)
				rec.mu.Unlock()
			}
			return false, nil, nil // fall through to the tracker's own delete
		})
	}
	return rec
}

// nestedString reads a string out of an unstructured object, failing the test if it is not there.
func nestedString(t *testing.T, obj map[string]any, fields ...string) string {
	t.Helper()
	v, found, err := unstructured.NestedString(obj, fields...)
	if err != nil || !found {
		t.Fatalf("%v is not set on the object", fields)
	}
	return v
}

// getObject reads one provisioning object, failing the test if it is absent.
func getObject(t *testing.T, dyn dynamic.Interface, gvr schema.GroupVersionResource, ns, name string) *unstructured.Unstructured {
	t.Helper()
	u, err := dyn.Resource(gvr).Namespace(ns).Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("reading %s %s/%s: %v", gvr.Resource, ns, name, err)
	}
	return u
}

// TestAttachProvisionsThroughObjectsAndOpensNoSuperuserConnection is the central claim of issue #520.
// An attach writes a `Database` and a `DatabaseRole` describing exactly what ADR-0031 promises, and
// the only connection it opens is the app's own — burrowd never holds a superuser DSN to provision.
func TestAttachProvisionsThroughObjectsAndOpensNoSuperuserConnection(t *testing.T) {
	ctx := context.Background()
	p, dyn, rec := provisionerFor(t, addonNS)

	dsn, err := p.EnsureAppDatabase(ctx, "web", controlplane.DefaultEnvironment)
	if err != nil {
		t.Fatalf("EnsureAppDatabase: %v", err)
	}

	name := provisioningObjectName(PostgresSecretName, "web")
	db := getObject(t, dyn, cnpgDatabaseGVR, addonNS, name)
	if got := nestedString(t, db.Object, "spec", "name"); got != "web" {
		t.Errorf("Database names the database %q, want web", got)
	}
	if got := nestedString(t, db.Object, "spec", "owner"); got != "app_web" {
		t.Errorf("Database owner = %q, want app_web", got)
	}
	if got := nestedString(t, db.Object, "spec", "cluster", "name"); got != PostgresSecretName {
		t.Errorf("Database names cluster %q, want %s", got, PostgresSecretName)
	}
	if got := nestedString(t, db.Object, "spec", "ensure"); got != cnpgEnsurePresent {
		t.Errorf("Database ensure = %q, want %s", got, cnpgEnsurePresent)
	}

	role := getObject(t, dyn, cnpgDatabaseRoleGVR, addonNS, name)
	if got := nestedString(t, role.Object, "spec", "name"); got != "app_web" {
		t.Errorf("DatabaseRole names the role %q, want app_web", got)
	}
	if login, _, _ := unstructured.NestedBool(role.Object, "spec", "login"); !login {
		t.Error("DatabaseRole does not grant LOGIN, so the app could not connect with it")
	}
	if got := nestedString(t, role.Object, "spec", "passwordSecret", "name"); got != name {
		t.Errorf("DatabaseRole reads its password from %q, want %s", got, name)
	}
	// Nothing the role does not need: an app's role owns its own database and administers nothing.
	for _, attr := range []string{"superuser", "createdb", "createrole"} {
		if v, found, _ := unstructured.NestedBool(role.Object, "spec", attr); found && v {
			t.Errorf("DatabaseRole grants %s", attr)
		}
	}

	// The credential the operator applies, in the shape the reference requires.
	sec, err := p.client.CoreV1().Secrets(addonNS).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("the role's password secret: %v", err)
	}
	if sec.Type != corev1.SecretTypeBasicAuth {
		t.Errorf("password secret type = %s, want %s", sec.Type, corev1.SecretTypeBasicAuth)
	}
	if string(sec.Data[cnpgSecretUsernameKey]) != "app_web" {
		t.Errorf("password secret username = %q, want app_web", sec.Data[cnpgSecretUsernameKey])
	}
	if len(sec.Data[PostgresPasswordKey]) == 0 {
		t.Error("password secret holds no password")
	}
	if sec.Labels[cnpgReloadLabel] != "true" {
		t.Errorf("password secret is not labelled %s=true, so a rewritten password would never reach the role", cnpgReloadLabel)
	}

	// The returned URL names the in-cluster Service, carries the role, and carries the password that
	// was written into the Secret — the operator and the app are handed the same credential.
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("the returned connection string does not parse: %v", err)
	}
	if u.User.Username() != "app_web" {
		t.Errorf("connection string user = %q, want app_web", u.User.Username())
	}
	if u.Host != PostgresSecretName+"."+addonNS+".svc:5432" {
		t.Errorf("connection string host = %q, want the in-cluster Service", u.Host)
	}
	if pw, _ := u.User.Password(); pw != string(sec.Data[PostgresPasswordKey]) {
		t.Error("the app is handed a password other than the one the operator was given")
	}

	// And the point of the whole change: no superuser connection was opened.
	for _, user := range rec.users() {
		if user == PostgresSuperuser {
			t.Fatalf("an attach opened a %s connection; provisioning runs no admin SQL (ADR-0066 §2)", PostgresSuperuser)
		}
		if user != "app_web" {
			t.Errorf("an attach opened a connection as %q; the only credential it may spend is the app's own", user)
		}
	}
}

// TestAttachRevokesPublicConnectAsTheDatabaseOwner asserts the one statement declarative
// provisioning cannot express survives, and survives on the least-privileged connection that can run
// it. Without it PUBLIC keeps PostgreSQL's default CONNECT and every app's role reaches every app's
// database — which is ADR-0031's isolation and the whole safety argument of `addon sql` (ADR-0087 §3).
func TestAttachRevokesPublicConnectAsTheDatabaseOwner(t *testing.T) {
	ctx := context.Background()
	p, _, rec := provisionerFor(t, addonNS)

	if _, err := p.EnsureAppDatabase(ctx, "web", controlplane.DefaultEnvironment); err != nil {
		t.Fatalf("EnsureAppDatabase: %v", err)
	}

	want := `REVOKE CONNECT ON DATABASE "web" FROM PUBLIC`
	if len(rec.statements) != 1 || rec.statements[0] != want {
		t.Fatalf("statements run = %q, want exactly [%q]", rec.statements, want)
	}
	if len(rec.dsns) != 1 {
		t.Fatalf("connections opened = %d, want exactly one", len(rec.dsns))
	}
	u, err := url.Parse(rec.dsns[0])
	if err != nil {
		t.Fatalf("the revoke connection string does not parse: %v", err)
	}
	// The owner, on its own database. A superuser here would work and would put the whole instance
	// one statement away from a path that only needs one database.
	if u.User.Username() != "app_web" {
		t.Errorf("the revoke ran as %q, want the database's owner app_web", u.User.Username())
	}
	if u.Path != "/web" {
		t.Errorf("the revoke ran against %q, want web's own database", u.Path)
	}
}

// TestAttachAdoptsAnExistingInstall is the upgrade guarantee: an app attached before provisioning
// became declarative has its database and role already, and the first attach afterwards describes
// them rather than asking for them again. The objects are updated in place — not created twice, and
// not refused for already existing.
func TestAttachAdoptsAnExistingInstall(t *testing.T) {
	ctx := context.Background()
	p, dyn, _ := provisionerFor(t, addonNS)
	name := provisioningObjectName(PostgresSecretName, "web")

	first, err := p.EnsureAppDatabase(ctx, "web", controlplane.DefaultEnvironment)
	if err != nil {
		t.Fatalf("first attach: %v", err)
	}
	dyn.ClearActions()

	second, err := p.EnsureAppDatabase(ctx, "web", controlplane.DefaultEnvironment)
	if err != nil {
		t.Fatalf("re-attach: %v", err)
	}
	if first == second {
		t.Error("a re-attach returned the previous connection string; the role password is rotated on every attach (ADR-0031)")
	}
	// What the second attach DID to the objects: it updated them. A create would mean the API server
	// refusing an existing object (and an attach failing on an install that already works); a delete
	// anywhere here would mean re-provisioning the thing ADR-0064 says survives.
	var created, updated, deleted int
	for _, action := range dyn.Actions() {
		if action.GetResource() != cnpgDatabaseGVR && action.GetResource() != cnpgDatabaseRoleGVR {
			continue
		}
		switch action.GetVerb() {
		case "create":
			created++
		case "update":
			updated++
		case "delete":
			deleted++
		}
	}
	if created != 0 || deleted != 0 || updated != 2 {
		t.Errorf("re-attach performed %d creates, %d updates and %d deletes; it must adopt both objects in place", created, updated, deleted)
	}
	if got := nestedString(t, getObject(t, dyn, cnpgDatabaseGVR, addonNS, name).Object, "spec", "name"); got != "web" {
		t.Errorf("the adopted Database names %q, want the existing web", got)
	}

	list, err := dyn.Resource(cnpgDatabaseGVR).Namespace(addonNS).List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatalf("listing Database objects: %v", err)
	}
	if len(list.Items) != 1 {
		t.Errorf("%d Database objects exist for one app; a re-attach must adopt, not add", len(list.Items))
	}
}

// TestAttachWaitsForTheOperatorAndReportsWhatItSaid asserts provisioning is treated as asynchronous:
// an object nobody applies does not silently succeed, and the failure carries the operator's own
// message rather than a bare timeout.
func TestAttachWaitsForTheOperatorAndReportsWhatItSaid(t *testing.T) {
	ctx := context.Background()
	p, dyn, _ := provisionerFor(t, addonNS)
	// An operator that refuses this object, and keeps saying so.
	const message = `reconciliation error: database "postgres" is reserved`
	dyn.PrependReactor("get", "databases", func(action k8stesting.Action) (bool, runtime.Object, error) {
		get := action.(k8stesting.GetAction)
		obj, err := dyn.Tracker().Get(cnpgDatabaseGVR, get.GetNamespace(), get.GetName())
		if err != nil {
			return true, nil, err
		}
		u := obj.(*unstructured.Unstructured).DeepCopy()
		_ = unstructured.SetNestedField(u.Object, false, "status", "applied")
		_ = unstructured.SetNestedField(u.Object, message, "status", "message")
		return true, u, nil
	})

	_, err := p.EnsureAppDatabase(ctx, "web", controlplane.DefaultEnvironment)
	if err == nil {
		t.Fatal("EnsureAppDatabase succeeded against an operator that never applied the Database")
	}
	if !strings.Contains(err.Error(), message) {
		t.Errorf("error %q does not carry what CloudNativePG said about it", err)
	}
}

// TestAttachProvesTheCredentialBeforeHandingItOut asserts the rotated password is not taken on
// trust. CloudNativePG applies it out of a labelled Secret rather than out of the object's spec, so
// no `status.applied` covers it; the connection that runs the revoke is what says it landed, and a
// refusal is retried rather than failed on.
func TestAttachProvesTheCredentialBeforeHandingItOut(t *testing.T) {
	ctx := context.Background()
	p, _, rec := provisionerFor(t, addonNS)
	rec.refusals = 2

	if _, err := p.EnsureAppDatabase(ctx, "web", controlplane.DefaultEnvironment); err != nil {
		t.Fatalf("EnsureAppDatabase: %v", err)
	}
	if len(rec.dsns) != 3 {
		t.Errorf("connections attempted = %d, want the two refusals plus the one that worked", len(rec.dsns))
	}
}

// TestAttachNeverHandsOutACredentialTheServerRefuses asserts the other side of the same wait: a
// password that never reaches the role fails the attach, loudly and naming the mechanism, rather
// than writing a connection string into the app's Secret that merely ought to work.
func TestAttachNeverHandsOutACredentialTheServerRefuses(t *testing.T) {
	ctx := context.Background()
	p, _, rec := provisionerFor(t, addonNS)
	rec.refusals = 1 << 30 // never accepted

	_, err := p.EnsureAppDatabase(ctx, "web", controlplane.DefaultEnvironment)
	if err == nil {
		t.Fatal("EnsureAppDatabase returned a connection string the server would not accept")
	}
	if !strings.Contains(err.Error(), cnpgReloadLabel) {
		t.Errorf("error %q does not name the mechanism that applies a rewritten password", err)
	}
}

// TestAttachLeavesTheDataSafeFromAnObjectGoingAway asserts the reclaim policy an ATTACH writes.
// While an app is attached, nothing that removes its objects — a namespace teardown, a collector,
// somebody tidying with kubectl — may take the database with them; only a detach, which flips the
// policy first, ever destroys anything.
func TestAttachLeavesTheDataSafeFromAnObjectGoingAway(t *testing.T) {
	ctx := context.Background()
	p, dyn, _ := provisionerFor(t, addonNS)
	name := provisioningObjectName(PostgresSecretName, "web")

	if _, err := p.EnsureAppDatabase(ctx, "web", controlplane.DefaultEnvironment); err != nil {
		t.Fatalf("EnsureAppDatabase: %v", err)
	}
	// Asserted on the objects themselves, because it is the objects — not this code — that decide
	// what happens to the data when they are deleted.
	for gvr, field := range map[schema.GroupVersionResource]string{
		cnpgDatabaseGVR:     "databaseReclaimPolicy",
		cnpgDatabaseRoleGVR: "databaseRoleReclaimPolicy",
	} {
		obj := getObject(t, dyn, gvr, addonNS, name)
		if got := nestedString(t, obj.Object, "spec", field); got != cnpgReclaimRetain {
			t.Errorf("%s %s = %q, want %q — an attached app's data would go with an object deleted by accident", gvr.Resource, field, got, cnpgReclaimRetain)
		}
	}
	// And the database accepts connections, which the teardown path turns off and a re-attach has to
	// turn back on.
	if allow, found, _ := unstructured.NestedBool(getObject(t, dyn, cnpgDatabaseGVR, addonNS, name).Object, "spec", "allowConnections"); !found || !allow {
		t.Error("an attached database does not state allowConnections: true, so a detach that failed halfway would leave it closed")
	}
}

// TestDetachDestroysTheDataThroughTheObjects is the destructive half, and it stays destructive
// (ADR-0031): the user was told at the confirmation prompt that their data goes. It goes through the
// objects rather than through admin SQL — each described one last time with a `delete` reclaim
// policy, then deleted — and the call does not return until the operator has actually finished.
func TestDetachDestroysTheDataThroughTheObjects(t *testing.T) {
	ctx := context.Background()
	p, dyn, rec := provisionerFor(t, addonNS)
	name := provisioningObjectName(PostgresSecretName, "web")

	if _, err := p.EnsureAppDatabase(ctx, "web", controlplane.DefaultEnvironment); err != nil {
		t.Fatalf("EnsureAppDatabase: %v", err)
	}

	// What the objects SAID when they were deleted is the whole of the destruction, so it is recorded
	// as they go rather than read afterwards from something that no longer exists.
	deleted := recordDeletions(dyn)

	before := len(rec.statements)
	if err := p.DropAppDatabase(ctx, "web", controlplane.DefaultEnvironment); err != nil {
		t.Fatalf("DropAppDatabase: %v", err)
	}
	if len(rec.statements) != before {
		t.Errorf("a detach ran %v; it destroys through the objects and runs no SQL of its own", rec.statements[before:])
	}

	db, ok := deleted.spec(cnpgDatabaseGVR)
	if !ok {
		t.Fatal("the Database object was never deleted, so nothing dropped the app's database")
	}
	if got := nestedString(t, db, "spec", "databaseReclaimPolicy"); got != cnpgReclaimDelete {
		t.Errorf("the Database was deleted holding databaseReclaimPolicy %q, want %q — the database would have survived the detach", got, cnpgReclaimDelete)
	}
	// The door is closed before the drop: CloudNativePG issues a plain DROP DATABASE, which one live
	// session refuses, where the admin SQL this replaced used WITH (FORCE).
	if allow, found, _ := unstructured.NestedBool(db, "spec", "allowConnections"); !found || allow {
		t.Error("the database was still accepting connections when it was dropped, so anything reconnecting would have blocked the drop")
	}
	role, ok := deleted.spec(cnpgDatabaseRoleGVR)
	if !ok {
		t.Fatal("the DatabaseRole object was never deleted, so nothing dropped the app's login role")
	}
	if got := nestedString(t, role, "spec", "databaseRoleReclaimPolicy"); got != cnpgReclaimDelete {
		t.Errorf("the DatabaseRole was deleted holding databaseRoleReclaimPolicy %q, want %q", got, cnpgReclaimDelete)
	}
	// PostgreSQL will not drop a role that still owns a database, so the order is not cosmetic.
	if deleted.order[0] != cnpgDatabaseGVR {
		t.Error("the login role was deleted before the database it owns; that DROP ROLE fails forever")
	}

	for _, gvr := range []schema.GroupVersionResource{cnpgDatabaseGVR, cnpgDatabaseRoleGVR} {
		if _, err := dyn.Resource(gvr).Namespace(addonNS).Get(ctx, name, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
			t.Errorf("the %s object survived the detach (err = %v)", gvr.Resource, err)
		}
	}
	if _, err := p.client.CoreV1().Secrets(addonNS).Get(ctx, name, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("the role's password secret survived the detach (err = %v)", err)
	}

	// Detaching twice is a no-op, not an error: a half-finished attach has to be tearable down.
	if err := p.DropAppDatabase(ctx, "web", controlplane.DefaultEnvironment); err != nil {
		t.Errorf("a second detach: %v", err)
	}
}

// TestDetachDescribesAnAttachmentItNeverProvisioned is the upgrade hole this would otherwise have,
// and it is the dangerous direction: an app attached before provisioning became declarative has a
// database and a role and NO objects describing them, so a detach that only deleted objects would
// destroy nothing and report success — leaving the data of an app somebody believed they had
// scrubbed. The teardown describes the attachment first, so it reaches exactly what the admin SQL
// used to.
func TestDetachDescribesAnAttachmentItNeverProvisioned(t *testing.T) {
	ctx := context.Background()
	p, dyn, _ := provisionerFor(t, addonNS)

	// No attach: nothing has ever written an object for this app.
	deleted := recordDeletions(dyn)
	if err := p.DropAppDatabase(ctx, "web", controlplane.DefaultEnvironment); err != nil {
		t.Fatalf("DropAppDatabase: %v", err)
	}
	db, ok := deleted.spec(cnpgDatabaseGVR)
	if !ok {
		t.Fatal("detaching an attachment with no objects deleted nothing, so the database was never dropped")
	}
	if got := nestedString(t, db, "spec", "name"); got != "web" {
		t.Errorf("the Database written to destroy the attachment names %q, want web", got)
	}
	if got := nestedString(t, db, "spec", "databaseReclaimPolicy"); got != cnpgReclaimDelete {
		t.Errorf("databaseReclaimPolicy = %q, want %q", got, cnpgReclaimDelete)
	}
	// It must not reference a password Secret that does not exist: the operator could not apply the
	// object at all, which is to say it could not drop the role.
	role, _ := deleted.spec(cnpgDatabaseRoleGVR)
	if _, found, _ := unstructured.NestedMap(role, "spec", "passwordSecret"); found {
		t.Error("the teardown DatabaseRole references a password Secret; an attachment made before this existed has none")
	}
}

// TestProvisioningObjectNamesCannotCollideAcrossEnvironments is issue #339 one layer down. Every
// environment's instance shares one add-on namespace, so the name these objects take has to be
// injective over (instance, app) — a hyphen is not, and the pair below is the counter-example that
// would have had a staging attach adopt the default environment's object.
func TestProvisioningObjectNamesCannotCollideAcrossEnvironments(t *testing.T) {
	staging := provisioningObjectName("burrow-postgres-staging", "web")
	defaulted := provisioningObjectName("burrow-postgres", "staging-web")
	if staging == defaulted {
		t.Fatalf("two different attachments share the object name %q", staging)
	}
	// And the name is still a legal object name.
	for _, name := range []string{staging, defaulted} {
		if strings.ContainsAny(name, "_ /") || name != strings.ToLower(name) {
			t.Errorf("%q is not a legal Kubernetes object name", name)
		}
	}
}

// TestProvisionerWithoutADynamicClientProvisionsNothing asserts the fail-closed posture: there is no
// admin-SQL path to fall back to, so a provisioner that cannot address custom resources refuses
// rather than reaching for the superuser credential sitting in the namespace.
func TestProvisionerWithoutADynamicClientProvisionsNothing(t *testing.T) {
	ctx := context.Background()
	rec := &sqlRecorder{}
	p := NewPostgresProvisioner(fake.NewSimpleClientset(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: PostgresSecretName, Namespace: addonNS},
		Data:       map[string][]byte{PostgresPasswordKey: []byte("supersecretpassword")},
	}), nil, AddonInstanceTarget(addonNS))
	p.open = rec.open

	if _, err := p.EnsureAppDatabase(ctx, "web", controlplane.DefaultEnvironment); !errors.Is(err, controlplane.ErrInvalid) {
		t.Errorf("EnsureAppDatabase err = %v, want ErrInvalid", err)
	}
	if err := p.DropAppDatabase(ctx, "web", controlplane.DefaultEnvironment); !errors.Is(err, controlplane.ErrInvalid) {
		t.Errorf("DropAppDatabase err = %v, want ErrInvalid", err)
	}
	if len(rec.dsns) != 0 {
		t.Errorf("a provisioner with no dynamic client opened %v", rec.dsns)
	}
}

// TestRolePasswordSecretInTheWrongShapeIsRefused asserts a Secret whose type CloudNativePG cannot
// read is refused by name rather than worked around. A Secret's type is immutable, so the operator
// would reject the reference and leave the role without the password the attach is about to hand
// out — a connection string for a credential that was never applied.
func TestRolePasswordSecretInTheWrongShapeIsRefused(t *testing.T) {
	ctx := context.Background()
	name := provisioningObjectName(PostgresSecretName, "web")
	p, _, _ := provisionerFor(t, addonNS, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: addonNS},
		Type:       corev1.SecretTypeOpaque,
		Data:       map[string][]byte{PostgresPasswordKey: []byte("left-behind")},
	})

	_, err := p.EnsureAppDatabase(ctx, "web", controlplane.DefaultEnvironment)
	if !errors.Is(err, controlplane.ErrInvalid) {
		t.Fatalf("EnsureAppDatabase err = %v, want ErrInvalid", err)
	}
	if !strings.Contains(err.Error(), name) {
		t.Errorf("error %q does not name the Secret that has to be dealt with", err)
	}
	if strings.Contains(err.Error(), "left-behind") {
		t.Errorf("error %q carries the Secret's value", err)
	}
}

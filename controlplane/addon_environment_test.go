// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package controlplane_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	cp "github.com/burrow-cloud/burrow/controlplane"
	"github.com/burrow-cloud/burrow/controlplane/internal/fake"
)

// This file holds the tests for ADR-0067 §1: one add-on instance per environment, and a provisioning
// seam that cannot be called without naming one.
//
// The hazard they exist for (issue #339) is worth restating, because it is what makes the assertions
// below the point rather than a formality. `addon attach postgres web` created a database named
// `web` owned by a role named `app_web`, and the provisioner was never told which environment it was
// acting for. With one instance for the whole cluster, an attach of `web` in staging therefore
// resolved to the same database as an attach of `web` in production — and because provisioning is
// IDEMPOTENT, the second one did not fail. It rotated the role password and handed staging a
// DATABASE_URL pointing at production's data. Nothing errored and nothing warned.
//
// So a test that only checks for an error would have proved nothing: THERE WAS NO ERROR. What the
// tests here assert is that the two attaches produce two different databases on two different
// servers, which is the property the collision violated.

// newEnvPostgresEngine builds an engine with a known app namespace, a permissive policy, and a fake
// provisioner that models one instance per environment.
func newEnvPostgresEngine(t *testing.T, appNamespace string) (*cp.Engine, *fake.Kubernetes, *fake.Database, *fake.Provisioner) {
	t.Helper()
	k := fake.NewKubernetes()
	d := fake.NewDatabase()
	d.SetPolicy(permissive())
	prov := fake.NewProvisioner()
	e, err := cp.New(cp.Deps{
		Kubernetes: k, Database: d,
		Clock: fake.NewClock(time.Date(2026, 6, 25, 12, 0, 0, 0, time.UTC)),
		IDs:   fake.NewIDs(), Resolver: fake.NewResolver(),
		Credentials: fake.NewCredentials(), DNS: fake.NewDNSFactory(),
		DatabaseProvisioner: prov,
		AppNamespace:        appNamespace,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return e, k, d, prov
}

// TestAddonInstanceNamePerEnvironment pins the derivation every other layer shares: an environment
// resolves to an instance, the default environment resolves to the name an existing install already
// has, and no two environments can resolve to the same one (ADR-0067 §1/§5).
func TestAddonInstanceNamePerEnvironment(t *testing.T) {
	// The default environment keeps today's unqualified name, which is what makes this change a
	// no-op for an install that predates environments: same pod, same volume, same credential.
	def, err := cp.AddonInstanceName(cp.AddonPostgres, cp.DefaultEnvironment)
	if err != nil {
		t.Fatalf("AddonInstanceName(default): %v", err)
	}
	if def != "burrow-postgres" {
		t.Errorf("default-environment instance = %q, want burrow-postgres (an existing install must not migrate)", def)
	}

	staging, err := cp.AddonInstanceName(cp.AddonPostgres, "staging")
	if err != nil {
		t.Fatalf("AddonInstanceName(staging): %v", err)
	}
	if staging == def {
		t.Fatalf("staging and the default environment resolved to the same instance %q — the collision is expressible", staging)
	}

	// Distinctness is the property, for every environment and every type: sharing one instance
	// across environments is not supported, so there must be no name that two environments produce
	// (ADR-0067 §5).
	seen := map[string]string{}
	for _, env := range []string{cp.DefaultEnvironment, "staging", "prod", "dev"} {
		for _, typ := range []cp.AddonType{cp.AddonPostgres, cp.AddonLogs, cp.AddonMetrics, cp.AddonCache} {
			name, nerr := cp.AddonInstanceName(typ, env)
			if nerr != nil {
				t.Fatalf("AddonInstanceName(%s, %s): %v", typ, env, nerr)
			}
			if prev, dup := seen[name]; dup {
				t.Errorf("instance name %q is produced by both %s and %s/%s", name, prev, typ, env)
			}
			seen[name] = string(typ) + "/" + env
		}
	}

	// An unnamed environment is an error, not a synonym for the default: a caller that omits it is
	// refused rather than quietly landing on whichever instance happens to exist (ADR-0067 §1).
	if _, err := cp.AddonInstanceName(cp.AddonPostgres, ""); !errors.Is(err, cp.ErrInvalid) {
		t.Errorf("AddonInstanceName with no environment = %v, want ErrInvalid", err)
	}
	if _, err := cp.AddonInstanceName(cp.AddonPostgres, "Not A Label"); !errors.Is(err, cp.ErrInvalid) {
		t.Errorf("AddonInstanceName with a malformed environment = %v, want ErrInvalid", err)
	}
}

// TestAttachInTwoEnvironmentsGetsTwoDatabases is THE test for issue #339: the same app name attached
// in two environments must end up with two databases on two instances, and the second attach must
// not adopt the first's.
//
// It is written to fail against the old code rather than to pass against the new. Before ADR-0067 §1
// the provisioner took no environment, so both attaches produced the SAME connection string; the
// assertion that the two URLs differ is what would have caught the bug, and the assertion that they
// name different instance hosts is what says the isolation comes from the instance rather than from
// a naming convention.
func TestAttachInTwoEnvironmentsGetsTwoDatabases(t *testing.T) {
	ctx := context.Background()
	e, k, _, prov := newEnvPostgresEngine(t, "burrow-apps")
	if _, err := e.AddEnvironment(ctx, "staging", "burrow-apps-staging"); err != nil {
		t.Fatalf("AddEnvironment: %v", err)
	}

	prodRes, err := e.AttachAddon(ctx, cp.AddonPostgres, "web", cp.DefaultEnvironment)
	if err != nil {
		t.Fatalf("AttachAddon(default): %v", err)
	}
	stagingRes, err := e.AttachAddon(ctx, cp.AddonPostgres, "web", "staging")
	if err != nil {
		t.Fatalf("AttachAddon(staging): %v", err)
	}
	if prodRes.Environment != cp.DefaultEnvironment || stagingRes.Environment != "staging" {
		t.Errorf("attach results name environments %q and %q, want default and staging", prodRes.Environment, stagingRes.Environment)
	}

	// Two provisioning calls, each naming its own environment.
	want := []fake.AppDatabase{{App: "web", Env: cp.DefaultEnvironment}, {App: "web", Env: "staging"}}
	got := prov.Ensured()
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("EnsureAppDatabase calls = %v, want %v", got, want)
	}

	// Two DISTINCT connection strings, naming two different instances. Under the old signature both
	// attaches returned the identical URL — this is the assertion that would have failed then.
	prodURL, ok := k.SecretValueInNamespace("burrow-apps", "web", "DATABASE_URL")
	if !ok {
		t.Fatal("no DATABASE_URL in the default environment's namespace")
	}
	stagingURL, ok := k.SecretValueInNamespace("burrow-apps-staging", "web", "DATABASE_URL")
	if !ok {
		t.Fatal("no DATABASE_URL in staging's namespace")
	}
	if prodURL == stagingURL {
		t.Fatal("both environments were handed the SAME connection string — staging is writing to the other environment's data (issue #339)")
	}
	if !strings.Contains(prodURL, "burrow-postgres.") {
		t.Errorf("default-environment URL does not name the default instance: %q", prodURL)
	}
	if !strings.Contains(stagingURL, "burrow-postgres-staging.") {
		t.Errorf("staging URL does not name staging's own instance: %q", stagingURL)
	}

	// And the databases themselves are two, one per instance — not one adopted twice.
	if defDBs := prov.Databases(cp.DefaultEnvironment); len(defDBs) != 1 || defDBs[0] != "web" {
		t.Errorf("default environment's instance holds %v, want [web]", defDBs)
	}
	if stgDBs := prov.Databases("staging"); len(stgDBs) != 1 || stgDBs[0] != "web" {
		t.Errorf("staging's instance holds %v, want [web]", stgDBs)
	}
}

// TestAttachIsIdempotentWithinOneEnvironment keeps the property that made the collision silent from
// being thrown away with it: re-attaching the SAME app in the SAME environment must still be a
// re-attach — it adopts the existing database and rotates the password (ADR-0031). Idempotence was
// never the bug; provisioning across an environment boundary was.
func TestAttachIsIdempotentWithinOneEnvironment(t *testing.T) {
	ctx := context.Background()
	e, _, _, prov := newEnvPostgresEngine(t, "burrow-apps")

	if _, err := e.AttachAddon(ctx, cp.AddonPostgres, "web", ""); err != nil {
		t.Fatalf("AttachAddon: %v", err)
	}
	if _, err := e.AttachAddon(ctx, cp.AddonPostgres, "web", ""); err != nil {
		t.Fatalf("re-attach in the same environment should succeed: %v", err)
	}
	if dbs := prov.Databases(cp.DefaultEnvironment); len(dbs) != 1 {
		t.Errorf("re-attach created %v, want the one existing database adopted", dbs)
	}
}

// TestDetachDropsOnlyTheNamedEnvironment asserts the destructive half is scoped too: detaching in
// one environment leaves the other environment's database alone. The old signature would have
// dropped whichever database the single instance held — for `web` in staging, that was production's
// (ADR-0067 §1).
func TestDetachDropsOnlyTheNamedEnvironment(t *testing.T) {
	ctx := context.Background()
	e, k, _, prov := newEnvPostgresEngine(t, "burrow-apps")
	if _, err := e.AddEnvironment(ctx, "staging", "burrow-apps-staging"); err != nil {
		t.Fatalf("AddEnvironment: %v", err)
	}
	for _, env := range []string{cp.DefaultEnvironment, "staging"} {
		if _, err := e.AttachAddon(ctx, cp.AddonPostgres, "web", env); err != nil {
			t.Fatalf("AttachAddon(%s): %v", env, err)
		}
	}

	if err := e.DetachAddon(ctx, cp.AddonPostgres, "web", "staging", true); err != nil {
		t.Fatalf("DetachAddon(staging): %v", err)
	}
	if dropped := prov.Dropped(); len(dropped) != 1 || dropped[0] != (fake.AppDatabase{App: "web", Env: "staging"}) {
		t.Fatalf("DropAppDatabase calls = %v, want one for staging only", dropped)
	}
	if dbs := prov.Databases(cp.DefaultEnvironment); len(dbs) != 1 || dbs[0] != "web" {
		t.Errorf("the default environment's database was destroyed by a staging detach: %v", dbs)
	}
	if dbs := prov.Databases("staging"); len(dbs) != 0 {
		t.Errorf("staging's database survived its own detach: %v", dbs)
	}
	// The credential is removed from staging's namespace only.
	if _, ok := k.SecretValueInNamespace("burrow-apps-staging", "web", "DATABASE_URL"); ok {
		t.Error("staging's DATABASE_URL survived the detach")
	}
	if _, ok := k.SecretValueInNamespace("burrow-apps", "web", "DATABASE_URL"); !ok {
		t.Error("the default environment's DATABASE_URL was removed by a staging detach")
	}
}

// TestAttachRefusesWhenTheEnvironmentIsAmbiguous asserts an attach that names no environment while
// several are registered is REFUSED rather than defaulting (ADR-0047 §1, ADR-0067 §1). Silently
// picking one is precisely the failure mode this record exists to close, and an attach is the
// operation where picking wrong hands an app another environment's data.
func TestAttachRefusesWhenTheEnvironmentIsAmbiguous(t *testing.T) {
	ctx := context.Background()
	e, _, _, prov := newEnvPostgresEngine(t, "burrow-apps")
	if _, err := e.AddEnvironment(ctx, "staging", "burrow-apps-staging"); err != nil {
		t.Fatalf("AddEnvironment: %v", err)
	}

	_, err := e.AttachAddon(ctx, cp.AddonPostgres, "web", "")
	if _, ok := cp.AsAmbiguousEnvironment(err); !ok {
		t.Fatalf("attach with no environment = %v, want an AmbiguousEnvironmentError", err)
	}
	if got := prov.Ensured(); len(got) != 0 {
		t.Errorf("a refused attach provisioned %v; nothing must be created before the environment is settled", got)
	}

	// An unregistered environment is a clear ErrNotFound, again before anything is provisioned.
	if _, err := e.AttachAddon(ctx, cp.AddonPostgres, "web", "ghost"); !errors.Is(err, cp.ErrNotFound) {
		t.Errorf("attach into an unregistered environment = %v, want ErrNotFound", err)
	}
	if got := prov.Ensured(); len(got) != 0 {
		t.Errorf("an attach into an unknown environment provisioned %v", got)
	}
}

// TestInstallAddonPerEnvironmentStandsUpSeparateInstances asserts installing the same add-on type in
// two environments produces two registry rows and two cluster instances, rather than one row being
// upserted over the other — the registry is no longer one row per type per cluster (ADR-0067 §1).
func TestInstallAddonPerEnvironmentStandsUpSeparateInstances(t *testing.T) {
	ctx := context.Background()
	e, k, d, _ := newEnvPostgresEngine(t, "burrow-apps")
	if _, err := e.AddEnvironment(ctx, "staging", "burrow-apps-staging"); err != nil {
		t.Fatalf("AddEnvironment: %v", err)
	}

	def, err := e.InstallAddon(ctx, cp.AddonPostgres, cp.DefaultEnvironment, true)
	if err != nil {
		t.Fatalf("InstallAddon(default): %v", err)
	}
	stg, err := e.InstallAddon(ctx, cp.AddonPostgres, "staging", true)
	if err != nil {
		t.Fatalf("InstallAddon(staging): %v", err)
	}
	if def.Name == stg.Name {
		t.Fatalf("both installs produced the instance %q — one environment's Postgres would be the other's", def.Name)
	}
	if def.Name != "burrow-postgres" {
		t.Errorf("default-environment instance = %q, want the pre-existing burrow-postgres", def.Name)
	}
	if def.Environment != cp.DefaultEnvironment || stg.Environment != "staging" {
		t.Errorf("instances report environments %q/%q, want default/staging", def.Environment, stg.Environment)
	}
	if def.Endpoint == stg.Endpoint {
		t.Errorf("both instances advertise the endpoint %q — they are the same server", def.Endpoint)
	}

	// Both rows are in the registry, and both instances are in the cluster.
	addons, err := d.Addons(ctx)
	if err != nil {
		t.Fatalf("Addons: %v", err)
	}
	var postgres int
	for _, a := range addons {
		if a.Type == cp.AddonPostgres {
			postgres++
		}
	}
	if postgres != 2 {
		t.Errorf("registry holds %d postgres add-ons, want 2 (one per environment)", postgres)
	}
	for _, name := range []string{def.Name, stg.Name} {
		if ready, rerr := k.AddonReady(ctx, name); rerr != nil || !ready {
			t.Errorf("instance %q is not in the cluster (ready=%v err=%v)", name, ready, rerr)
		}
		if _, hasVol := k.AddonVolume(name); !hasVol {
			t.Errorf("instance %q has no data volume of its own", name)
		}
	}
}

// TestRemoveAddonNamesOnlyItsOwnEnvironmentsApps asserts the removal confirmation asks the instance
// being removed who is attached, not some other environment's instance (ADR-0067 §1). The
// confirmation message is what an operator approves a destructive removal on, so naming the wrong
// environment's apps would make it actively misleading.
func TestRemoveAddonNamesOnlyItsOwnEnvironmentsApps(t *testing.T) {
	ctx := context.Background()
	e, _, _, prov := newEnvPostgresEngine(t, "burrow-apps")
	if _, err := e.AddEnvironment(ctx, "staging", "burrow-apps-staging"); err != nil {
		t.Fatalf("AddEnvironment: %v", err)
	}
	if _, err := e.InstallAddon(ctx, cp.AddonPostgres, "staging", true); err != nil {
		t.Fatalf("InstallAddon(staging): %v", err)
	}
	prov.SetAttachedApps(cp.DefaultEnvironment, "billing", "web")
	prov.SetAttachedApps("staging", "web")

	res, err := e.RemoveAddon(ctx, "burrow-postgres-staging", true, true)
	if err != nil {
		t.Fatalf("RemoveAddon(staging): %v", err)
	}
	if len(res.AttachedApps) != 1 || res.AttachedApps[0] != "web" {
		t.Errorf("removal reported attached apps %v, want staging's [web] — not the default environment's", res.AttachedApps)
	}
}

// TestBackupAndRestoreAreEnvironmentScoped asserts a dump records the instance it came from and can
// only be restored into that same environment (ADR-0067 §1). Restoring one environment's dump into
// another's live database is the issue #339 hazard pointed the other way: it would overwrite real
// data with another environment's, and idempotent, successful-looking machinery would do it.
func TestBackupAndRestoreAreEnvironmentScoped(t *testing.T) {
	ctx := context.Background()
	e, k, _, _ := newEnvPostgresEngine(t, "burrow-apps")
	if _, err := e.AddEnvironment(ctx, "staging", "burrow-apps-staging"); err != nil {
		t.Fatalf("AddEnvironment: %v", err)
	}

	stgBackup, err := e.BackupAddon(ctx, cp.AddonPostgres, "web", "staging")
	if err != nil {
		t.Fatalf("BackupAddon(staging): %v", err)
	}
	if stgBackup.Backup.Environment != "staging" {
		t.Errorf("recorded backup environment = %q, want staging", stgBackup.Backup.Environment)
	}
	if jobs := k.BackupJobs(); len(jobs) != 1 || jobs[0].Env != "staging" {
		t.Errorf("backup Jobs = %+v, want one run against staging's instance", jobs)
	}

	// The dump restores into the environment it came from.
	if err := e.RestoreAddon(ctx, cp.AddonPostgres, "web", stgBackup.Backup.ID, "staging", true); err != nil {
		t.Fatalf("RestoreAddon(staging): %v", err)
	}
	if jobs := k.RestoreJobs(); len(jobs) != 1 || jobs[0].Env != "staging" {
		t.Errorf("restore Jobs = %+v, want one run against staging's instance", jobs)
	}

	// And it is refused as a source for another environment's live database.
	err = e.RestoreAddon(ctx, cp.AddonPostgres, "web", stgBackup.Backup.ID, cp.DefaultEnvironment, true)
	if !errors.Is(err, cp.ErrInvalid) {
		t.Fatalf("restoring staging's dump into the default environment = %v, want ErrInvalid", err)
	}
	if jobs := k.RestoreJobs(); len(jobs) != 1 {
		t.Errorf("a refused restore still ran a Job: %+v", jobs)
	}
}

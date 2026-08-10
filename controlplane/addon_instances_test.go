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

// ADR-0091: one shared Postgres instance per environment is a DEFAULT, not a ceiling.
//
// The tests here are about the four things that ceiling was hiding — how a second instance is named,
// what attach means when several exist, which per-environment names are really per-instance, and who
// may create one — plus the property every one of them is measured against: an operator who never
// types --name cannot tell this record was accepted.

// TestSecondInstanceIsAskedForByName is §1 and §2 together. `--name analytics` stands a second
// instance up BESIDE the environment's own, and the two are distinguishable in the one way that
// matters: they are different `Cluster`s with different names, so they are different servers.
//
// The second one's cluster name is GENERATED and is not composed from the label, because a composed
// name has parts that can be got wrong — `burrow-postgres-staging-x` is both the instance `x` in
// staging and the first instance of an environment called `staging-x`, which is the ambiguity cloud
// ADR-0029 removed after it let one tenant reach another's database.
func TestSecondInstanceIsAskedForByName(t *testing.T) {
	ctx := context.Background()
	e, _, d, _ := newPostgresEngine(t)

	first, err := d.AddonByLabel(ctx, cp.DefaultEnvironment, mustInstance(t, cp.AddonPostgres, cp.DefaultEnvironment))
	if err != nil {
		t.Fatalf("the environment's own instance is not in the registry: %v", err)
	}

	second, err := e.InstallAddon(ctx, cp.AddonPostgres, cp.DefaultEnvironment, cp.InstallAddonOptions{Name: "analytics", Confirm: true})
	if err != nil {
		t.Fatalf("InstallAddon(--name analytics): %v", err)
	}
	if second.Label != "analytics" {
		t.Errorf("label = %q, want the name the operator typed", second.Label)
	}
	if second.Name == first.Name {
		t.Fatalf("the second instance came up as %q, the same `Cluster` as the environment's own — it is the same server", second.Name)
	}
	if strings.Contains(second.Name, "analytics") || strings.Contains(second.Name, cp.DefaultEnvironment) {
		t.Errorf("cluster name = %q, want a GENERATED id: a name composed from the label or the environment is one whose parts can be parsed back out wrong (ADR-0091 §2)", second.Name)
	}
	if !strings.HasPrefix(second.Name, "burrow-postgres-") {
		t.Errorf("cluster name = %q, want burrow-postgres-<id>", second.Name)
	}

	// And the environment's own instance is untouched: same name, same label, same row.
	after, err := d.Addon(ctx, first.Name)
	if err != nil {
		t.Fatalf("the environment's own instance disappeared: %v", err)
	}
	if after.Label != first.Label || after.Name != first.Name {
		t.Errorf("the environment's own instance became %s/%s, want %s/%s — nothing on a live install may move", after.Name, after.Label, first.Name, first.Label)
	}
}

// TestFirstInstanceKeepsTheNameItHasAndIsLabelledWithIt is ADR-0091 §2's upgrade guarantee, and it
// is the clause the whole record rests on: an install that predates it must not move.
//
// The label is that same name deliberately. It is what a guardrail key holds and what `addon remove`
// takes, and both are strings an operator may already have typed — so a prettier label would silently
// stop `prod.burrow-postgres.addon.remove` from matching the instance it was written for, which reads
// as protection and is not.
func TestFirstInstanceKeepsTheNameItHasAndIsLabelledWithIt(t *testing.T) {
	ctx := context.Background()
	e, _, d, _ := newPostgresEngine(t)

	for _, env := range []string{cp.DefaultEnvironment, "staging"} {
		if env != cp.DefaultEnvironment {
			if _, err := e.AddEnvironment(ctx, env, "burrow-apps-"+env); err != nil {
				t.Fatalf("AddEnvironment: %v", err)
			}
			installPostgresIn(t, e, env)
		}
		want := mustInstance(t, cp.AddonPostgres, env)
		info, err := d.Addon(ctx, want)
		if err != nil {
			t.Fatalf("environment %s's instance is not registered under %q: %v", env, want, err)
		}
		if info.Label != want {
			t.Errorf("environment %s's first instance is labelled %q, want %q — a guardrail key an operator already wrote holds that string", env, info.Label, want)
		}
	}
}

// TestInstallingTheSameNameTwiceReinstallsThatInstance: an instance's identity is its label, and
// `addon install` has always been idempotent. The alternative is a second pod and a second volume for
// a label somebody typed twice.
func TestInstallingTheSameNameTwiceReinstallsThatInstance(t *testing.T) {
	ctx := context.Background()
	e, _, d, _ := newPostgresEngine(t)

	first, err := e.InstallAddon(ctx, cp.AddonPostgres, cp.DefaultEnvironment, cp.InstallAddonOptions{Name: "analytics", Confirm: true})
	if err != nil {
		t.Fatalf("InstallAddon: %v", err)
	}
	again, err := e.InstallAddon(ctx, cp.AddonPostgres, cp.DefaultEnvironment, cp.InstallAddonOptions{Name: "analytics", Confirm: true})
	if err != nil {
		t.Fatalf("InstallAddon (again): %v", err)
	}
	if again.Name != first.Name {
		t.Errorf("the second install came up as %q rather than re-applying %q — a label typed twice is a pod and a volume nobody asked for", again.Name, first.Name)
	}
	instances, err := d.AddonsInEnvironment(ctx, cp.AddonPostgres, cp.DefaultEnvironment)
	if err != nil {
		t.Fatalf("AddonsInEnvironment: %v", err)
	}
	if len(instances) != 2 {
		t.Errorf("environment holds %d instances, want 2 (its own and analytics)", len(instances))
	}
}

// TestInstallRefusesALabelAnotherTypeHolds: a label is unique WITHIN an environment and not within a
// (type, environment) pair, because ADR-0085's guardrail key is `<env>.<name>.<code>` and has no type
// component to disambiguate with. Two instances answering to one key would make a disposition
// ambiguous, which is the property that key shape depends on.
func TestInstallRefusesALabelAnotherTypeHolds(t *testing.T) {
	ctx := context.Background()
	e, _, _, _ := newPostgresEngine(t)

	if _, err := e.InstallAddon(ctx, cp.AddonLogs, cp.DefaultEnvironment, cp.InstallAddonOptions{Name: "shared", Confirm: true}); err != nil {
		t.Fatalf("InstallAddon(logs --name shared): %v", err)
	}
	_, err := e.InstallAddon(ctx, cp.AddonPostgres, cp.DefaultEnvironment, cp.InstallAddonOptions{Name: "shared", Confirm: true})
	if !errors.Is(err, cp.ErrInvalid) {
		t.Fatalf("installing a postgres instance under a label the logs add-on holds = %v, want ErrInvalid", err)
	}
}

// TestAttachWithNoNameIsTheEnvironmentsOwnInstance is the "nobody can tell this record was accepted"
// property, on the verb an agent uses most.
func TestAttachWithNoNameIsTheEnvironmentsOwnInstance(t *testing.T) {
	ctx := context.Background()
	e, _, _, prov := newPostgresEngine(t)
	if _, err := e.InstallAddon(ctx, cp.AddonPostgres, cp.DefaultEnvironment, cp.InstallAddonOptions{Name: "analytics", Confirm: true}); err != nil {
		t.Fatalf("InstallAddon(analytics): %v", err)
	}

	res, err := e.AttachAddon(ctx, cp.AddonPostgres, "web", "", cp.AttachAddonOptions{Confirm: true})
	if err != nil {
		t.Fatalf("AttachAddon: %v", err)
	}
	own := mustInstance(t, cp.AddonPostgres, cp.DefaultEnvironment)
	if res.Instance != own {
		t.Errorf("attach landed on %q, want the environment's own instance %q — a second instance existing must not change what naming none means", res.Instance, own)
	}
	if res.SecretKey != cp.AppDatabaseURLKey {
		t.Errorf("variable = %q, want %s", res.SecretKey, cp.AppDatabaseURLKey)
	}
	if got := prov.Ensured(); len(got) != 1 || got[0].Instance != own {
		t.Errorf("provisioned on %v, want one call against %q", got, own)
	}
}

// TestSecondAttachmentMustNameItsOwnVariable is §3, and the refusal is the decision.
//
// Burrow does not invent `DATABASE_URL_2`. A generated second name is a name the application was
// never told to read, so the attach would report success and the app would find nothing — the failure
// arriving at a connection rather than at the command that caused it. The refusal that names the
// occupied variable is the better answer and it already existed.
func TestSecondAttachmentMustNameItsOwnVariable(t *testing.T) {
	ctx := context.Background()
	e, _, _, _ := newPostgresEngine(t)
	if _, err := e.InstallAddon(ctx, cp.AddonPostgres, cp.DefaultEnvironment, cp.InstallAddonOptions{Name: "analytics", Confirm: true}); err != nil {
		t.Fatalf("InstallAddon(analytics): %v", err)
	}
	if _, err := e.AttachAddon(ctx, cp.AddonPostgres, "web", "", cp.AttachAddonOptions{Confirm: true}); err != nil {
		t.Fatalf("first AttachAddon: %v", err)
	}

	_, err := e.AttachAddon(ctx, cp.AddonPostgres, "web", "", cp.AttachAddonOptions{Confirm: true, Instance: "analytics"})
	if !errors.Is(err, cp.ErrInvalid) {
		t.Fatalf("a second attachment falling back to DATABASE_URL = %v, want ErrInvalid", err)
	}
	if !strings.Contains(err.Error(), cp.AppDatabaseURLKey) {
		t.Errorf("refusal %q does not name the variable that is taken", err)
	}
}

// TestAnAppMayHoldSeveralAttachments is the rest of §3: a second attachment under its own variable
// works, the two are separate databases on separate servers, and detaching one leaves the other's
// variable, role and database alone.
func TestAnAppMayHoldSeveralAttachments(t *testing.T) {
	ctx := context.Background()
	e, k, _, prov := newPostgresEngine(t)
	second, err := e.InstallAddon(ctx, cp.AddonPostgres, cp.DefaultEnvironment, cp.InstallAddonOptions{Name: "analytics", Confirm: true})
	if err != nil {
		t.Fatalf("InstallAddon(analytics): %v", err)
	}
	own := mustInstance(t, cp.AddonPostgres, cp.DefaultEnvironment)

	if _, err := e.AttachAddon(ctx, cp.AddonPostgres, "web", "", cp.AttachAddonOptions{Confirm: true}); err != nil {
		t.Fatalf("first AttachAddon: %v", err)
	}
	res, err := e.AttachAddon(ctx, cp.AddonPostgres, "web", "", cp.AttachAddonOptions{Confirm: true, Instance: "analytics", EnvKey: "ANALYTICS_URL"})
	if err != nil {
		t.Fatalf("second AttachAddon: %v", err)
	}
	if res.Instance != "analytics" || res.SecretKey != "ANALYTICS_URL" {
		t.Errorf("second attachment = %s/%s, want analytics/ANALYTICS_URL", res.Instance, res.SecretKey)
	}
	if res.PreviousSecretKey != "" {
		t.Errorf("the second attachment reported moving %q — it is a second variable, not a rename of the first", res.PreviousSecretKey)
	}

	// Both variables are in the app's Secret, and they are DIFFERENT connection strings: one per
	// server, which is the whole point of the second instance.
	firstURL, ok := k.SecretValue("web", cp.AppDatabaseURLKey)
	if !ok {
		t.Fatal("the first attachment's variable was removed by the second attach")
	}
	secondURL, ok := k.SecretValue("web", "ANALYTICS_URL")
	if !ok {
		t.Fatal("the second attachment's variable was not written")
	}
	if firstURL == secondURL {
		t.Error("both attachments hand the app the same connection string — they are the same server")
	}
	if secondURL != fake.URLForInstance("web", second.Name) {
		t.Errorf("the second attachment's URL does not name the second instance: %q", secondURL)
	}

	// Detaching one leaves the other untouched (§3).
	if err := e.DetachAddon(ctx, cp.AddonPostgres, "web", "", cp.DetachAddonOptions{Instance: "analytics", Confirm: true}); err != nil {
		t.Fatalf("DetachAddon(analytics): %v", err)
	}
	if _, ok := k.SecretValue("web", "ANALYTICS_URL"); ok {
		t.Error("detach left the second attachment's variable behind")
	}
	if _, ok := k.SecretValue("web", cp.AppDatabaseURLKey); !ok {
		t.Error("detaching the second attachment removed the FIRST one's variable")
	}
	for _, revoked := range prov.Revoked() {
		if revoked.Instance == own {
			t.Errorf("detaching from analytics revoked the app's role on %q as well", own)
		}
	}
}

// TestBackupClaimIsPerInstance is §4. Two instances in ONE environment both holding a database called
// `web` must not write the same path on one disk — issue #339's shape with the environment held
// constant, where the registry rows would say which instance each dump came from while nothing on the
// volume did.
func TestBackupClaimIsPerInstance(t *testing.T) {
	ctx := context.Background()
	e, _, _, _ := newPostgresEngine(t)
	second, err := e.InstallAddon(ctx, cp.AddonPostgres, cp.DefaultEnvironment, cp.InstallAddonOptions{Name: "analytics", Confirm: true})
	if err != nil {
		t.Fatalf("InstallAddon(analytics): %v", err)
	}
	if _, err := e.AttachAddon(ctx, cp.AddonPostgres, "web", "", cp.AttachAddonOptions{Confirm: true}); err != nil {
		t.Fatalf("AttachAddon: %v", err)
	}
	if _, err := e.AttachAddon(ctx, cp.AddonPostgres, "web", "", cp.AttachAddonOptions{Confirm: true, Instance: "analytics", EnvKey: "ANALYTICS_URL"}); err != nil {
		t.Fatalf("AttachAddon(analytics): %v", err)
	}

	own, err := e.BackupAddon(ctx, cp.AddonPostgres, "web", "", "", "")
	if err != nil {
		t.Fatalf("BackupAddon(the environment's own instance): %v", err)
	}
	other, err := e.BackupAddon(ctx, cp.AddonPostgres, "web", "", "analytics", "")
	if err != nil {
		t.Fatalf("BackupAddon(analytics): %v", err)
	}
	if own.Backup.Volume == other.Backup.Volume {
		t.Fatalf("both dumps landed on %q — two instances' dumps for an app of the same name share one disk", own.Backup.Volume)
	}
	if own.Backup.Volume != cp.PostgresBackupVolume {
		t.Errorf("the default environment's claim = %q, want %q — no existing dump may move", own.Backup.Volume, cp.PostgresBackupVolume)
	}
	wantOther, err := cp.BackupVolumeName(cp.AddonPostgres, second.Name)
	if err != nil {
		t.Fatalf("BackupVolumeName: %v", err)
	}
	if other.Backup.Volume != wantOther {
		t.Errorf("the second instance's claim = %q, want %q", other.Backup.Volume, wantOther)
	}
}

// TestAGuardrailScopesByTheLabel is §4's last clause, and it has two halves that matter equally.
//
// A disposition an operator already wrote — against the name their instance has always had — keeps
// matching, because that name is now its label. And a disposition against a SECOND instance is
// written against the label too, never the generated cluster name: a key nobody can read is a key
// nobody will write.
func TestAGuardrailScopesByTheLabel(t *testing.T) {
	ctx := context.Background()
	e, _, d, _ := newPostgresEngine(t)
	if _, err := e.InstallAddon(ctx, cp.AddonPostgres, cp.DefaultEnvironment, cp.InstallAddonOptions{Name: "analytics", Confirm: true}); err != nil {
		t.Fatalf("InstallAddon(analytics): %v", err)
	}

	// Deny removal of the SECOND instance by its label, and leave the environment's own alone.
	pol := permissive()
	if pol.Dispositions == nil {
		pol.Dispositions = map[cp.GuardrailCode]cp.Disposition{}
	}
	d.SetPolicy(pol)
	if err := e.SetGuardrail(ctx, cp.GuardrailScope{Env: cp.DefaultEnvironment, Name: "analytics"}, "", cp.GuardrailAddonRemove, cp.DispositionDeny); err != nil {
		t.Fatalf("SetGuardrail: %v", err)
	}

	if _, err := e.RemoveAddon(ctx, "analytics", cp.RemoveAddonOptions{Environment: cp.DefaultEnvironment, Confirm: true}); err == nil {
		t.Fatal("the denied instance was removed; a disposition written against a label protected nothing")
	}
	// And the environment's own instance is not caught by it: the disposition names one instance.
	if _, err := e.RemoveAddon(ctx, mustInstance(t, cp.AddonPostgres, cp.DefaultEnvironment), cp.RemoveAddonOptions{Confirm: true}); err != nil {
		t.Fatalf("removing the environment's own instance was refused by a disposition written for another: %v", err)
	}
}

// TestRemoveStillTakesTheRegistryName is the compatibility half of the removal path. `addon remove
// burrow-postgres-staging` names a row outright and has never needed an environment, so it must keep
// working unchanged; and naming an environment that contradicts the row is a refusal rather than a
// silent preference for one of them, because this verb destroys.
func TestRemoveStillTakesTheRegistryName(t *testing.T) {
	ctx := context.Background()
	e, _, _, _ := newPostgresEngine(t)
	if _, err := e.AddEnvironment(ctx, "staging", "burrow-apps-staging"); err != nil {
		t.Fatalf("AddEnvironment: %v", err)
	}
	installPostgresIn(t, e, "staging")
	staging := mustInstance(t, cp.AddonPostgres, "staging")

	// Naming an environment the row contradicts is refused, with nothing removed.
	if _, err := e.RemoveAddon(ctx, staging, cp.RemoveAddonOptions{Environment: cp.DefaultEnvironment, Confirm: true}); !errors.Is(err, cp.ErrInvalid) {
		t.Fatalf("removing staging's instance while naming prod = %v, want ErrInvalid", err)
	}
	// And the plain form, with no environment at all, still resolves it.
	if _, err := e.RemoveAddon(ctx, staging, cp.RemoveAddonOptions{Confirm: true}); err != nil {
		t.Fatalf("RemoveAddon by registry name: %v", err)
	}
}

// TestAnInstanceBelongsToExactlyOneEnvironment is §6, and it is ADR-0067 §1's fix surviving whole:
// this record relaxes how many instances an environment may hold and nothing about what an instance
// isolates. A label in one environment does not answer in another, whatever it is called.
func TestAnInstanceBelongsToExactlyOneEnvironment(t *testing.T) {
	ctx := context.Background()
	e, _, _, _ := newPostgresEngine(t)
	if _, err := e.AddEnvironment(ctx, "staging", "burrow-apps-staging"); err != nil {
		t.Fatalf("AddEnvironment: %v", err)
	}
	installPostgresIn(t, e, "staging")
	if _, err := e.InstallAddon(ctx, cp.AddonPostgres, "staging", cp.InstallAddonOptions{Name: "analytics", Confirm: true}); err != nil {
		t.Fatalf("InstallAddon(staging --name analytics): %v", err)
	}

	// `analytics` exists in staging and nowhere else, so an attach in production cannot reach it.
	_, err := e.AttachAddon(ctx, cp.AddonPostgres, "web", cp.DefaultEnvironment, cp.AttachAddonOptions{Confirm: true, Instance: "analytics", EnvKey: "ANALYTICS_URL"})
	if !errors.Is(err, cp.ErrNotFound) {
		t.Fatalf("attaching in prod to an instance that only exists in staging = %v, want ErrNotFound", err)
	}
}

// TestPhysicalRestoreRefusesAnotherInstancesBaseBackup is §4 on the most destructive verb in the
// product. Two instances in one environment each keep their own pgBackRest repository, so the
// environment on a recorded backup no longer says which server it came from — and recovering one
// instance from the other's base backup would replace a live database with another server's data.
//
// The stanza is read back out of the object key Burrow composed when the backup completed, so no
// column had to be added and every physical backup taken before this record still resolves.
func TestPhysicalRestoreRefusesAnotherInstancesBaseBackup(t *testing.T) {
	ctx := context.Background()
	// An engine with object storage registered, because a physical restore needs a repository to
	// recover from before it can get as far as looking at the backup.
	e, _, d, creds := newBackupDestinationEngine(t)
	seedObjectStoreProvider(t, d, creds, "b2")
	other := "burrow-postgres-an4lyt"

	// A completed physical backup of ANOTHER instance in the same environment.
	backup := cp.Backup{
		ID:          "bk-other",
		Kind:        cp.BackupKindPhysical,
		Environment: cp.DefaultEnvironment,
		Status:      cp.BackupCompleted,
		Destination: cp.BackupDestinationObjectStore,
		Provider:    "b2",
		ObjectKey:   cp.PgBackRestManifestKey("burrow/pgbackrest", other, "20260809-120000F"),
	}
	if err := d.RecordBackup(ctx, backup); err != nil {
		t.Fatalf("RecordBackup: %v", err)
	}

	_, err := e.RestoreInstance(ctx, cp.AddonPostgres, cp.DefaultEnvironment, cp.RestoreInstanceOptions{
		Backup: backup.ID, Confirm: true,
	})
	if !errors.Is(err, cp.ErrInvalid) {
		t.Fatalf("recovering the environment's own instance from another instance's base backup = %v, want ErrInvalid", err)
	}
	if !strings.Contains(err.Error(), other) {
		t.Errorf("refusal %q does not name the instance the backup actually came from", err)
	}
}

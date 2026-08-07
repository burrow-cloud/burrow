// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package controlplane_test

import (
	"context"
	"errors"
	"slices"
	"testing"

	cp "github.com/burrow-cloud/burrow/controlplane"
	"github.com/burrow-cloud/burrow/controlplane/internal/fake"
)

// A secret key that is a file and NOT an environment variable (ADR-0089 §4). This is the half that
// actually removes a credential from /proc/self/environ, and it is the half with a price: the app
// leaves the envFrom fast path, so its pod template names each remaining key and a `secret set` has
// to re-apply the workload rather than restarting it.

// boolPtr is the tri-state MountSecret takes: nil leaves a key's marking alone.
func boolPtr(b bool) *bool { return &b }

// secretEnvKeys returns the keys the app's currently-applied workload still delivers as environment
// variables, and whether it has left the envFrom fast path at all.
func secretEnvKeys(t *testing.T, k *fake.Kubernetes, app string) ([]string, bool) {
	t.Helper()
	spec, ok := k.Spec(app)
	if !ok {
		t.Fatalf("no workload applied for %q", app)
	}
	return spec.SecretEnvKeys, spec.SecretFiles.AnyFileOnly()
}

// mountedApp deploys an app holding three secrets, one of which is about to become file-only.
func mountedApp(t *testing.T) (*cp.Engine, *fake.Kubernetes) {
	t.Helper()
	e, k, _, _ := newEngine(t, permissive())
	k.SetSecret("web", "KUBECONFIG", "apiVersion: v1")
	k.SetSecret("web", "DATABASE_URL", "postgres://x")
	k.SetSecret("web", "STRIPE_SECRET_KEY", "sk_live_xyz")
	if _, err := e.Deploy(context.Background(), cp.DeployRequest{App: "web", Image: "img:1", Replicas: 1}); err != nil {
		t.Fatalf("deploy: %v", err)
	}
	return e, k
}

// TestNoEnvTakesTheKeyOutOfTheEnvironment: the key is on disk, it is not among the keys the pod
// template delivers as variables, and the other two still are.
func TestNoEnvTakesTheKeyOutOfTheEnvironment(t *testing.T) {
	ctx := context.Background()
	e, k := mountedApp(t)

	res, err := e.MountSecret(ctx, "web", "", "KUBECONFIG", "", "", boolPtr(true))
	if err != nil {
		t.Fatalf("MountSecret --no-env: %v", err)
	}
	if len(res.Mounts) != 1 || !res.Mounts[0].NoEnv {
		t.Fatalf("mounts = %+v, want KUBECONFIG marked file-only", res.Mounts)
	}
	if got := res.FileOnly(); !slices.Equal(got, []string{"KUBECONFIG"}) {
		t.Errorf("FileOnly = %v, want [KUBECONFIG]", got)
	}
	keys, enumerated := secretEnvKeys(t, k, "web")
	if !enumerated {
		t.Fatal("the app kept the envFrom fast path with a file-only key, which sources the whole Secret and would put the credential back")
	}
	if !slices.Equal(keys, []string{"DATABASE_URL", "STRIPE_SECRET_KEY"}) {
		t.Errorf("enumerated keys = %v, want the two keys that are still variables, sorted", keys)
	}
	// The value is untouched: it is still in the Secret, and still on disk.
	if got := mountedKeys(t, k, "web"); !slices.Equal(got, []string{"KUBECONFIG"}) {
		t.Errorf("workload projects %v as files, want KUBECONFIG — file-only means file, not gone", got)
	}
}

// TestMountWithoutNoEnvKeepsTheFastPath is the engine half of the property the adapter test pins:
// mounting is not what costs the fast path. The SecretKeys read is made to FAIL, which proves the
// enumeration is not merely empty but never attempted — an app that asked for none of this pays
// neither the template change nor the extra call.
func TestMountWithoutNoEnvKeepsTheFastPath(t *testing.T) {
	ctx := context.Background()
	e, k := mountedApp(t)

	if _, err := e.MountSecret(ctx, "web", "", "KUBECONFIG", "", "", nil); err != nil {
		t.Fatalf("MountSecret: %v", err)
	}
	k.SetError(fake.OpSecretKeys, errors.New("the keys must not be read for an app on the fast path"))
	defer k.SetError(fake.OpSecretKeys, nil)
	if _, err := e.Deploy(ctx, cp.DeployRequest{App: "web", Image: "img:2", Replicas: 1}); err != nil {
		t.Fatalf("deploy: %v", err)
	}
	keys, enumerated := secretEnvKeys(t, k, "web")
	if enumerated || len(keys) != 0 {
		t.Errorf("an app that mounted a key without --no-env enumerates %v, want the wholesale envFrom", keys)
	}
}

// TestUnmarkingRestoresTheVariable, and the tri-state that guards it: a mount that says nothing
// about the environment leaves the marking alone, so renaming a file cannot silently return a
// credential to an environment somebody deliberately took it out of.
func TestUnmarkingRestoresTheVariable(t *testing.T) {
	ctx := context.Background()
	e, k := mountedApp(t)

	if _, err := e.MountSecret(ctx, "web", "", "KUBECONFIG", "", "", boolPtr(true)); err != nil {
		t.Fatalf("MountSecret --no-env: %v", err)
	}
	res, err := e.MountSecret(ctx, "web", "", "KUBECONFIG", "kubeconfig.yaml", "", nil)
	if err != nil {
		t.Fatalf("MountSecret (rename): %v", err)
	}
	if len(res.Mounts) != 1 || !res.Mounts[0].NoEnv || res.Mounts[0].Filename != "kubeconfig.yaml" {
		t.Fatalf("mounts = %+v, want the file renamed and the key still file-only", res.Mounts)
	}

	res, err = e.MountSecret(ctx, "web", "", "KUBECONFIG", "", "", boolPtr(false))
	if err != nil {
		t.Fatalf("MountSecret --no-env=false: %v", err)
	}
	if res.AnyFileOnly() {
		t.Fatalf("mounts = %+v, want the marking removed", res.Mounts)
	}
	keys, enumerated := secretEnvKeys(t, k, "web")
	if enumerated || len(keys) != 0 {
		t.Errorf("the app still enumerates %v after un-marking; with no file-only key it belongs back on envFrom, which delivers every key", keys)
	}
}

// TestUnmountRestoresTheVariable. A file-only marking is a field of the mount, so it cannot outlive
// the file: unmounting puts the key back in the environment rather than leaving it reachable by no
// route at all. That placement is the rule "a key is file-only only while it is mounted".
func TestUnmountRestoresTheVariable(t *testing.T) {
	ctx := context.Background()
	e, k := mountedApp(t)

	if _, err := e.MountSecret(ctx, "web", "", "KUBECONFIG", "", "", boolPtr(true)); err != nil {
		t.Fatalf("MountSecret --no-env: %v", err)
	}
	res, err := e.UnmountSecret(ctx, "web", "", "KUBECONFIG")
	if err != nil {
		t.Fatalf("UnmountSecret: %v", err)
	}
	if res.AnyFileOnly() {
		t.Fatalf("mounts = %+v, want nothing file-only once the file is gone", res.Mounts)
	}
	if _, enumerated := secretEnvKeys(t, k, "web"); enumerated {
		t.Error("the app still enumerates after unmounting its only file-only key")
	}
	keys, err := e.ListSecrets(ctx, "web", "")
	if err != nil {
		t.Fatalf("ListSecrets: %v", err)
	}
	if !slices.Contains(keys, "KUBECONFIG") {
		t.Errorf("secret keys = %v, want KUBECONFIG still set: an unmount removes a file, not a value", keys)
	}
}

// TestSetSecretOfANewKeyReachesAnEnumeratedApp is the behaviour §4 changes, and the reason it is
// named in the record rather than left to be discovered. envFrom picks up whatever the Secret holds,
// so a restart was enough; an enumerated template names each key, so a new one only arrives if the
// workload is re-applied.
func TestSetSecretOfANewKeyReachesAnEnumeratedApp(t *testing.T) {
	ctx := context.Background()
	e, k := mountedApp(t)
	if _, err := e.MountSecret(ctx, "web", "", "KUBECONFIG", "", "", boolPtr(true)); err != nil {
		t.Fatalf("MountSecret --no-env: %v", err)
	}

	if err := e.SetSecret(ctx, "web", "", "SENTRY_DSN", "https://sentry.example", false); err != nil {
		t.Fatalf("SetSecret: %v", err)
	}
	keys, _ := secretEnvKeys(t, k, "web")
	if !slices.Contains(keys, "SENTRY_DSN") {
		t.Errorf("enumerated keys = %v after setting a new key; on an enumerated app a restart rolls the pod without it, so `secret set` has to re-apply the workload", keys)
	}
	// And a removal leaves the template saying what the Secret says.
	if err := e.UnsetSecret(ctx, "web", "", "SENTRY_DSN", false); err != nil {
		t.Fatalf("UnsetSecret: %v", err)
	}
	keys, _ = secretEnvKeys(t, k, "web")
	if slices.Contains(keys, "SENTRY_DSN") {
		t.Errorf("enumerated keys = %v after unsetting the key, want it gone from the template too", keys)
	}
}

// TestSetSecretKeepsTheAnnotationBumpOnTheFastPath: the app that changed nothing keeps the cheap
// path. A `secret set` there is one annotation patch, not a workload reapply.
func TestSetSecretKeepsTheAnnotationBumpOnTheFastPath(t *testing.T) {
	ctx := context.Background()
	e, k := mountedApp(t)
	if _, err := e.MountSecret(ctx, "web", "", "KUBECONFIG", "", "", nil); err != nil {
		t.Fatalf("MountSecret: %v", err)
	}

	if err := e.SetSecret(ctx, "web", "", "SENTRY_DSN", "https://sentry.example", false); err != nil {
		t.Fatalf("SetSecret: %v", err)
	}
	if _, rolled := k.RestartedAt("web"); !rolled {
		t.Error("a `secret set` on an app with no file-only key must still roll it by bumping the restart annotation")
	}
	keys, _ := secretEnvKeys(t, k, "web")
	if len(keys) != 0 {
		t.Errorf("the app enumerates %v, want nothing: envFrom already delivers every key the Secret holds", keys)
	}
}

// TestFileOnlySurvivesARollback: §5 applies to the marking as much as to the file. Rolling back to a
// release cut before the key was taken out of the environment must not put it back — the running
// code reads the file, and a rollback is an incident escape hatch, not a change of configuration.
func TestFileOnlySurvivesARollback(t *testing.T) {
	ctx := context.Background()
	e, k := mountedApp(t)
	if _, err := e.Deploy(ctx, cp.DeployRequest{App: "web", Image: "img:2", Replicas: 1}); err != nil {
		t.Fatalf("deploy v2: %v", err)
	}
	if _, err := e.MountSecret(ctx, "web", "", "KUBECONFIG", "", "", boolPtr(true)); err != nil {
		t.Fatalf("MountSecret --no-env: %v", err)
	}

	if _, err := e.Rollback(ctx, "web", "", cp.RollbackOptions{}); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	keys, enumerated := secretEnvKeys(t, k, "web")
	if !enumerated || slices.Contains(keys, "KUBECONFIG") {
		t.Errorf("after a rollback the app delivers %v as variables (enumerated=%v); a rollback must not put a credential back in the environment", keys, enumerated)
	}
}

// TestRunDoesNotPutAFileOnlyKeyBackInTheEnvironment. A one-off run is the app's own image with the
// app's own environment (ADR-0048 §2) — and it is where an environment variable is most dangerous,
// because a run is what starts a shell and a variable is inherited by every child process. A Job that
// sourced the Secret wholesale would put the credential back exactly where the app took it out of.
func TestRunDoesNotPutAFileOnlyKeyBackInTheEnvironment(t *testing.T) {
	ctx := context.Background()
	e, k := mountedApp(t)
	if _, err := e.MountSecret(ctx, "web", "", "KUBECONFIG", "", "", boolPtr(true)); err != nil {
		t.Fatalf("MountSecret --no-env: %v", err)
	}

	if _, err := e.Run(ctx, cp.RunRequest{App: "web", Command: []string{"sh", "-c", "env"}, Confirm: true}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	runs := k.RunJobs()
	if len(runs) != 1 {
		t.Fatalf("run jobs = %d, want one", len(runs))
	}
	if slices.Contains(runs[0].SecretEnvKeys, "KUBECONFIG") {
		t.Errorf("the run Job's environment names %v; a file-only key must not come back as a variable in the one process most likely to hand it to a child",
			runs[0].SecretEnvKeys)
	}
	if !slices.Equal(runs[0].SecretEnvKeys, []string{"DATABASE_URL", "STRIPE_SECRET_KEY"}) {
		t.Errorf("the run Job's secret environment = %v, want the keys that are still variables — a run sees what the app sees", runs[0].SecretEnvKeys)
	}
	// And it reaches the command as the FILE it is, or the credential would be unreachable in a run.
	if !slices.Equal(runs[0].SecretFiles.Keys(), []string{"KUBECONFIG"}) {
		t.Errorf("the run Job projects %v as files, want KUBECONFIG", runs[0].SecretFiles.Keys())
	}
}

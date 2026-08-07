// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package controlplane_test

import (
	"context"
	"strings"
	"testing"

	cp "github.com/burrow-cloud/burrow/controlplane"
	"github.com/burrow-cloud/burrow/controlplane/internal/fake"
)

// A secret key projected as a file (ADR-0089). The engine-side properties, in the order the record
// argues them: only mounted keys reach the workload, the key must already exist, a mount is app
// configuration rather than a release property, and nothing on this path ever holds a value.

// mountedKeys returns the keys the app's currently-applied workload projects as files.
func mountedKeys(t *testing.T, k *fake.Kubernetes, app string) []string {
	t.Helper()
	spec, ok := k.Spec(app)
	if !ok {
		t.Fatalf("no workload applied for %q", app)
	}
	return spec.SecretFiles.Keys()
}

// TestMountProjectsOnlyTheKeyItNames: a mount reaches the running workload, and the other keys the
// app holds stay out of it. This is the store-to-WorkloadSpec half of the property the adapter test
// pins on the pod template.
func TestMountProjectsOnlyTheKeyItNames(t *testing.T) {
	ctx := context.Background()
	e, k, _, _ := newEngine(t, permissive())
	k.SetSecret("web", "GOOGLE_CREDENTIALS", "{\"type\":\"service_account\"}")
	k.SetSecret("web", "STRIPE_SECRET_KEY", "sk_live_xyz")
	if _, err := e.Deploy(ctx, cp.DeployRequest{App: "web", Image: "img:1", Replicas: 1}); err != nil {
		t.Fatalf("deploy: %v", err)
	}

	res, err := e.MountSecret(ctx, "web", "", "GOOGLE_CREDENTIALS", "", "")
	if err != nil {
		t.Fatalf("MountSecret: %v", err)
	}
	if res.Directory() != cp.DefaultSecretsDir {
		t.Errorf("directory = %q, want %q", res.Directory(), cp.DefaultSecretsDir)
	}
	if len(res.Mounts) != 1 || res.Mounts[0].Filename != "GOOGLE_CREDENTIALS" {
		t.Fatalf("mounts = %+v, want one, named after the key", res.Mounts)
	}
	got := mountedKeys(t, k, "web")
	if len(got) != 1 || got[0] != "GOOGLE_CREDENTIALS" {
		t.Errorf("workload projects %v, want only the mounted key — an unmounted key must not land on disk", got)
	}
}

// TestMountRefusesAKeyThatIsNotSet. Mounting a key that was never set produces an app that starts,
// finds no file, and fails at the moment it needs the credential — the failure the record exists to
// avoid making easy. The refusal names the key and points at the command that sets it.
func TestMountRefusesAKeyThatIsNotSet(t *testing.T) {
	ctx := context.Background()
	e, k, _, _ := newEngine(t, permissive())
	k.SetSecret("web", "STRIPE_SECRET_KEY", "sk_live_xyz")
	if _, err := e.Deploy(ctx, cp.DeployRequest{App: "web", Image: "img:1", Replicas: 1}); err != nil {
		t.Fatalf("deploy: %v", err)
	}

	_, err := e.MountSecret(ctx, "web", "", "TLS_KEY", "", "")
	if err == nil {
		t.Fatal("MountSecret of an unset key succeeded, want a refusal")
	}
	if !strings.Contains(err.Error(), "TLS_KEY") || !strings.Contains(err.Error(), "burrow app secret set") {
		t.Errorf("error = %q, want it to name the key and the command that sets it", err)
	}
	if got := mountedKeys(t, k, "web"); len(got) != 0 {
		t.Errorf("a refused mount reached the workload: %v", got)
	}
}

// TestMountFilenameIsOneSegment. `..` in a projected path is how a mount escapes the directory
// Burrow owns, which is what every other property here rests on.
func TestMountFilenameIsOneSegment(t *testing.T) {
	ctx := context.Background()
	e, k, _, _ := newEngine(t, permissive())
	k.SetSecret("web", "TLS_KEY", "-----BEGIN PRIVATE KEY-----")

	for _, name := range []string{"sub/tls.key", "../../etc/passwd", "..", "."} {
		if _, err := e.MountSecret(ctx, "web", "", "TLS_KEY", name, ""); err == nil {
			t.Errorf("--filename %q was accepted, want a refusal", name)
		}
	}
	// The record's own example, and the ordinary case, both stay legal.
	if _, err := e.MountSecret(ctx, "web", "", "TLS_KEY", "tls.key", ""); err != nil {
		t.Errorf("--filename tls.key: %v", err)
	}
}

// TestMountDirIsPerAppAndValidated. `--dir` moves the directory for the whole app; there is no
// per-key form, and the type has nowhere to put one. A relative or unclean path is refused, and so
// is `/`, which would hide the whole image behind the volume.
func TestMountDirIsPerAppAndValidated(t *testing.T) {
	ctx := context.Background()
	e, k, _, _ := newEngine(t, permissive())
	k.SetSecret("web", "TLS_KEY", "-----BEGIN PRIVATE KEY-----")
	k.SetSecret("web", "TLS_CERT", "-----BEGIN CERTIFICATE-----")

	for _, dir := range []string{"etc/app", "/etc/app/../secrets", "/"} {
		if _, err := e.MountSecret(ctx, "web", "", "TLS_KEY", "", dir); err == nil {
			t.Errorf("--dir %q was accepted, want a refusal", dir)
		}
	}
	if _, err := e.MountSecret(ctx, "web", "", "TLS_KEY", "", "/etc/app/secrets"); err != nil {
		t.Fatalf("MountSecret: %v", err)
	}
	// The second key inherits the app's directory rather than carrying one of its own.
	res, err := e.MountSecret(ctx, "web", "", "TLS_CERT", "", "")
	if err != nil {
		t.Fatalf("MountSecret: %v", err)
	}
	if res.Directory() != "/etc/app/secrets" {
		t.Errorf("directory = %q, want the app's override to apply to every mounted key", res.Directory())
	}
}

// TestUnmountLeavesTheValueAlone. An unmount removes a FILE. The key stays set and stays in the
// app's environment, which is what makes the verb reversible and safe on the agent surface.
func TestUnmountLeavesTheValueAlone(t *testing.T) {
	ctx := context.Background()
	e, k, _, _ := newEngine(t, permissive())
	k.SetSecret("web", "TLS_KEY", "-----BEGIN PRIVATE KEY-----")
	if _, err := e.Deploy(ctx, cp.DeployRequest{App: "web", Image: "img:1", Replicas: 1}); err != nil {
		t.Fatalf("deploy: %v", err)
	}
	if _, err := e.MountSecret(ctx, "web", "", "TLS_KEY", "tls.key", ""); err != nil {
		t.Fatalf("MountSecret: %v", err)
	}

	res, err := e.UnmountSecret(ctx, "web", "", "TLS_KEY")
	if err != nil {
		t.Fatalf("UnmountSecret: %v", err)
	}
	if len(res.Mounts) != 0 {
		t.Errorf("mounts = %+v, want none", res.Mounts)
	}
	if got := mountedKeys(t, k, "web"); len(got) != 0 {
		t.Errorf("workload still projects %v after an unmount", got)
	}
	keys, err := e.ListSecrets(ctx, "web", "")
	if err != nil {
		t.Fatalf("ListSecrets: %v", err)
	}
	if len(keys) != 1 || keys[0] != "TLS_KEY" {
		t.Errorf("secret keys = %v, want TLS_KEY still set: an unmount removes a file, not a value", keys)
	}
	// Unmounting a key that is not mounted is the state the app is already in, not an error.
	if _, err := e.UnmountSecret(ctx, "web", "", "TLS_KEY"); err != nil {
		t.Errorf("second UnmountSecret: %v", err)
	}
}

// TestMountSurvivesARollbackToAnEarlierRelease is ADR-0089 §5, and the reason a mount is app
// configuration rather than a release property. The mount is made AFTER v2 is cut and the rollback
// target is v1 — a release from before the mount existed. If the mount rode the release, the
// rollback would take the credential the running code needs with it.
func TestMountSurvivesARollbackToAnEarlierRelease(t *testing.T) {
	ctx := context.Background()
	e, k, _, _ := newEngine(t, permissive())
	k.SetSecret("web", "GOOGLE_CREDENTIALS", "{\"type\":\"service_account\"}")

	if _, err := e.Deploy(ctx, cp.DeployRequest{App: "web", Image: "img:1", Replicas: 1}); err != nil {
		t.Fatalf("deploy v1: %v", err)
	}
	if _, err := e.Deploy(ctx, cp.DeployRequest{App: "web", Image: "img:2", Replicas: 1}); err != nil {
		t.Fatalf("deploy v2: %v", err)
	}
	if _, err := e.MountSecret(ctx, "web", "", "GOOGLE_CREDENTIALS", "creds.json", ""); err != nil {
		t.Fatalf("MountSecret: %v", err)
	}

	if _, err := e.Rollback(ctx, "web", "", cp.RollbackOptions{}); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	spec, _ := k.Spec("web")
	if spec.Image != "img:1" {
		t.Fatalf("cluster image = %q, want img:1", spec.Image)
	}
	if len(spec.SecretFiles.Mounts) != 1 || spec.SecretFiles.Mounts[0].Filename != "creds.json" {
		t.Errorf("after a rollback to a release cut before the mount existed, the workload projects %+v — "+
			"the rollback took the credential with it (ADR-0089 §5)", spec.SecretFiles.Mounts)
	}
}

// TestMountReachesTheNextDeploy: with no running release there is nothing to roll, so the mount is
// persisted and lands on the next deploy — the same shape `config set` has.
func TestMountReachesTheNextDeploy(t *testing.T) {
	ctx := context.Background()
	e, k, _, _ := newEngine(t, permissive())
	k.SetSecret("web", "TLS_KEY", "-----BEGIN PRIVATE KEY-----")

	if _, err := e.MountSecret(ctx, "web", "", "TLS_KEY", "tls.key", ""); err != nil {
		t.Fatalf("MountSecret before any deploy: %v", err)
	}
	if _, err := e.Deploy(ctx, cp.DeployRequest{App: "web", Image: "img:1", Replicas: 1}); err != nil {
		t.Fatalf("deploy: %v", err)
	}
	if got := mountedKeys(t, k, "web"); len(got) != 1 || got[0] != "TLS_KEY" {
		t.Errorf("workload projects %v, want the mount made before the first deploy", got)
	}
}

// TestMountIsPerEnvironment: the same app in two environments holds two different Secrets, so what
// it projects out of them is per environment too.
func TestMountIsPerEnvironment(t *testing.T) {
	ctx := context.Background()
	e, k, d, _ := newEngine(t, permissive())
	if err := d.CreateEnvironment(ctx, "staging", "burrow-staging"); err != nil {
		t.Fatalf("CreateEnvironment: %v", err)
	}
	k.WithNamespace("burrow-staging").(*fake.Kubernetes).SetSecret("web", "TLS_KEY", "-----BEGIN PRIVATE KEY-----")

	if _, err := e.MountSecret(ctx, "web", "staging", "TLS_KEY", "", ""); err != nil {
		t.Fatalf("MountSecret(staging): %v", err)
	}
	prod, err := e.SecretMounts(ctx, "web", "")
	if err != nil {
		t.Fatalf("SecretMounts(prod): %v", err)
	}
	if len(prod.Mounts) != 0 {
		t.Errorf("the default environment projects %+v, want nothing: a mount is per environment", prod.Mounts)
	}
}

// TestMountAuditsNothingAndHoldsNoValue. ADR-0029 holds unchanged: a mount is a key name and a
// filename, so there is no value for a log line or an audit row to carry. This asserts the shape
// rather than a log capture — the paths that could hold a value are the audit trail and the stored
// record, and neither has anywhere to put one.
func TestMountAuditsNothingAndHoldsNoValue(t *testing.T) {
	ctx := context.Background()
	e, k, d, _ := newEngine(t, permissive())
	const value = "-----BEGIN PRIVATE KEY-----super-secret"
	k.SetSecret("web", "TLS_KEY", value)
	if _, err := e.Deploy(ctx, cp.DeployRequest{App: "web", Image: "img:1", Replicas: 1}); err != nil {
		t.Fatalf("deploy: %v", err)
	}
	before := d.AuditRows()
	if _, err := e.MountSecret(ctx, "web", "", "TLS_KEY", "tls.key", ""); err != nil {
		t.Fatalf("MountSecret: %v", err)
	}
	if _, err := e.UnmountSecret(ctx, "web", "", "TLS_KEY"); err != nil {
		t.Fatalf("UnmountSecret: %v", err)
	}
	after := d.AuditRows()
	// Mount and unmount sit in the ungated config/secret neighbourhood, which records no decision
	// rows today (ADR-0089 §7). Whatever the trail holds, it must never hold the value.
	for _, entry := range after {
		for k, v := range entry.Args {
			if strings.Contains(v, value) {
				t.Fatalf("audit entry arg %q carries the secret value", k)
			}
		}
	}
	if len(after) != len(before) {
		t.Errorf("mount/unmount wrote %d audit row(s); they are ungated and audit nothing today, like `secret set` beside them",
			len(after)-len(before))
	}
}

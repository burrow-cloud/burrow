// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package kube

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/burrow-cloud/burrow/controlplane"
)

// kubeTrace is everything an operation did to a cluster, in order, across BOTH clients — the typed
// one the password Secret is written through and the dynamic one the provisioning objects are
// written through.
//
// It is one interleaved list rather than two, because the interleaving is load-bearing: the Secret
// goes before the `DatabaseRole` that references it, and a `DatabaseRole` naming a Secret that does
// not exist yet is the operator racing the control plane into a failure. Two separate traces would
// agree while that ordering had moved.
type kubeTrace struct {
	mu      sync.Mutex
	entries []string
}

// objectAction and namedAction are the two shapes of client-go action this reads. They are declared
// structurally rather than matched against k8stesting's CreateAction/UpdateAction, whose interfaces
// are identical and so match each other's implementations.
type (
	objectAction interface{ GetObject() runtime.Object }
	namedAction  interface{ GetName() string }
)

// watch installs a recording reactor on both fake clients. Every reactor returns "not handled", so
// the recording changes nothing about what the operation does — the trace is of the real run.
func (tr *kubeTrace) watch(t *testing.T, p *PostgresProvisioner, dyn *dynamicfake.FakeDynamicClient) {
	t.Helper()
	record := func(action k8stesting.Action) (bool, runtime.Object, error) {
		entry := action.GetVerb() + " " + action.GetResource().Resource + " " + action.GetNamespace() + "/"
		if a, ok := action.(objectAction); ok {
			obj := a.GetObject()
			m, ok := obj.(metav1.Object)
			if !ok {
				t.Fatalf("a %s action carried a %T, which has no name", action.GetVerb(), obj)
			}
			entry += m.GetName() + " " + renderObject(t, obj)
		} else if a, ok := action.(namedAction); ok {
			entry += a.GetName()
		}
		tr.mu.Lock()
		tr.entries = append(tr.entries, entry)
		tr.mu.Unlock()
		return false, nil, nil
	}
	dyn.PrependReactor("*", "*", record)
	p.client.(*fake.Clientset).PrependReactor("*", "*", record)
}

// renderObject is what a written object SAID, reduced to the parts an attachment is made of: the
// spec and the descriptive labels of a provisioning object, and the shape of the password Secret.
//
// The generated password is REDACTED rather than compared. It is fresh on every attach and every
// detach, so two runs of the same code produce different bytes there and nothing else; comparing it
// would fail every time while comparing nothing.
func renderObject(t *testing.T, obj runtime.Object) string {
	t.Helper()
	switch o := obj.(type) {
	case *unstructured.Unstructured:
		return renderJSON(t, map[string]any{"labels": o.GetLabels(), "spec": o.Object["spec"]})
	case *corev1.Secret:
		data := map[string]string{}
		for k, v := range o.Data {
			data[k] = string(v)
			if k == PostgresPasswordKey {
				data[k] = "<a generated password>"
				if len(v) == 0 {
					data[k] = "<empty>"
				}
			}
		}
		return renderJSON(t, map[string]any{"type": string(o.Type), "labels": o.Labels, "data": data})
	}
	t.Fatalf("an object of type %T was written and this test does not know how to compare it", obj)
	return ""
}

func renderJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v) // json.Marshal sorts map keys, so the rendering is stable
	if err != nil {
		t.Fatalf("rendering an object: %v", err)
	}
	return string(b)
}

// attachAndDetach performs one full attach and one plain detach against a fresh set of fakes and
// returns what the cluster saw, what SQL was run, and how it was run.
//
// The two dispositions it can be asked for are the whole point of the comparison below: `substitute`
// swaps ONLY the runner of the connected steps, which is the one thing an embedder replaces.
func attachAndDetach(t *testing.T, substitute bool) (trace []string, statements []string, steps []PostgresConnectedStep, dsns int) {
	t.Helper()
	ctx := context.Background()
	p, dyn, rec := provisionerFor(t, addonNS)
	if substitute {
		p.WithConnectedStep(func(_ context.Context, step PostgresConnectedStep) error {
			steps = append(steps, step)
			statements = append(statements, step.Statements...)
			return nil
		})
	}
	tr := &kubeTrace{}
	tr.watch(t, p, dyn)

	if _, err := p.EnsureAppDatabase(ctx, "web", controlplane.DefaultEnvironment, testInstance(controlplane.DefaultEnvironment)); err != nil {
		t.Fatalf("EnsureAppDatabase (substituted=%v): %v", substitute, err)
	}
	if err := p.RevokeAppDatabase(ctx, "web", controlplane.DefaultEnvironment, testInstance(controlplane.DefaultEnvironment)); err != nil {
		t.Fatalf("RevokeAppDatabase (substituted=%v): %v", substitute, err)
	}
	if !substitute {
		statements = append(statements, rec.statements...)
	}
	return tr.entries, statements, steps, len(rec.dsns)
}

// TestASubstitutedConnectedStepProvisionsExactlyWhatTheDefaultDoes is the property issue #532 is
// about, and the reason the seam is a runner rather than a second EnsureAppDatabase: an embedder
// that performs the connected steps somewhere else gets the SAME attachment — the same objects,
// under the same names, holding the same specs, written in the same order, with the same statements
// asked for against the same credential.
//
// It establishes that by running both paths against one harness and comparing what the cluster saw,
// rather than by checking each against a hand-written expectation. Two hand-written expectations are
// exactly the duplication this closes: they agree until somebody edits one of them, which is how a
// detach came to hand a database to a role that did not exist.
func TestASubstitutedConnectedStepProvisionsExactlyWhatTheDefaultDoes(t *testing.T) {
	byDefault, defaultSQL, _, defaultDSNs := attachAndDetach(t, false)
	substituted, substitutedSQL, steps, substitutedDSNs := attachAndDetach(t, true)

	if len(byDefault) == 0 {
		t.Fatal("the default path touched the cluster not at all; the comparison below would be vacuous")
	}
	if len(byDefault) != len(substituted) {
		t.Fatalf("the default path performed %d cluster operations and the substituted path %d:\n default: %s\n substituted: %s",
			len(byDefault), len(substituted), strings.Join(byDefault, "\n          "), strings.Join(substituted, "\n              "))
	}
	for i := range byDefault {
		if byDefault[i] != substituted[i] {
			t.Fatalf("cluster operation %d differs between the two paths\n default: %s\n substituted: %s\nan embedder replaces HOW a connected step runs, never what an attachment is", i, byDefault[i], substituted[i])
		}
	}

	// And the same SQL, in the same order. The substituted path never ran it — it was ASKED to, which
	// is the other half of the claim: the statements are decided here, not there.
	if !reflect.DeepEqual(defaultSQL, substitutedSQL) {
		t.Errorf("the default path ran %q and the substituted path was asked for %q", defaultSQL, substitutedSQL)
	}
	if len(steps) != 2 {
		t.Fatalf("the substituted runner was handed %d steps, want the attach's revoke and the detach's release", len(steps))
	}
	// The connection is the difference and the only difference: one path opened some, the other
	// opened none, which is what an embedder with no route to the instance needs to be true.
	if defaultDSNs == 0 {
		t.Error("the default path opened no connection at all")
	}
	if substitutedDSNs != 0 {
		t.Errorf("the substituted path opened %d connections; the whole reason to substitute is that it cannot", substitutedDSNs)
	}
}

// TestAConnectedStepSaysWhatItIsAndAsWhom asserts a step is a description rather than an opaque
// string of SQL. An implementation running it in another cluster has to know which step this is,
// which database it is against, and which credential to spend — and must not have to re-derive any
// of those from the statement text or from a naming rule it keeps its own copy of.
func TestAConnectedStepSaysWhatItIsAndAsWhom(t *testing.T) {
	ctx := context.Background()
	p, _, _ := provisionerFor(t, addonNS)
	var steps []PostgresConnectedStep
	// The Secret is read AS THE STEP IS HANDED OVER, not afterwards: a detach deletes it once the role
	// is gone, and the claim being made is that an implementation which mounts it finds it there at
	// the moment it is asked to run.
	var secrets []*corev1.Secret
	p.WithConnectedStep(func(ctx context.Context, step PostgresConnectedStep) error {
		steps = append(steps, step)
		sec, err := p.client.CoreV1().Secrets(step.Target.Namespace).Get(ctx, step.PasswordSecret, metav1.GetOptions{})
		if err != nil {
			t.Errorf("step %q names a password secret that is not there when it runs: %v", step.Kind, err)
			sec = &corev1.Secret{}
		}
		secrets = append(secrets, sec)
		return nil
	})

	if _, err := p.EnsureAppDatabase(ctx, "web", controlplane.DefaultEnvironment, testInstance(controlplane.DefaultEnvironment)); err != nil {
		t.Fatalf("EnsureAppDatabase: %v", err)
	}
	if err := p.RevokeAppDatabase(ctx, "web", controlplane.DefaultEnvironment, testInstance(controlplane.DefaultEnvironment)); err != nil {
		t.Fatalf("RevokeAppDatabase: %v", err)
	}
	if len(steps) != 2 {
		t.Fatalf("%d steps were handed over, want the attach's and the detach's", len(steps))
	}

	attach, detach := steps[0], steps[1]
	if attach.Kind != PostgresRevokePublicConnect {
		t.Errorf("the attach step is %q, want %q", attach.Kind, PostgresRevokePublicConnect)
	}
	if detach.Kind != PostgresReleaseOwnedObjects {
		t.Errorf("the detach step is %q, want %q", detach.Kind, PostgresReleaseOwnedObjects)
	}
	if got := attach.Statements; len(got) != 1 || got[0] != `REVOKE CONNECT ON DATABASE "web" FROM PUBLIC` {
		t.Errorf("the attach step asks for %q", got)
	}
	// Both statements of the detach arrive as ONE step, in order. Handed over separately, the embedder
	// would be the one deciding that DROP OWNED follows REASSIGN OWNED — and run alone it destroys the
	// rows a plain detach exists to keep.
	want := []string{`REASSIGN OWNED BY "app_web" TO "app_web_data"`, `DROP OWNED BY "app_web"`}
	if !reflect.DeepEqual(detach.Statements, want) {
		t.Errorf("the detach step asks for %q, want %q", detach.Statements, want)
	}

	for i, step := range steps {
		if step.App != "web" || step.Database != "web" {
			t.Errorf("step %q names app %q and database %q, want web", step.Kind, step.App, step.Database)
		}
		if step.Environment != controlplane.DefaultEnvironment {
			t.Errorf("step %q names environment %q", step.Kind, step.Environment)
		}
		// As the app's own role, never the superuser: the credential is named in the step so the
		// decision is not the embedder's to make.
		if step.Role != "app_web" {
			t.Errorf("step %q runs as %q, want the app's own login role app_web", step.Kind, step.Role)
		}
		if step.Target.Instance != PostgresSecretName || step.Target.Namespace != addonNS {
			t.Errorf("step %q names instance %s/%s", step.Kind, step.Target.Namespace, step.Target.Instance)
		}
		if step.Labels[addonEnvLabel] != controlplane.DefaultEnvironment || step.Labels[addonLabel] != string(controlplane.AddonPostgres) {
			t.Errorf("step %q carries labels %v, so anything an implementation creates for it is unattributable", step.Kind, step.Labels)
		}
		// A window to retry a refused credential in. Zero would leave an implementation to invent one,
		// and what is being waited for — CloudNativePG reloading a Secret onto the role — takes as long
		// from a Job as it does from here.
		if step.CredentialTimeout != p.credentialTimeout {
			t.Errorf("step %q carries a credential timeout of %s, want the provisioner's %s", step.Kind, step.CredentialTimeout, p.credentialTimeout)
		}

		// The credential reaches an implementation two ways, and they must be the same credential: the
		// password itself, and the name of the Secret CloudNativePG applies to the role — which is what
		// a step performed in a pod mounts rather than carrying the value.
		if step.PasswordSecret != provisioningObjectName(PostgresSecretName, "web") {
			t.Errorf("step %q names password secret %q, want the attachment's own", step.Kind, step.PasswordSecret)
		}
		sec := secrets[i]
		if step.Password == "" || step.Password != string(sec.Data[PostgresPasswordKey]) {
			t.Errorf("step %q carries a password other than the one in the Secret the operator reads", step.Kind)
		}
		if string(sec.Data[cnpgSecretUsernameKey]) != step.Role {
			t.Errorf("step %q runs as %q but its Secret is for %q", step.Kind, step.Role, sec.Data[cnpgSecretUsernameKey])
		}
	}
}

// TestAConnectedStepThatDidNotRunFailsTheOperation is what stops a substituted runner from being a
// way to skip the steps. The revoke is the whole of an app's database isolation and the release is
// what lets a login role be dropped without taking the rows with it; an implementation that could
// not perform one says so, and the operation it was part of must fail rather than report success.
func TestAConnectedStepThatDidNotRunFailsTheOperation(t *testing.T) {
	ctx := context.Background()
	p, dyn, _ := provisionerFor(t, addonNS)
	refused := errors.New("the job could not reach the instance")
	p.WithConnectedStep(func(context.Context, PostgresConnectedStep) error { return refused })

	dsn, err := p.EnsureAppDatabase(ctx, "web", controlplane.DefaultEnvironment, testInstance(controlplane.DefaultEnvironment))
	if !errors.Is(err, refused) {
		t.Fatalf("EnsureAppDatabase err = %v, want the runner's refusal", err)
	}
	if dsn != "" {
		t.Error("an attach whose revoke did not run returned a connection string; the database still carries PostgreSQL's default CONNECT for PUBLIC")
	}
	if !strings.Contains(err.Error(), "revoking public connect") {
		t.Errorf("the failure %q does not say which step did not happen", err)
	}

	if err := p.RevokeAppDatabase(ctx, "web", controlplane.DefaultEnvironment, testInstance(controlplane.DefaultEnvironment)); !errors.Is(err, refused) {
		t.Fatalf("RevokeAppDatabase err = %v, want the runner's refusal", err)
	}
	// And the login role's object is still there, which is the honest state: nothing dropped the role,
	// so nothing may claim the app's access has ended.
	if _, err := dyn.Resource(cnpgDatabaseRoleGVR).Namespace(addonNS).
		Get(ctx, provisioningObjectName(PostgresSecretName, "web"), metav1.GetOptions{}); err != nil {
		t.Errorf("a detach whose release did not run deleted the login role anyway: %v", err)
	}
}

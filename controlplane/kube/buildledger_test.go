// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package kube

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"

	"github.com/burrow-cloud/burrow/controlplane"
)

// The record that outlives the caller (issue #504). A build Job runs for minutes and outlives the
// request that created it; before this it knew nothing about why it had been started, so a client
// that went away took the whole result with it. These tests assert the two halves of the fix: the
// intent is written onto the Job, and a Job that succeeded with nobody left to use it is readable
// back as a stranded build.

var testIntent = controlplane.BuildIntent{App: "shop", Env: "prod", Image: "registry.example/shop:build"}

// runningJobClient returns a fake whose build Job is never observed terminal, so a wait falls through
// to the caller's context — the shape of a client that disconnects while the build runs on.
func runningJobClient() *fake.Clientset {
	client := fake.NewSimpleClientset()
	client.PrependReactor("get", "jobs", func(a clienttesting.Action) (bool, runtime.Object, error) {
		name := a.(clienttesting.GetAction).GetName()
		return true, &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: buildNamespace}}, nil
	})
	return client
}

// storedJob reads a Job straight out of the fake's tracker, past any Get reactor.
func storedJob(t *testing.T, client *fake.Clientset, name string) *batchv1.Job {
	t.Helper()
	list, err := client.BatchV1().Jobs(buildNamespace).List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatalf("list jobs: %v", err)
	}
	for i := range list.Items {
		if list.Items[i].Name == name {
			return list.Items[i].DeepCopy()
		}
	}
	t.Fatalf("build job %q was not created", name)
	return nil
}

// completeStoredJob marks a stored Job Complete at completedAt and seeds the pod carrying its digest
// — the Job carrying on to success after its caller has gone.
func completeStoredJob(t *testing.T, client *fake.Clientset, name string, completedAt time.Time, digest string) {
	t.Helper()
	job := storedJob(t, client, name)
	job.Status.Succeeded = 1
	job.Status.CompletionTime = &metav1.Time{Time: completedAt}
	if _, err := client.BatchV1().Jobs(buildNamespace).Update(context.Background(), job, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("update job: %v", err)
	}
	if digest != "" {
		seedDigestPod(t, client, name, digest)
	}
}

// TestBuildRecordsWhatItIsForOnTheJob asserts the intent is written onto the Job at creation. The Job
// is the only thing that survives a disconnect, so it is the only place the intent can live.
func TestBuildRecordsWhatItIsForOnTheJob(t *testing.T) {
	source := controlplane.SourceRef{Repo: "https://github.com/acme/shop", Ref: "abc123"}
	const target = "reg.burrow.svc/shop:build"
	client, created := buildFakeSucceeding(t, source, target, validDigest)

	b := NewBuilder(client)
	if _, err := b.BuildAttributed(context.Background(), testIntent, source, target, false, controlplane.SourceCredential{}, nil); err != nil {
		t.Fatalf("BuildAttributed: %v", err)
	}
	if len(*created) != 1 {
		t.Fatalf("created %d jobs, want 1", len(*created))
	}
	job := (*created)[0]
	if got := job.Labels[buildAppLabel]; got != testIntent.App {
		t.Errorf("job label %s = %q, want %q", buildAppLabel, got, testIntent.App)
	}
	if got := job.Labels[buildEnvLabel]; got != testIntent.Env {
		t.Errorf("job label %s = %q, want %q", buildEnvLabel, got, testIntent.Env)
	}
	if got := job.Annotations[buildImageAnnotation]; got != testIntent.Image {
		t.Errorf("job annotation %s = %q, want %q", buildImageAnnotation, got, testIntent.Image)
	}
	// The pod template is how this adapter finds a build's pods; nothing about the deploy belongs on it.
	if _, ok := job.Spec.Template.Labels[buildAppLabel]; ok {
		t.Errorf("the pod template carries %s; the intent belongs on the Job alone", buildAppLabel)
	}
}

// TestAnUnattributedBuildRecordsNothing asserts the plain Builder seam is unchanged: a caller that
// recorded no intent creates exactly the Job it always created.
func TestAnUnattributedBuildRecordsNothing(t *testing.T) {
	source := controlplane.SourceRef{Repo: "https://github.com/acme/shop", Ref: "abc123"}
	const target = "reg.burrow.svc/shop:build"
	client, created := buildFakeSucceeding(t, source, target, validDigest)

	b := NewBuilder(client)
	if _, err := b.Build(context.Background(), source, target, false, controlplane.SourceCredential{}); err != nil {
		t.Fatalf("Build: %v", err)
	}
	job := (*created)[0]
	if _, ok := job.Labels[buildAppLabel]; ok {
		t.Errorf("an unattributed build recorded %s", buildAppLabel)
	}
	if len(job.Annotations) != 0 {
		t.Errorf("an unattributed build recorded annotations %v", job.Annotations)
	}
}

// TestAnIntentThatIsNotALegalLabelDoesNotFailTheBuild asserts the ordering of the two harms. The app
// and the environment arrive as caller data; a Job the apiserver rejects for a malformed label would
// turn a build that used to run into a server error, which is worse than the build being
// unrecoverable. The intent is dropped and the build proceeds.
func TestAnIntentThatIsNotALegalLabelDoesNotFailTheBuild(t *testing.T) {
	source := controlplane.SourceRef{Repo: "https://github.com/acme/shop", Ref: "abc123"}
	const target = "reg.burrow.svc/shop:build"
	client, created := buildFakeSucceeding(t, source, target, validDigest)

	bad := controlplane.BuildIntent{App: "shop", Env: "an environment/with slashes", Image: "registry.example/shop:build"}
	b := NewBuilder(client)
	digest, err := b.BuildAttributed(context.Background(), bad, source, target, false, controlplane.SourceCredential{}, nil)
	if err != nil {
		t.Fatalf("BuildAttributed: %v", err)
	}
	if digest != validDigest {
		t.Errorf("digest = %q, want the build to have run normally", digest)
	}
	if _, ok := (*created)[0].Labels[buildAppLabel]; ok {
		t.Error("an intent that is not a legal label value was written to the Job anyway")
	}
}

// TestABuildIsSharedAcrossEnvironments asserts a KNOWN LIMIT rather than leaving it ambiguous. A
// build's identity is its repo, ref, and target — not its app or environment — so building the same
// source to the same target for two environments reuses one Job, which is what keeps the second one
// from paying to build a byte-identical image. The recorded intent is therefore the LATEST caller's,
// and if both callers disconnect only the latest one's deploy is recovered.
func TestABuildIsSharedAcrossEnvironments(t *testing.T) {
	source := controlplane.SourceRef{Repo: "https://github.com/acme/shop", Ref: "abc123"}
	const target = "reg.burrow.svc/shop:build"
	client := runningJobClient()
	name := buildJobName(source, target)
	b := NewBuilder(client)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for _, env := range []string{"staging", "prod"} {
		intent := controlplane.BuildIntent{App: "shop", Env: env, Image: "registry.example/shop:build"}
		if _, err := b.BuildAttributed(ctx, intent, source, target, false, controlplane.SourceCredential{}, nil); !errors.Is(err, context.Canceled) {
			t.Fatalf("BuildAttributed(%s) err = %v, want context.Canceled", env, err)
		}
	}
	completeStoredJob(t, client, name, time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC), validDigest)

	stranded, err := b.StrandedBuilds(context.Background(), time.Date(2026, 8, 7, 13, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("StrandedBuilds: %v", err)
	}
	if len(stranded) != 1 {
		t.Fatalf("stranded = %d, want one Job serving both environments", len(stranded))
	}
	if stranded[0].Env != "prod" {
		t.Errorf("recovered environment = %q, want the latest caller's; the earlier one's deploy is the known limit", stranded[0].Env)
	}
}

// TestABuildOutlivesItsClient is the bug (issue #504). The client goes away while the build runs;
// the Job carries on to success, because it is a Kubernetes object with a life of its own. What must
// survive with it is the knowledge of what it was for — and it does: the build is readable back as
// stranded, with the app, the environment, the deploy reference, and the digest it produced.
func TestABuildOutlivesItsClient(t *testing.T) {
	source := controlplane.SourceRef{Repo: "https://github.com/acme/shop", Ref: "abc123"}
	const target = "reg.burrow.svc/shop:build"
	client := runningJobClient()
	name := buildJobName(source, target)

	// The client disconnects: the request's context is cancelled while the Job is still running.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	b := NewBuilder(client)
	if _, err := b.BuildAttributed(ctx, testIntent, source, target, false, controlplane.SourceCredential{}, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("BuildAttributed err = %v, want context.Canceled", err)
	}

	// The Job carries on and succeeds with nobody attached.
	completed := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)
	completeStoredJob(t, client, name, completed, validDigest)

	stranded, err := b.StrandedBuilds(context.Background(), completed.Add(2*time.Minute))
	if err != nil {
		t.Fatalf("StrandedBuilds: %v", err)
	}
	if len(stranded) != 1 {
		t.Fatalf("stranded = %d builds, want the one whose client went away", len(stranded))
	}
	got := stranded[0]
	want := controlplane.StrandedBuild{ID: name, App: "shop", Env: "prod", Image: testIntent.Image, Digest: validDigest, CompletedAt: completed}
	if got.ID != want.ID || got.App != want.App || got.Env != want.Env || got.Image != want.Image || got.Digest != want.Digest || !got.CompletedAt.Equal(want.CompletedAt) {
		t.Errorf("stranded build = %+v, want %+v", got, want)
	}
}

// TestStrandedBuildsReportsOnlyWhatCanBeFinished walks the conditions that keep a build off the list.
// Each one is load-bearing: a build still running has a caller, a build inside the settling margin
// may still have one, a build with no digest has nothing to deploy, a held build's decision has
// already been recorded, and a build with no intent cannot be finished by anyone.
func TestStrandedBuildsReportsOnlyWhatCanBeFinished(t *testing.T) {
	completed := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)
	cutoff := completed.Add(2 * time.Minute)

	cases := []struct {
		name    string
		mutate  func(t *testing.T, client *fake.Clientset, b *BuildAdapter, jobName string)
		wantLen int
	}{
		{
			name:    "a successful build nobody finished",
			mutate:  func(*testing.T, *fake.Clientset, *BuildAdapter, string) {},
			wantLen: 1,
		},
		{
			name: "a build still running",
			mutate: func(t *testing.T, client *fake.Clientset, _ *BuildAdapter, name string) {
				job := storedJob(t, client, name)
				job.Status.Succeeded = 0
				job.Status.CompletionTime = nil
				if _, err := client.BatchV1().Jobs(buildNamespace).Update(context.Background(), job, metav1.UpdateOptions{}); err != nil {
					t.Fatalf("update: %v", err)
				}
			},
			wantLen: 0,
		},
		{
			name: "a build that finished inside the settling margin",
			mutate: func(t *testing.T, client *fake.Clientset, _ *BuildAdapter, name string) {
				job := storedJob(t, client, name)
				job.Status.CompletionTime = &metav1.Time{Time: cutoff.Add(time.Minute)}
				if _, err := client.BatchV1().Jobs(buildNamespace).Update(context.Background(), job, metav1.UpdateOptions{}); err != nil {
					t.Fatalf("update: %v", err)
				}
			},
			wantLen: 0,
		},
		{
			name: "a build whose digest cannot be read",
			mutate: func(t *testing.T, client *fake.Clientset, _ *BuildAdapter, name string) {
				if err := client.CoreV1().Pods(buildNamespace).Delete(context.Background(), name+"-abc", metav1.DeleteOptions{}); err != nil {
					t.Fatalf("delete pod: %v", err)
				}
			},
			wantLen: 0,
		},
		{
			name: "a build already held",
			mutate: func(t *testing.T, _ *fake.Clientset, b *BuildAdapter, name string) {
				if err := b.HoldBuild(context.Background(), name, "app.deploy"); err != nil {
					t.Fatalf("HoldBuild: %v", err)
				}
			},
			wantLen: 0,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			source := controlplane.SourceRef{Repo: "https://github.com/acme/shop", Ref: "abc123"}
			const target = "reg.burrow.svc/shop:build"
			client := runningJobClient()
			name := buildJobName(source, target)
			b := NewBuilder(client)

			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			if _, err := b.BuildAttributed(ctx, testIntent, source, target, false, controlplane.SourceCredential{}, nil); !errors.Is(err, context.Canceled) {
				t.Fatalf("BuildAttributed err = %v, want context.Canceled", err)
			}
			completeStoredJob(t, client, name, completed, validDigest)
			tc.mutate(t, client, b, name)

			stranded, err := b.StrandedBuilds(context.Background(), cutoff)
			if err != nil {
				t.Fatalf("StrandedBuilds: %v", err)
			}
			if len(stranded) != tc.wantLen {
				t.Errorf("stranded = %d, want %d (%+v)", len(stranded), tc.wantLen, stranded)
			}
		})
	}
}

// TestStrandedBuildsIgnoresABuildWithNoIntent asserts a build started by a control plane that
// predates attribution is left alone rather than picked up and failed at. It is the known limit,
// asserted rather than left ambiguous.
func TestStrandedBuildsIgnoresABuildWithNoIntent(t *testing.T) {
	source := controlplane.SourceRef{Repo: "https://github.com/acme/shop", Ref: "abc123"}
	const target = "reg.burrow.svc/shop:build"
	client := runningJobClient()
	name := buildJobName(source, target)
	b := NewBuilder(client)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := b.Build(ctx, source, target, false, controlplane.SourceCredential{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("Build err = %v, want context.Canceled", err)
	}
	completed := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)
	completeStoredJob(t, client, name, completed, validDigest)

	stranded, err := b.StrandedBuilds(context.Background(), completed.Add(2*time.Minute))
	if err != nil {
		t.Fatalf("StrandedBuilds: %v", err)
	}
	if len(stranded) != 0 {
		t.Errorf("stranded = %+v, want nothing: a build with no recorded intent cannot be finished", stranded)
	}
}

// TestReapBuildToleratesAnAlreadyReapedBuild asserts the TTL controller and the reconciler are both
// allowed to be the one that removed a Job.
func TestReapBuildToleratesAnAlreadyReapedBuild(t *testing.T) {
	b := NewBuilder(fake.NewSimpleClientset())
	if err := b.ReapBuild(context.Background(), "burrow-build-gone"); err != nil {
		t.Errorf("ReapBuild on a missing job = %v, want nil", err)
	}
}

// TestHoldBuildMarksTheJobAndLeavesIt asserts the two halves of a hold: the decision is recorded on
// the build so it is not re-recorded on every sweep, and the build itself is NOT discarded — the
// image is good and already paid for, so a confirmed re-run must be able to reuse it.
func TestHoldBuildMarksTheJobAndLeavesIt(t *testing.T) {
	source := controlplane.SourceRef{Repo: "https://github.com/acme/shop", Ref: "abc123"}
	const target = "reg.burrow.svc/shop:build"
	client := runningJobClient()
	name := buildJobName(source, target)
	b := NewBuilder(client)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := b.BuildAttributed(ctx, testIntent, source, target, false, controlplane.SourceCredential{}, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("BuildAttributed err = %v, want context.Canceled", err)
	}
	completeStoredJob(t, client, name, time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC), validDigest)

	if err := b.HoldBuild(context.Background(), name, "app.deploy"); err != nil {
		t.Fatalf("HoldBuild: %v", err)
	}
	job := storedJob(t, client, name)
	if got := job.Annotations[buildHeldAnnotation]; got != "app.deploy" {
		t.Errorf("held annotation = %q, want the guardrail code", got)
	}
	if job.Status.Succeeded != 1 {
		t.Error("a held build was not left in place")
	}
}

// TestReusingABuildRestampsItAndClearsTheHold asserts that asking for the same build again — the
// deterministic-name reuse path, which is how a confirmed re-run avoids paying to build the same
// image twice — records this run's intent and clears any hold a previous unattended attempt left.
func TestReusingABuildRestampsItAndClearsTheHold(t *testing.T) {
	source := controlplane.SourceRef{Repo: "https://github.com/acme/shop", Ref: "abc123"}
	const target = "reg.burrow.svc/shop:build"
	name := buildJobName(source, target)

	client := fake.NewSimpleClientset(&batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: buildNamespace,
			Labels:      map[string]string{buildAppLabel: "shop", buildEnvLabel: "staging"},
			Annotations: map[string]string{buildImageAnnotation: "registry.example/shop:old", buildHeldAnnotation: "app.deploy"},
		},
		Status: batchv1.JobStatus{Succeeded: 1},
	})
	client.PrependReactor("create", "jobs", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewAlreadyExists(schema.GroupResource{Group: "batch", Resource: "jobs"}, name)
	})
	client.PrependReactor("get", "jobs", func(a clienttesting.Action) (bool, runtime.Object, error) {
		return true, &batchv1.Job{
			ObjectMeta: metav1.ObjectMeta{Name: a.(clienttesting.GetAction).GetName(), Namespace: buildNamespace},
			Status:     batchv1.JobStatus{Succeeded: 1},
		}, nil
	})
	seedDigestPod(t, client, name, validDigest)

	b := NewBuilder(client)
	if _, err := b.BuildAttributed(context.Background(), testIntent, source, target, false, controlplane.SourceCredential{}, nil); err != nil {
		t.Fatalf("BuildAttributed: %v", err)
	}

	// The reuse reaps the Job on success, so read the patch off the tracker before that: assert on the
	// patch action itself, which is what actually crossed the wire.
	var patched bool
	for _, a := range client.Actions() {
		p, ok := a.(clienttesting.PatchAction)
		if !ok || p.GetName() != name {
			continue
		}
		patched = true
		body := string(p.GetPatch())
		for _, want := range []string{`"burrow.cloud/build-environment":"prod"`, `"burrow.cloud/build-image":"registry.example/shop:build"`, `"burrow.cloud/build-held":null`} {
			if !strings.Contains(body, want) {
				t.Errorf("intent patch = %s, want it to contain %s", body, want)
			}
		}
	}
	if !patched {
		t.Error("reusing a build did not re-record what it is for")
	}
}

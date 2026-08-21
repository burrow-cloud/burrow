// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package kube

import (
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/burrow-cloud/burrow/controlplane"
)

// [ADR-0073](../../docs/adr/0073-placement-policy-reaches-every-authored-pod.md) §1 is a STANDING
// obligation rather than a one-time change:
//
//	a new authored pod path arrives with a hook, or it arrives undeployable.
//
// This file enforces it. Four pod paths — the backup and restore Jobs, the add-on instance, the
// metrics collector — reached today's tree by nobody asking the question, and they were found by
// READING SOURCE rather than by any check failing. A convention that has already been missed four
// times is not a convention.
//
// The failure it guards against is invisible in review and quiet in production. A new builder that
// returns a fully-formed *batchv1.Job looks complete and correct; nothing about it says a hook is
// missing. The cost appears later, on a cluster whose only capacity is tainted or whose workloads
// must name a runtimeClassName, where the pod sits Pending forever — and a Pending Job leaves both
// Failed and Succeeded at zero, so the waiter burns its full timeout and reports a timeout rather
// than an unschedulable pod. Usually that is someone else's cluster.
//
// # What is enforced, and why it is per-path rather than "has a hook"
//
// ADR-0073 §2 splits the reach by WHOSE IMAGE RUNS, and its Consequences call that classification
// load-bearing: a path assigned to the wrong hook gives the operator policy they did not intend,
// and the app hook on a Burrow-image pod would put Burrow's own workload on the tenant pool. So
// each path is asserted against the SPECIFIC hook its classification calls for — statically, at the
// call site, and behaviourally, by wiring every hook at once and checking exactly one reaches the
// authored pod. A test that only asked "is some hook wired?" would pass on precisely the mistake
// the split exists to prevent.
//
// # How the enumeration is kept honest
//
// The catalogue below is hand-written — the classification, the reason, and the way to exercise a
// path cannot be derived from source — but it is not TRUSTED. Which paths exist is derived, by
// parsing this package's own non-test source for every function that constructs a corev1.PodSpec
// literal (authoredPodSpecSites). The catalogue is then checked against that scan in both
// directions:
//
//   - A pod path in the source with no catalogue entry FAILS. That is the case this file exists
//     for: someone adds a builder, and the test tells them the three classifications and makes them
//     choose one.
//   - A catalogue entry with no matching source site FAILS. A path that is deleted, or a builder
//     that moves to another file or is renamed, cannot leave behind a stale entry that still
//     passes — the entry is keyed by "<file>:<function>", so the scan stops matching it.
//
// That is deliberately the opposite of "we will remember to update the list": remembering is what
// produced the four uncovered paths. The scan is plain go/ast over eight files in one package — no
// reflection, no build tags, no code generation — and it is legible enough that its own failure
// (say, a pod spec built somewhere it cannot see) surfaces as an entry it cannot match rather than
// as silence.
//
// What the scan CANNOT check is whether a stated classification is true: an author who wires the
// app hook on a Burrow-image pod and writes "the app's own image" in the catalogue is consistent
// and wrong. That decision is made once per path, in this repository, and ADR-0073's Consequences
// say it belongs in the doc comment at the call site where a reader meets it. What this file
// guarantees is that the decision is MADE, stated, and matches what the code actually does.
//
// The catalogue lives here rather than in package code because this is its only reader — unlike
// internal/agentsurface, whose catalogue `guard` also reports at runtime.
//
// # What this file does NOT cover
//
// It scans for corev1.PodSpec literals, so its subject is pods Burrow ASSEMBLES.
// [ADR-0077](../../docs/adr/0077-placement-policy-for-pods-burrow-does-not-author.md) §1 restates
// ADR-0073 §1 to every pod Burrow CAUSES TO EXIST, which includes pods a third-party controller
// composes from a custom resource Burrow creates. Those have no pod spec to scan for and no hook to
// apply; they carry placement through WithControllerPodPlacement and are guarded by
// placement_test.go and placement_cnpg_test.go. Reading §1 through this file alone would read
// "authored" narrowly, which is the gap ADR-0077 exists to close.

// placementHook names one of ADR-0073's three operator hooks, by the method that wires it.
type placementHook string

const (
	// hookApp is Adapter.WithAppPodMutator: pods running the APP's own image (ADR-0061, ADR-0073 §2).
	// Adapter.WithPodMutator is the same hook under its earlier, identity-free spelling.
	hookApp placementHook = "WithAppPodMutator"
	// hookPlatform is Adapter.WithPlatformPodMutator: pods running BURROW's own images on Burrow's
	// behalf (ADR-0073 §2).
	hookPlatform placementHook = "WithPlatformPodMutator"
	// hookBuild is BuildAdapter.WithBuildPodMutator: Burrow's builder image over the app's source,
	// a third hook for a third case (ADR-0053 §6, ADR-0073 §2).
	hookBuild placementHook = "WithBuildPodMutator"
)

// authoredPodPath is one pod path the engine authors: which function builds it, whose image runs in
// it, which hook that classification calls for, and how to exercise the real path end to end.
type authoredPodPath struct {
	// name is the path in ADR-0073's own words ("app Deployment", "backup Job").
	name string
	// file and builder locate the function that constructs the pod spec. Together they key the
	// entry against the source scan, so a builder that is renamed or moved to another file surfaces
	// as a stale entry rather than passing on a name that no longer means what it did.
	file    string
	builder string
	// image is the ADR-0073 §2 classification in one line: whose image runs in this pod. It is the
	// claim the hook follows from, and the thing a reviewer checks.
	image string
	// hook is the hook this path must carry, given image.
	hook placementHook
	// exercise runs the real path with ALL hooks wired to distinct markers and returns the pod spec
	// that reached the API server, so the assertion is on authored output rather than on the
	// presence of a call.
	exercise func(t *testing.T) corev1.PodSpec
}

// key is how an entry is matched to the source scan.
func (p authoredPodPath) key() string { return p.file + ":" + p.builder }

// authoredPodPaths is every pod path this package authors, with the hook each carries. Adding a
// pod path means adding an entry here; the source scan below makes that mandatory rather than
// remembered.
var authoredPodPaths = []authoredPodPath{
	{
		name:     "app Deployment",
		file:     "adapter.go",
		builder:  "buildDeployment",
		image:    "the app's own image, in the app's namespace",
		hook:     hookApp,
		exercise: exerciseAppDeployment,
	},
	{
		name:     "one-off run Job",
		file:     "run.go",
		builder:  "runJob",
		image:    "the app's own image — the same workload, for one command (ADR-0048)",
		hook:     hookApp,
		exercise: exerciseRunJob,
	},
	{
		name:     "build Job",
		file:     "build.go",
		builder:  "buildJob",
		image:    "Burrow's builder image over the app's source, with its own security context (ADR-0056)",
		hook:     hookBuild,
		exercise: exerciseBuildJob,
	},
	{
		// Backup and restore share backupJob, so they share a placement policy; they are listed
		// separately because they are separate paths for an operator, and the restore is the one
		// discovered during an incident.
		name:     "backup Job",
		file:     "backups.go",
		builder:  "backupJob",
		image:    "Burrow's own image (postgres), running pg_dump on Burrow's behalf",
		hook:     hookPlatform,
		exercise: exerciseBackupJob,
	},
	{
		name:     "restore Job",
		file:     "backups.go",
		builder:  "backupJob",
		image:    "Burrow's own image (postgres), running pg_restore on Burrow's behalf",
		hook:     hookPlatform,
		exercise: exerciseRestoreJob,
	},
	{
		name:     "add-on instance Deployment",
		file:     "addons.go",
		builder:  "DeployAddon",
		image:    "an image Burrow chose (Postgres, VictoriaLogs, VictoriaMetrics, Valkey), not the app's",
		hook:     hookPlatform,
		exercise: exerciseAddonInstance,
	},
	{
		name:     "log-collector DaemonSet",
		file:     "addons.go",
		builder:  "deployLogsCollector",
		image:    "Fluent Bit, Burrow's image, on every node (its blanket toleration stays — ADR-0073 §3)",
		hook:     hookPlatform,
		exercise: exerciseLogsCollector,
	},
	{
		name:     "metrics-collector Deployment",
		file:     "addons.go",
		builder:  "deployMetricsCollector",
		image:    "vmagent, Burrow's image — it scrapes the tenant's pods but is not one of them",
		hook:     hookPlatform,
		exercise: exerciseMetricsCollector,
	},
}

// authoredPodPathRationale is appended to every failure in this file so the message teaches the
// decision rather than merely reporting a mismatch. ADR-0073's Consequences make the classification
// load-bearing, so a contributor who hits this must be able to pick the RIGHT hook rather than
// whichever one makes the test pass.
const authoredPodPathRationale = `
Why this test exists:

  ADR-0073 §1: "a new authored pod path arrives with a hook, or it arrives undeployable."

  A pod the operator cannot adjust cannot run on a cluster with rules of its own. On a cluster whose
  only capacity is tainted, or which mandates a runtimeClassName, an authored pod with no hook sits
  Pending forever — and quietly: a Pending Job leaves Failed and Succeeded at zero, so the waiter
  reports a timeout rather than an unschedulable pod, and a Pending Deployment reports zero ready
  replicas, which reads like a slow start.

  Four paths reached the tree without a hook because nobody asked the question in review, where a
  fully-formed *batchv1.Job looks complete and correct. This test asks it for you.

What to do — pick by WHOSE IMAGE runs in the pod (ADR-0073 §2):

  - The APP's own image (the Deployment's pod template, a one-off run Job): apply the app hook,
    Adapter.WithAppPodMutator, via "a.applyAppPodMutator(PodIdentity{...}, &spec)". The identity
    names the app the pod belongs to and the namespace it is being written into, both of which the
    builder already has; a hook that is not told them can only carry policy true of every app.
  - BURROW's own image, on Burrow's behalf (an add-on instance, a collector, a backup or restore
    Job): apply the platform hook, Adapter.WithPlatformPodMutator, via
    "a.applyPlatformPodMutator(&spec)".
  - Burrow's BUILDER image over the app's source: apply BuildAdapter.WithBuildPodMutator. This is
    the build path only; it has its own security context (ADR-0056) that no other path shares.

  The choice is load-bearing and not interchangeable: the app hook on a Burrow-image pod puts
  Burrow's own workload on the tenant pool, which is policy the operator did not ask for.

Then, in this file:

  - Apply the hook LAST, over the fully-constructed pod spec, on every write (ADR-0073 §6).
  - Add an entry to authoredPodPaths naming the file, the builder function, whose image runs, the
    hook, and an exercise that drives the real path.
  - State the classification in a doc comment at the call site, where the next reader meets it.

Wiring nothing sandboxes nothing (ADR-0073 §5): a nil mutator must leave the object byte-for-byte
unchanged (§4). The hook makes the operator's policy REACH the pod; it does not enforce isolation.`

// hookMarkerKeys are the nodeSelector keys each hook's marker mutator sets. One key per hook rather
// than one shared key with three values: a pod reached by two hooks must be visible as such, and a
// shared key would let the second write hide the first.
var hookMarkerKeys = map[placementHook]string{
	hookApp:      "burrow.test/app-hook",
	hookPlatform: "burrow.test/platform-hook",
	hookBuild:    "burrow.test/build-hook",
}

// markPod is a mutator that records which hook it was wired to on the pod it reaches. It satisfies
// ADR-0073 §6's obligations on a real mutator — idempotent (setting a map key twice is setting it
// once) and tolerant of a pod spec it did not author (it adds a key rather than replacing the
// nodeSelector, so the log collector's blanket toleration and a Job's RestartPolicy are untouched).
func markPod(h placementHook) func(*corev1.PodSpec) {
	return func(pod *corev1.PodSpec) {
		if pod.NodeSelector == nil {
			pod.NodeSelector = map[string]string{}
		}
		pod.NodeSelector[hookMarkerKeys[h]] = string(h)
	}
}

// hooksThatReached reports which hooks left their marker on a pod, sorted for a stable message.
func hooksThatReached(pod corev1.PodSpec) []placementHook {
	var got []placementHook
	for h, key := range hookMarkerKeys {
		if pod.NodeSelector[key] != "" {
			got = append(got, h)
		}
	}
	sort.Slice(got, func(i, j int) bool { return got[i] < got[j] })
	return got
}

// markedAdapter is an Adapter with BOTH of its hooks wired, each to its own marker. Wiring both at
// once is what makes the assertion specific: the test can tell "the platform hook reached this pod"
// from "a hook reached this pod", and misclassification fails rather than passing.
//
// The app hook is wired through WithAppPodMutator, the spelling a new path should use. What it is
// told about the pod is not asserted here — this guard is about WHICH hook reaches WHICH pod, and
// the identity itself is pinned in app_pod_identity_test.go.
func markedAdapter(client kubernetes.Interface) *Adapter {
	markApp := markPod(hookApp)
	return New(client, guardAppNamespace).
		WithAddonNamespace(addonNS).
		WithAppPodMutator(func(_ PodIdentity, pod *corev1.PodSpec) error { markApp(pod); return nil }).
		WithPlatformPodMutator(markPod(hookPlatform))
}

// guardAppNamespace is the app namespace the exercises below deploy into.
const guardAppNamespace = "apps"

func exerciseAppDeployment(t *testing.T) corev1.PodSpec {
	t.Helper()
	ctx := context.Background()
	client := fake.NewSimpleClientset()
	if err := markedAdapter(client).ApplyWorkload(ctx, controlplane.WorkloadSpec{App: "shop", Image: "shop:v1", Replicas: 1}); err != nil {
		t.Fatalf("ApplyWorkload: %v", err)
	}
	dep, err := client.AppsV1().Deployments(guardAppNamespace).Get(ctx, "shop", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get app Deployment: %v", err)
	}
	return dep.Spec.Template.Spec
}

func exerciseRunJob(t *testing.T) corev1.PodSpec {
	t.Helper()
	ctx := context.Background()
	client := fake.NewSimpleClientset()
	var jobs []*batchv1.Job
	succeedJobs(client, &jobs)
	spec := controlplane.RunSpec{App: "shop", ID: "r1", Image: "shop:v1", Command: []string{"echo", "hi"}}
	if _, err := markedAdapter(client).RunJob(ctx, spec); err != nil {
		t.Fatalf("RunJob: %v", err)
	}
	return onlyJob(t, jobs).Spec.Template.Spec
}

func exerciseBuildJob(t *testing.T) corev1.PodSpec {
	t.Helper()
	ctx := context.Background()
	source := controlplane.SourceRef{Repo: "https://github.com/acme/shop", Ref: "v1"}
	const target = "reg.burrow.svc/acme/shop:1"
	client, created := buildFakeSucceeding(t, source, target, validDigest)
	// The build hook is the only one BuildAdapter has: the app and platform hooks are not wirable
	// here at all, so this path's "no other hook reached it" is structural rather than asserted.
	b := NewBuilder(client).WithBuildPodMutator(markPod(hookBuild))
	if _, err := b.Build(ctx, source, target, false, controlplane.SourceCredential{}); err != nil {
		t.Fatalf("Build: %v", err)
	}
	return onlyJob(t, *created).Spec.Template.Spec
}

func exerciseBackupJob(t *testing.T) corev1.PodSpec {
	t.Helper()
	ctx := context.Background()
	client := fake.NewSimpleClientset()
	var jobs []*batchv1.Job
	succeedJobs(client, &jobs)
	if _, err := markedAdapter(client).RunBackupJob(ctx, "shop", controlplane.DefaultEnvironment, testInstance(controlplane.DefaultEnvironment), "bk1", nil); err != nil {
		t.Fatalf("RunBackupJob: %v", err)
	}
	return onlyJob(t, jobs).Spec.Template.Spec
}

func exerciseRestoreJob(t *testing.T) corev1.PodSpec {
	t.Helper()
	ctx := context.Background()
	client := fake.NewSimpleClientset()
	var jobs []*batchv1.Job
	succeedJobs(client, &jobs)
	if err := markedAdapter(client).RunRestoreJob(ctx, "shop", controlplane.DefaultEnvironment, testInstance(controlplane.DefaultEnvironment), "bk1"); err != nil {
		t.Fatalf("RunRestoreJob: %v", err)
	}
	return onlyJob(t, jobs).Spec.Template.Spec
}

func exerciseAddonInstance(t *testing.T) corev1.PodSpec {
	t.Helper()
	ctx := context.Background()
	client := fake.NewSimpleClientset()
	spec := controlplane.AddonSpec{Type: controlplane.AddonCache, Backend: "valkey", Image: "valkey:test", Port: 6379}
	if _, err := markedAdapter(client).DeployAddon(ctx, spec, controlplane.DefaultEnvironment, testInstanceOf(spec, controlplane.DefaultEnvironment), nil); err != nil {
		t.Fatalf("DeployAddon: %v", err)
	}
	dep, err := client.AppsV1().Deployments(addonNS).Get(ctx, "burrow-cache", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get add-on Deployment: %v", err)
	}
	return dep.Spec.Template.Spec
}

func exerciseLogsCollector(t *testing.T) corev1.PodSpec {
	t.Helper()
	ctx := context.Background()
	client := fake.NewSimpleClientset()
	spec := controlplane.AddonSpec{Type: controlplane.AddonLogs, Backend: "victorialogs", Image: "victoria-logs:test", Port: 9428}
	if _, err := markedAdapter(client).DeployAddon(ctx, spec, controlplane.DefaultEnvironment, testInstanceOf(spec, controlplane.DefaultEnvironment), nil); err != nil {
		t.Fatalf("DeployAddon: %v", err)
	}
	ds, err := client.AppsV1().DaemonSets(addonNS).Get(ctx, "burrow-logs-collector", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get collector DaemonSet: %v", err)
	}
	return ds.Spec.Template.Spec
}

func exerciseMetricsCollector(t *testing.T) corev1.PodSpec {
	t.Helper()
	ctx := context.Background()
	// The vmagent ServiceAccount is staged by the CLI at install time; without it the deploy is
	// refused before anything is created.
	client := fake.NewSimpleClientset(&corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{Name: vmagentServiceAccount, Namespace: addonNS},
	})
	spec := controlplane.AddonSpec{Type: controlplane.AddonMetrics, Backend: "victoriametrics", Image: "victoria-metrics:test", Port: 8428}
	if _, err := markedAdapter(client).DeployAddon(ctx, spec, controlplane.DefaultEnvironment, testInstanceOf(spec, controlplane.DefaultEnvironment), nil); err != nil {
		t.Fatalf("DeployAddon: %v", err)
	}
	dep, err := client.AppsV1().Deployments(addonNS).Get(ctx, "burrow-metrics-collector", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get metrics collector: %v", err)
	}
	return dep.Spec.Template.Spec
}

// onlyJob returns the single Job the exercised path created, failing if the count is not one — a
// path that created none would otherwise read as a path with no hook.
func onlyJob(t *testing.T, jobs []*batchv1.Job) *batchv1.Job {
	t.Helper()
	if len(jobs) != 1 {
		t.Fatalf("the path created %d Jobs, want exactly 1", len(jobs))
	}
	return jobs[0]
}

// TestEveryAuthoredPodPathIsCatalogued is the anti-drift half: which pod paths exist is DERIVED
// from this package's source, and the catalogue must match it exactly in both directions. A new
// builder with no entry fails here with the classification it has to choose from; an entry whose
// builder was renamed, moved, or deleted fails as stale rather than sitting there passing.
func TestEveryAuthoredPodPathIsCatalogued(t *testing.T) {
	sites := authoredPodSpecSites(t)

	catalogued := map[string]bool{}
	for _, p := range authoredPodPaths {
		catalogued[p.key()] = true
	}

	for _, key := range sortedKeysOf(sites) {
		if catalogued[key] {
			continue
		}
		site := sites[key]
		t.Errorf("%s:%d — %s constructs a pod spec and is not in authoredPodPaths.\n"+
			"Every pod Burrow authors must be reachable by operator placement policy, and which hook "+
			"it takes depends on whose image runs in it.%s",
			site.file, site.line, site.fn, authoredPodPathRationale)
	}

	for _, p := range authoredPodPaths {
		if _, ok := sites[p.key()]; ok {
			continue
		}
		t.Errorf("authoredPodPaths lists %q as built by %s, but no function of that name in that file "+
			"constructs a pod spec. The path was removed, renamed, or moved — update the entry (or "+
			"delete it) so the enumeration keeps describing the tree rather than the tree it used to "+
			"describe.%s", p.name, p.key(), authoredPodPathRationale)
	}
}

// TestEveryAuthoredPodPathAppliesItsClassifiedHook checks the call site itself: the function that
// builds each pod spec applies exactly one hook, and it is the one its classification calls for. It
// is the check that fires on the failure this file exists for — a new builder with no hook at all —
// and it names the file and line, so the fix is where the message points.
func TestEveryAuthoredPodPathAppliesItsClassifiedHook(t *testing.T) {
	sites := authoredPodSpecSites(t)
	// Keyed by builder rather than by path, because one builder can serve two paths: backup and
	// restore share backupJob, so they necessarily share a placement policy, and a mismatch there
	// is a mismatch for both.
	want := map[string][]authoredPodPath{}
	for _, p := range authoredPodPaths {
		want[p.key()] = append(want[p.key()], p)
	}

	for _, key := range sortedKeysOf(sites) {
		site := sites[key]
		switch len(site.hooks) {
		case 0:
			t.Errorf("%s:%d — %s constructs a pod spec and applies NO placement hook, so the pod it "+
				"authors carries no toleration, no runtimeClassName, and no nodeSelector on any cluster, "+
				"ever.%s", site.file, site.line, site.fn, authoredPodPathRationale)
			continue
		case 1:
		default:
			t.Errorf("%s:%d — %s applies more than one placement hook (%v). A pod belongs to exactly one "+
				"side of the ADR-0073 §2 split: whose image runs in it.%s",
				site.file, site.line, site.fn, site.hooks, authoredPodPathRationale)
			continue
		}
		for _, p := range want[key] { // absent from the catalogue: reported by the test above
			if site.hooks[0] == p.hook {
				continue
			}
			t.Errorf("%s:%d — the %s applies %s, but it runs %s, which takes %s (ADR-0073 §2). The wrong "+
				"hook gives the operator policy they did not intend: the app hook on a Burrow-image pod "+
				"puts Burrow's own workload on the tenant pool.%s",
				site.file, site.line, p.name, site.hooks[0], p.image, p.hook, authoredPodPathRationale)
		}
	}
}

// TestAuthoredPodPathIsReachedByItsClassifiedHookOnly drives each path for real, with every hook
// wired at once, and asserts exactly the classified one reaches the authored pod. The static check
// above sees a call; this sees the object that would go to the API server, so a hook applied to the
// wrong spec, applied before construction and overwritten, or dropped on a copy of the adapter
// fails here.
//
// It is specific on purpose. ADR-0073's Consequences call the classification load-bearing, and a
// test that asserted only "some hook reached it" would pass on a Burrow-image pod wired to the app
// hook — the exact mistake that puts Burrow's own workload on the tenant pool.
func TestAuthoredPodPathIsReachedByItsClassifiedHookOnly(t *testing.T) {
	for _, p := range authoredPodPaths {
		t.Run(p.name, func(t *testing.T) {
			pod := p.exercise(t)
			got := hooksThatReached(pod)
			if want := []placementHook{p.hook}; !reflect.DeepEqual(got, want) {
				t.Errorf("the %s pod was reached by %v, want exactly [%s].\n"+
					"It runs %s, so it takes %s (ADR-0073 §2).%s",
					p.name, got, p.hook, p.image, p.hook, authoredPodPathRationale)
			}
		})
	}
}

// TestAuthoredPodPathCatalogueIsWellFormed asserts each entry carries what a reader (and the two
// checks above) need. An entry with no exercise or no stated image degrades the guard to a name
// list: it would still fail when a path went missing, but it would stop saying which hook the path
// needs or proving that the hook reaches it.
func TestAuthoredPodPathCatalogueIsWellFormed(t *testing.T) {
	seen := map[string]bool{}
	for _, p := range authoredPodPaths {
		if strings.TrimSpace(p.name) == "" || strings.TrimSpace(p.file) == "" || strings.TrimSpace(p.builder) == "" {
			t.Errorf("catalogue entry is missing its name, file, or builder: %+v", p)
			continue
		}
		if seen[p.name] {
			t.Errorf("catalogue lists %q twice; one entry per pod path", p.name)
		}
		seen[p.name] = true
		if strings.TrimSpace(p.image) == "" {
			t.Errorf("%q does not say whose image runs in it, which is the ADR-0073 §2 classification the "+
				"hook follows from", p.name)
		}
		switch p.hook {
		case hookApp, hookPlatform, hookBuild:
		default:
			t.Errorf("%q carries hook %q, want one of %s, %s, %s", p.name, p.hook, hookApp, hookPlatform, hookBuild)
		}
		if p.exercise == nil {
			t.Errorf("%q has no exercise; without one the guard can assert that a hook is CALLED but not "+
				"that it reaches the authored pod", p.name)
		}
	}
}

// podSpecSite is one place in this package's source where a pod spec is constructed, and the
// placement hooks the same function applies.
type podSpecSite struct {
	file  string
	fn    string
	line  int
	hooks []placementHook
}

// authoredPodSpecSites parses this package's own non-test source and returns every function that
// constructs a corev1.PodSpec literal, keyed by "<file>:<function>".
//
// Deriving the enumeration rather than hand-listing it is the whole point: a hand-maintained list
// of pod paths is exactly what ADR-0073 found four gaps in. The scan is deliberately plain — it
// looks for a composite literal of the type named PodSpec in the package's core/v1 import, and for
// a call to applyPlatformPodMutator or to the podMutator field in the same function — so a reader
// can confirm what it does in a minute. Its own blind spots surface as failures rather than as
// silence: a builder that moves out of this package, or a pod spec assembled without a literal,
// leaves its catalogue entry unmatched and fails TestEveryAuthoredPodPathIsCatalogued.
func authoredPodSpecSites(t *testing.T) map[string]podSpecSite {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("reading the package directory: %v", err)
	}
	fset := token.NewFileSet()
	sites := map[string]podSpecSite{}
	var parsed int
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parsing %s: %v", name, err)
		}
		parsed++
		alias := coreV1Alias(f)
		if alias == "" {
			continue // the file cannot name corev1.PodSpec
		}
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			line, found := podSpecLiteralLine(fset, fn, alias)
			if !found {
				continue
			}
			key := name + ":" + fn.Name.Name
			if prev, dup := sites[key]; dup {
				t.Fatalf("two functions named %s construct a pod spec in %s (lines %d and %d); the scan "+
					"keys on file and name and cannot tell them apart", fn.Name.Name, name, prev.line, line)
			}
			sites[key] = podSpecSite{file: name, fn: fn.Name.Name, line: line, hooks: hooksAppliedIn(fn)}
		}
	}
	if parsed == 0 {
		t.Fatal("the source scan parsed no files, so it would report no pod paths and pass whatever the " +
			"tree contains; it must run with the package directory as its working directory")
	}
	return sites
}

// coreV1Alias returns the name this file binds k8s.io/api/core/v1 to, or "" if it does not import
// it. Resolving the alias rather than assuming "corev1" keeps the scan from being defeated by a
// rename — and from matching some other package's PodSpec.
func coreV1Alias(f *ast.File) string {
	for _, imp := range f.Imports {
		if imp.Path.Value != `"k8s.io/api/core/v1"` {
			continue
		}
		if imp.Name != nil {
			return imp.Name.Name
		}
		return "v1"
	}
	return ""
}

// podSpecLiteralLine reports the line of the first corev1.PodSpec composite literal in fn.
func podSpecLiteralLine(fset *token.FileSet, fn *ast.FuncDecl, alias string) (int, bool) {
	var line int
	var found bool
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		if found {
			return false
		}
		lit, ok := n.(*ast.CompositeLit)
		if !ok {
			return true
		}
		sel, ok := lit.Type.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "PodSpec" {
			return true
		}
		if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == alias {
			line, found = fset.Position(lit.Pos()).Line, true
			return false
		}
		return true
	})
	return line, found
}

// hooksAppliedIn reports which placement hooks fn applies, deduplicated. The app and platform hooks
// each have their own helper on Adapter, so they are recognized by name. The build hook is a call of
// a field called podMutator on BuildAdapter, and the same field name on Adapter is still recognized
// as the app hook, so a call site that applies the hook inline rather than through the helper is not
// invisible to the scan — the receiver tells the two apart, which is also the honest reading:
// Adapter's podMutator IS the app hook and BuildAdapter's IS the build hook.
func hooksAppliedIn(fn *ast.FuncDecl) []placementHook {
	recv := receiverTypeName(fn)
	seen := map[placementHook]bool{}
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		switch sel.Sel.Name {
		case "applyAppPodMutator":
			seen[hookApp] = true
		case "applyPlatformPodMutator":
			seen[hookPlatform] = true
		case "podMutator":
			switch recv {
			case "Adapter":
				seen[hookApp] = true
			case "BuildAdapter":
				seen[hookBuild] = true
			}
		}
		return true
	})
	var hooks []placementHook
	for h := range seen {
		hooks = append(hooks, h)
	}
	sort.Slice(hooks, func(i, j int) bool { return hooks[i] < hooks[j] })
	return hooks
}

// receiverTypeName is the (pointer-dereferenced) type name fn is a method on, or "" for a plain
// function.
func receiverTypeName(fn *ast.FuncDecl) string {
	if fn.Recv == nil || len(fn.Recv.List) == 0 {
		return ""
	}
	expr := fn.Recv.List[0].Type
	if star, ok := expr.(*ast.StarExpr); ok {
		expr = star.X
	}
	if id, ok := expr.(*ast.Ident); ok {
		return id.Name
	}
	return fmt.Sprintf("%v", expr)
}

// sortedKeysOf returns a map's keys in order, so failures are reported deterministically.
func sortedKeysOf(sites map[string]podSpecSite) []string {
	keys := make([]string, 0, len(sites))
	for k := range sites {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

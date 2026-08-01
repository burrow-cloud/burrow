// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package kube

import (
	"context"
	"reflect"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/burrow-cloud/burrow/controlplane"
)

// platformPlacement stands in for an operator's platform-pool policy: a node selector and a mandated
// runtime class, plus a toleration added only when an equal one is not already there. It satisfies
// both obligations ADR-0073 §6 puts on a platform mutator — it is idempotent, so running it on every
// write does not accumulate tolerations, and it tolerates a pod spec it did not author, so the log
// collector's blanket toleration survives it.
func platformPlacement() func(*corev1.PodSpec) {
	dedicated := corev1.Toleration{
		Key:      "dedicated",
		Operator: corev1.TolerationOpEqual,
		Value:    "platform",
		Effect:   corev1.TaintEffectNoSchedule,
	}
	return func(pod *corev1.PodSpec) {
		pod.NodeSelector = map[string]string{"pool": "platform"}
		pod.RuntimeClassName = ptrTo("kata")
		for _, t := range pod.Tolerations {
			if t == dedicated {
				return
			}
		}
		pod.Tolerations = append(pod.Tolerations, dedicated)
	}
}

// assertPlatformPlacement asserts the pod carries what platformPlacement supplies.
func assertPlatformPlacement(t *testing.T, what string, pod corev1.PodSpec) {
	t.Helper()
	if pod.NodeSelector["pool"] != "platform" {
		t.Errorf("%s nodeSelector = %v, want pool=platform", what, pod.NodeSelector)
	}
	if pod.RuntimeClassName == nil || *pod.RuntimeClassName != "kata" {
		t.Errorf("%s runtimeClassName = %v, want kata", what, pod.RuntimeClassName)
	}
	var found bool
	for _, tol := range pod.Tolerations {
		if tol.Key == "dedicated" && tol.Value == "platform" {
			found = true
		}
	}
	if !found {
		t.Errorf("%s tolerations = %+v, want one for the platform pool", what, pod.Tolerations)
	}
}

// assertNoPlacement asserts the pod carries no placement fields beyond the ones the engine itself
// hard-codes — the state ADR-0073 §4 requires when nothing is wired.
func assertNoPlacement(t *testing.T, what string, pod corev1.PodSpec) {
	t.Helper()
	if pod.RuntimeClassName != nil {
		t.Errorf("%s runtimeClassName = %q, want unset", what, *pod.RuntimeClassName)
	}
	if len(pod.NodeSelector) != 0 {
		t.Errorf("%s nodeSelector = %v, want none", what, pod.NodeSelector)
	}
	if pod.Affinity != nil {
		t.Errorf("%s affinity = %+v, want none", what, pod.Affinity)
	}
}

// TestPlatformPodMutatorReachesAddonInstance covers the add-on instance Deployment: a stateful store
// whose volume binds it to a node, so an operator whose only schedulable capacity is tainted
// otherwise has an add-on that sits Pending reporting zero ready replicas — which reads like a slow
// start rather than an unschedulable pod.
//
// The Postgres instance is deliberately NOT this case. Its pods are CloudNativePG's, composed from a
// `Cluster` rather than authored here, so there is no PodSpec to hand to this hook at all — it takes
// the placement seam ADR-0077 built, asserted by TestControllerPlacementReachesTheCluster.
func TestPlatformPodMutatorReachesAddonInstance(t *testing.T) {
	ctx := context.Background()
	client := fake.NewSimpleClientset()
	a := New(client, "apps").WithAddonNamespace(addonNS).WithPlatformPodMutator(platformPlacement())

	spec := controlplane.AddonSpec{Type: controlplane.AddonLogs, Backend: "victorialogs", Image: "victoria-logs:test", Port: 9428, StorageGi: 5}
	if _, err := a.DeployAddon(ctx, spec, controlplane.DefaultEnvironment, nil); err != nil {
		t.Fatalf("DeployAddon: %v", err)
	}
	dep, err := client.AppsV1().Deployments(addonNS).Get(ctx, "burrow-logs", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	assertPlatformPlacement(t, "logs add-on", dep.Spec.Template.Spec)
	// Placement is all the hook changed: the instance still runs its store.
	if len(dep.Spec.Template.Spec.Containers) != 1 {
		t.Errorf("containers = %d, want 1 (the store)", len(dep.Spec.Template.Spec.Containers))
	}
}

// TestPlatformPodMutatorReachesLogsCollector covers the DaemonSet, and with it ADR-0073 §3: the
// blanket `Operator: Exists` toleration is a decision, not an omission — a node-log collector that
// skips tainted nodes silently loses exactly those nodes' logs — so it stays, and the hook applies
// to the pod anyway.
func TestPlatformPodMutatorReachesLogsCollector(t *testing.T) {
	ctx := context.Background()
	client := fake.NewSimpleClientset()
	a := New(client, "apps").WithAddonNamespace(addonNS).WithPlatformPodMutator(platformPlacement())

	spec := controlplane.AddonSpec{Type: controlplane.AddonLogs, Backend: "victorialogs", Image: "victoria-logs:test", Port: 9428, StorageGi: 5}
	if _, err := a.DeployAddon(ctx, spec, controlplane.DefaultEnvironment, nil); err != nil {
		t.Fatalf("DeployAddon: %v", err)
	}
	ds, err := client.AppsV1().DaemonSets(addonNS).Get(ctx, "burrow-logs-collector", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Get daemonset: %v", err)
	}
	assertPlatformPlacement(t, "logs collector", ds.Spec.Template.Spec)

	// The blanket toleration survives an idempotent mutator that adds rather than replaces.
	var blanket bool
	for _, tol := range ds.Spec.Template.Spec.Tolerations {
		if tol.Operator == corev1.TolerationOpExists && tol.Key == "" {
			blanket = true
		}
	}
	if !blanket {
		t.Errorf("tolerations = %+v, want the blanket Operator:Exists toleration kept (ADR-0073 §3)", ds.Spec.Template.Spec.Tolerations)
	}
	// The store the collector ships to gets the same policy, being another Burrow-image pod.
	store, err := client.AppsV1().Deployments(addonNS).Get(ctx, "burrow-logs", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Get store: %v", err)
	}
	assertPlatformPlacement(t, "logs store", store.Spec.Template.Spec)
}

// TestPlatformPodMutatorReachesMetricsCollector covers the vmagent Deployment. It scrapes the
// tenant's pods but is not one of them, so it belongs wherever the operator puts Burrow's own
// workloads — and the hook must not cost it the pre-provisioned ServiceAccount its pod discovery
// depends on.
func TestPlatformPodMutatorReachesMetricsCollector(t *testing.T) {
	ctx := context.Background()
	// The vmagent ServiceAccount is staged by the CLI at install time; without it the deploy is
	// refused before anything is created.
	client := fake.NewSimpleClientset(&corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{Name: vmagentServiceAccount, Namespace: addonNS},
	})
	a := New(client, "apps").WithAddonNamespace(addonNS).WithPlatformPodMutator(platformPlacement())

	spec := controlplane.AddonSpec{Type: controlplane.AddonMetrics, Backend: "victoriametrics", Image: "victoria-metrics:test", Port: 8428, StorageGi: 10}
	if _, err := a.DeployAddon(ctx, spec, controlplane.DefaultEnvironment, nil); err != nil {
		t.Fatalf("DeployAddon: %v", err)
	}
	col, err := client.AppsV1().Deployments(addonNS).Get(ctx, "burrow-metrics-collector", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Get collector: %v", err)
	}
	assertPlatformPlacement(t, "metrics collector", col.Spec.Template.Spec)
	if col.Spec.Template.Spec.ServiceAccountName != vmagentServiceAccount {
		t.Errorf("collector serviceAccount = %q, want %q", col.Spec.Template.Spec.ServiceAccountName, vmagentServiceAccount)
	}
	store, err := client.AppsV1().Deployments(addonNS).Get(ctx, "burrow-metrics", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Get store: %v", err)
	}
	assertPlatformPlacement(t, "metrics store", store.Spec.Template.Spec)
}

// TestPlatformPodMutatorSeesConstructedPodSpec pins the ordering (ADR-0073 §6): the hook runs over
// the FULLY-constructed pod spec, so a mutator can key its decision off what the engine composed. On
// the DaemonSet that is also how a mutator author learns the collector arrives with a toleration
// already set — the spec it did not expect, and the one it must not clobber.
func TestPlatformPodMutatorSeesConstructedPodSpec(t *testing.T) {
	ctx := context.Background()
	client := fake.NewSimpleClientset()

	var sawFluentBit bool
	var sawTolerations int
	var sawConfigVolume bool
	a := New(client, "apps").WithAddonNamespace(addonNS).WithPlatformPodMutator(func(pod *corev1.PodSpec) {
		for _, c := range pod.Containers {
			if c.Name == "fluent-bit" && c.Image == fluentBitImage {
				sawFluentBit = true
				sawTolerations = len(pod.Tolerations)
				for _, v := range pod.Volumes {
					if v.ConfigMap != nil {
						sawConfigVolume = true
					}
				}
			}
		}
	})

	spec := controlplane.AddonSpec{Type: controlplane.AddonLogs, Backend: "victorialogs", Image: "victoria-logs:test", Port: 9428}
	if _, err := a.DeployAddon(ctx, spec, controlplane.DefaultEnvironment, nil); err != nil {
		t.Fatalf("DeployAddon: %v", err)
	}
	if !sawFluentBit {
		t.Fatal("mutator never saw the collector's fluent-bit container — it ran before the pod was built")
	}
	if sawTolerations != 1 {
		t.Errorf("mutator saw %d tolerations on the collector pod, want 1 (the blanket one, already set)", sawTolerations)
	}
	if !sawConfigVolume {
		t.Error("mutator did not see the collector's config volume")
	}
}

// TestNoPlatformPodMutatorLeavesAddonDeploymentUnchanged is ADR-0073 §4's obligation on the add-on
// instance path: with nothing wired, the Deployment is byte-for-byte what it was before the hook
// existed. The cache add-on is the plain case — no volume, no probe, no sidecar — so the whole
// expected object fits here and any accidental change to the add-on pod's shape fails this test.
func TestNoPlatformPodMutatorLeavesAddonDeploymentUnchanged(t *testing.T) {
	ctx := context.Background()
	client := fake.NewSimpleClientset()
	a := New(client, "apps").WithAddonNamespace(addonNS)

	spec := controlplane.AddonSpec{Type: controlplane.AddonCache, Backend: "valkey", Image: "valkey:test", Port: 6379}
	if _, err := a.DeployAddon(ctx, spec, controlplane.DefaultEnvironment, nil); err != nil {
		t.Fatalf("DeployAddon: %v", err)
	}
	got, err := client.AppsV1().Deployments(addonNS).Get(ctx, "burrow-cache", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	// The fake clientset stamps its own resourceVersion on create; that is the tracker's, not the
	// adapter's, so it is cleared before comparing.
	got.ResourceVersion = ""

	labels := map[string]string{
		nameLabel:      "burrow-cache",
		managedByLabel: managedByValue,
		addonLabel:     string(controlplane.AddonCache),
		addonEnvLabel:  controlplane.DefaultEnvironment,
	}
	want := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "burrow-cache", Namespace: addonNS, Labels: labels},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptrTo(int32(1)),
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{nameLabel: "burrow-cache"}},
			Strategy: appsv1.DeploymentStrategy{Type: appsv1.RecreateDeploymentStrategyType},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name:  string(controlplane.AddonCache),
						Image: "valkey:test",
						Ports: []corev1.ContainerPort{{ContainerPort: 6379}},
					}},
				},
			},
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("add-on Deployment built with no platform mutator differs from the pre-hook output (ADR-0073 §4)\n got: %#v\nwant: %#v", got, want)
	}
}

// TestNoPlatformPodMutatorLeavesLogsCollectorUnchanged is ADR-0073 §4's obligation on the DaemonSet
// path, and pins §3 with it: the blanket toleration is present with nothing wired, because it is the
// engine's own decision rather than something the hook supplies.
func TestNoPlatformPodMutatorLeavesLogsCollectorUnchanged(t *testing.T) {
	ctx := context.Background()
	client := fake.NewSimpleClientset()
	a := New(client, "apps").WithAddonNamespace(addonNS)

	spec := controlplane.AddonSpec{Type: controlplane.AddonLogs, Backend: "victorialogs", Image: "victoria-logs:test", Port: 9428}
	if _, err := a.DeployAddon(ctx, spec, controlplane.DefaultEnvironment, nil); err != nil {
		t.Fatalf("DeployAddon: %v", err)
	}
	got, err := client.AppsV1().DaemonSets(addonNS).Get(ctx, "burrow-logs-collector", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	got.ResourceVersion = ""

	labels := map[string]string{
		nameLabel:      "burrow-logs-collector",
		managedByLabel: managedByValue,
		addonLabel:     string(controlplane.AddonLogs),
	}
	hostPathDir := corev1.HostPathDirectory
	want := &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{Name: "burrow-logs-collector", Namespace: addonNS, Labels: labels},
		Spec: appsv1.DaemonSetSpec{
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{nameLabel: "burrow-logs-collector"}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					Tolerations: []corev1.Toleration{{Operator: corev1.TolerationOpExists}},
					Containers: []corev1.Container{{
						Name:  "fluent-bit",
						Image: fluentBitImage,
						VolumeMounts: []corev1.VolumeMount{
							{Name: "varlog", MountPath: "/var/log", ReadOnly: true},
							{Name: "config", MountPath: "/fluent-bit/etc/fluent-bit.conf", SubPath: "fluent-bit.conf"},
						},
					}},
					Volumes: []corev1.Volume{
						{Name: "varlog", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/var/log", Type: &hostPathDir}}},
						{Name: "config", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{LocalObjectReference: corev1.LocalObjectReference{Name: "burrow-logs-collector"}}}},
					},
				},
			},
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("collector DaemonSet built with no platform mutator differs from the pre-hook output (ADR-0073 §4)\n got: %#v\nwant: %#v", got, want)
	}
}

// TestNoPlatformPodMutatorLeavesMetricsCollectorUnchanged is ADR-0073 §4's obligation on the vmagent
// path: with nothing wired, the collector Deployment is byte-for-byte what it was.
func TestNoPlatformPodMutatorLeavesMetricsCollectorUnchanged(t *testing.T) {
	ctx := context.Background()
	client := fake.NewSimpleClientset(&corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{Name: vmagentServiceAccount, Namespace: addonNS},
	})
	a := New(client, "apps").WithAddonNamespace(addonNS)

	spec := controlplane.AddonSpec{Type: controlplane.AddonMetrics, Backend: "victoriametrics", Image: "victoria-metrics:test", Port: 8428}
	if _, err := a.DeployAddon(ctx, spec, controlplane.DefaultEnvironment, nil); err != nil {
		t.Fatalf("DeployAddon: %v", err)
	}
	got, err := client.AppsV1().Deployments(addonNS).Get(ctx, "burrow-metrics-collector", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	got.ResourceVersion = ""

	labels := map[string]string{
		nameLabel:      "burrow-metrics-collector",
		managedByLabel: managedByValue,
		addonLabel:     string(controlplane.AddonMetrics),
	}
	want := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "burrow-metrics-collector", Namespace: addonNS, Labels: labels},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptrTo(int32(1)),
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{nameLabel: "burrow-metrics-collector"}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					ServiceAccountName: vmagentServiceAccount,
					Containers: []corev1.Container{{
						Name:  "vmagent",
						Image: vmagentImage,
						Args: []string{
							"-promscrape.config=/config/scrape.yml",
							"-remoteWrite.url=http://burrow-metrics." + addonNS + ".svc:8428/api/v1/write",
						},
						Ports:        []corev1.ContainerPort{{ContainerPort: vmagentPort}},
						VolumeMounts: []corev1.VolumeMount{{Name: "config", MountPath: "/config"}},
					}},
					Volumes: []corev1.Volume{
						{Name: "config", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{LocalObjectReference: corev1.LocalObjectReference{Name: "burrow-metrics-collector"}}}},
					},
				},
			},
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("collector Deployment built with no platform mutator differs from the pre-hook output (ADR-0073 §4)\n got: %#v\nwant: %#v", got, want)
	}
}

// TestAppPodMutatorDoesNotReachPlatformPods holds the classification the split rests on (ADR-0073
// §2): a pod running Burrow's own image takes the platform hook and NOT the app one. An operator who
// sandboxed the tenant's image on tenant-only nodes did not ask for their logs store and collector
// there, and one hook covering both is exactly the policy they did not intend.
func TestAppPodMutatorDoesNotReachPlatformPods(t *testing.T) {
	ctx := context.Background()
	client := fake.NewSimpleClientset()
	a := New(client, "apps").WithAddonNamespace(addonNS).WithPodMutator(platformPlacement())

	spec := controlplane.AddonSpec{Type: controlplane.AddonLogs, Backend: "victorialogs", Image: "victoria-logs:test", Port: 9428}
	if _, err := a.DeployAddon(ctx, spec, controlplane.DefaultEnvironment, nil); err != nil {
		t.Fatalf("DeployAddon: %v", err)
	}
	store, err := client.AppsV1().Deployments(addonNS).Get(ctx, "burrow-logs", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Get store: %v", err)
	}
	assertNoPlacement(t, "logs store", store.Spec.Template.Spec)
	ds, err := client.AppsV1().DaemonSets(addonNS).Get(ctx, "burrow-logs-collector", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Get collector: %v", err)
	}
	assertNoPlacement(t, "logs collector", ds.Spec.Template.Spec)
}

// TestPlatformPodMutatorDoesNotReachAppPods is the other direction of the same classification: the
// app's own Deployment takes the app hook, so a platform policy meant for Burrow's Postgres and
// collectors never lands on the tenant's workload.
func TestPlatformPodMutatorDoesNotReachAppPods(t *testing.T) {
	ctx := context.Background()
	client := fake.NewSimpleClientset()
	a := New(client, "apps").WithAddonNamespace(addonNS).WithPlatformPodMutator(platformPlacement())

	if err := a.ApplyWorkload(ctx, controlplane.WorkloadSpec{App: "shop", Image: "shop:v1", Replicas: 1}); err != nil {
		t.Fatalf("ApplyWorkload: %v", err)
	}
	dep, err := client.AppsV1().Deployments("apps").Get(ctx, "shop", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	assertNoPlacement(t, "app deployment", dep.Spec.Template.Spec)
}

// TestWithNamespacePropagatesPlatformPodMutator is the case that would otherwise work in every
// single-namespace test and fail in a multi-environment install. WithNamespace returns a shallow
// copy so a hook wired once at construction reaches every per-tenant view of the adapter; a platform
// hook that survived only on the receiver would silently stop applying the moment an operation was
// routed to a named environment's namespace, and the symptom would be an add-on or a backup that
// will not schedule in one environment and schedules fine in another.
func TestWithNamespacePropagatesPlatformPodMutator(t *testing.T) {
	ctx := context.Background()
	client := fake.NewSimpleClientset()
	var jobs []*batchv1.Job
	succeedJobs(client, &jobs)
	a := New(client, "apps").WithAddonNamespace(addonNS).WithPlatformPodMutator(platformPlacement())

	// An environment-scoped view: app resources move to that environment's namespace, add-ons stay
	// in the add-on namespace.
	scoped := a.WithNamespace("apps-staging")

	spec := controlplane.AddonSpec{Type: controlplane.AddonLogs, Backend: "victorialogs", Image: "victoria-logs:test", Port: 9428}
	if _, err := scoped.DeployAddon(ctx, spec, "staging", nil); err != nil {
		t.Fatalf("DeployAddon on the scoped view: %v", err)
	}
	store, err := client.AppsV1().Deployments(addonNS).Get(ctx, "burrow-logs-staging", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Get store: %v", err)
	}
	assertPlatformPlacement(t, "scoped add-on instance", store.Spec.Template.Spec)
	ds, err := client.AppsV1().DaemonSets(addonNS).Get(ctx, "burrow-logs-staging-collector", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Get collector: %v", err)
	}
	assertPlatformPlacement(t, "scoped collector", ds.Spec.Template.Spec)

	// The backup path too: it is the one an operator discovers has lost the hook during an incident.
	if _, err := scoped.RunBackupJob(ctx, "shop", "staging", "bk1", nil); err != nil {
		t.Fatalf("RunBackupJob on the scoped view: %v", err)
	}
	if len(jobs) != 1 {
		t.Fatalf("created %d jobs, want 1", len(jobs))
	}
	assertPlatformPlacement(t, "scoped backup Job", jobs[0].Spec.Template.Spec)
}

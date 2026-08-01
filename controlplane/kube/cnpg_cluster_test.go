// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package kube_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/burrow-cloud/burrow/controlplane"
	"github.com/burrow-cloud/burrow/controlplane/kube"
)

// cnpgClusterGVR mirrors the resource the adapter writes. It is spelled again here rather than
// exported from the package: a test that names the resource independently is what notices the day
// the adapter starts writing a different one.
var cnpgClusterGVR = schema.GroupVersionResource{Group: kube.CNPGAPIGroup, Version: "v1", Resource: "clusters"}

// cnpgTestNamespace is the add-on namespace these tests act in — the adapter's default.
const cnpgTestNamespace = "burrow-addons"

// cnpgReadyCluster stands up a fake cluster with CloudNativePG installed AND a controller running,
// which is what deployPostgresCluster requires before it writes anything.
func cnpgReadyCluster(objects ...runtime.Object) (*fake.Clientset, dynamic.Interface) {
	client := fake.NewSimpleClientset(append(objects, cnpgOperatorDeployment("cnpg-system", "ghcr.io/cloudnative-pg/cloudnative-pg:"+kube.CNPGVersion, 1))...)
	client.Resources = cnpgGroupServed()
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{cnpgClusterGVR: "ClusterList"})
	return client, dyn
}

// postgresSpec is the catalog entry under test, read from the catalog rather than restated so a
// change to the storage size or the port is exercised rather than shadowed.
func postgresSpec(t *testing.T) controlplane.AddonSpec {
	t.Helper()
	spec, ok := controlplane.LookupAddon(controlplane.AddonPostgres)
	if !ok {
		t.Fatal("the catalog has no postgres add-on")
	}
	return spec
}

// getCluster reads the `Cluster` the adapter wrote.
func getCluster(t *testing.T, dyn dynamic.Interface, name string) *unstructured.Unstructured {
	t.Helper()
	u, err := dyn.Resource(cnpgClusterGVR).Namespace(cnpgTestNamespace).Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("reading the Cluster %q: %v", name, err)
	}
	return u
}

// TestDeployAddonOnCloudNativePGCreatesTheCluster is the slice's central claim: a Postgres add-on
// instance can be backed by a CloudNativePG `Cluster` (ADR-0066 §1). It asserts the fields the rest
// of Burrow depends on, not the whole object — each one below is load-bearing somewhere else.
func TestDeployAddonOnCloudNativePGCreatesTheCluster(t *testing.T) {
	ctx := context.Background()
	client, dyn := cnpgReadyCluster()
	a := kube.New(client, "burrow").WithDynamicClient(dyn)
	spec := postgresSpec(t)

	info, err := a.DeployAddon(ctx, spec, controlplane.DefaultEnvironment, nil)
	if err != nil {
		t.Fatalf("DeployAddon on CloudNativePG: %v", err)
	}

	instance, err := controlplane.AddonInstanceName(controlplane.AddonPostgres, controlplane.DefaultEnvironment)
	if err != nil {
		t.Fatal(err)
	}
	// The endpoint is the compatibility contract: the provisioner, the backup Jobs and every app's
	// DATABASE_URL resolve this name, and none of them knows which mechanism is behind it.
	if want := instance + "." + cnpgTestNamespace + ".svc:5432"; info.Endpoint != want {
		t.Errorf("endpoint = %q, want %q — a CloudNativePG instance must be reachable where a Deployment-backed one is", info.Endpoint, want)
	}
	// The Backend is what `addon list` shows an operator who asks what is running their database, and
	// it is the catalog's — the add-on has one implementation, so the registry row carries it rather
	// than a per-install choice.
	if info.Backend != "cloudnative-pg" {
		t.Errorf("backend = %q, want cloudnative-pg", info.Backend)
	}
	if info.Image != controlplane.CNPGPostgresImage {
		t.Errorf("image = %q, want the pinned operand image %q", info.Image, controlplane.CNPGPostgresImage)
	}

	u := getCluster(t, dyn, instance)
	if got := nestedString(t, u, "metadata", "labels", "app.kubernetes.io/managed-by"); got != "burrow" {
		t.Errorf("the Cluster object itself is not labelled Burrow's (managed-by = %q)", got)
	}
	if got := nestedInt(t, u, "spec", "instances"); got != 1 {
		t.Errorf("instances = %d, want the single-replica default ADR-0066 §1 keeps for the small self-hoster", got)
	}
	if got := nestedString(t, u, "spec", "imageName"); got != controlplane.CNPGPostgresImage {
		t.Errorf("spec.imageName = %q, want %q", got, controlplane.CNPGPostgresImage)
	}
	if got := nestedString(t, u, "spec", "storage", "size"); got != "10Gi" {
		t.Errorf("spec.storage.size = %q, want the catalog's 10Gi", got)
	}

	// The superuser role is DECLARED to the operator rather than created after the fact, so it
	// survives a failover and a restore, and its password comes from the Secret Burrow owns.
	roles, _, err := unstructured.NestedSlice(u.Object, "spec", "managed", "roles")
	if err != nil || len(roles) != 1 {
		t.Fatalf("spec.managed.roles = %v (err %v), want exactly one declared role", roles, err)
	}
	role, _ := roles[0].(map[string]any)
	if role["name"] != kube.PostgresSuperuser {
		t.Errorf("the declared role is %v, want %q — the role every Burrow admin connection uses", role["name"], kube.PostgresSuperuser)
	}
	if role["superuser"] != true || role["login"] != true {
		t.Errorf("the declared role is %v, want a superuser that can log in", role)
	}
	if secret, _ := role["passwordSecret"].(map[string]any); secret["name"] != instance {
		t.Errorf("the role's passwordSecret is %v, want the per-instance Secret %q", secret, instance)
	}

	// The additional managed service is what carries the instance's name onto the primary. Without
	// it the endpoint above resolves to nothing, because CNPG's own services are <cluster>-rw/-ro/-r.
	services, _, err := unstructured.NestedSlice(u.Object, "spec", "managed", "services", "additional")
	if err != nil || len(services) != 1 {
		t.Fatalf("spec.managed.services.additional = %v (err %v), want exactly one", services, err)
	}
	svc, _ := services[0].(map[string]any)
	if svc["selectorType"] != "rw" {
		t.Errorf("selectorType = %v, want \"rw\": admin SQL and dumps go to the primary", svc["selectorType"])
	}
	tmpl, _ := svc["serviceTemplate"].(map[string]any)
	meta, _ := tmpl["metadata"].(map[string]any)
	if meta["name"] != instance {
		t.Errorf("the managed service is named %v, want the instance name %q that every consumer resolves", meta["name"], instance)
	}
}

// TestCloudNativePGClusterDoesNotInheritTheManagedByLabel guards a specific false statement the
// volume listing would otherwise make. AddonVolumes selects claims by `managed-by=burrow` and then
// attributes an unroled one as a DATA claim a reinstall adopts (ADR-0064 §1). CNPG's claims are the
// operator's and a fresh `Cluster` does not adopt them, so inheriting that label would put rows in
// `addon list` promising a reinstall that brings the data back — which would not.
func TestCloudNativePGClusterDoesNotInheritTheManagedByLabel(t *testing.T) {
	ctx := context.Background()
	client, dyn := cnpgReadyCluster()
	a := kube.New(client, "burrow").WithDynamicClient(dyn)

	instance, err := controlplane.AddonInstanceName(controlplane.AddonPostgres, "staging")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.DeployAddon(ctx, postgresSpec(t), "staging", nil); err != nil {
		t.Fatalf("DeployAddon: %v", err)
	}

	u := getCluster(t, dyn, instance)
	inherited, _, err := unstructured.NestedStringMap(u.Object, "spec", "inheritedMetadata", "labels")
	if err != nil {
		t.Fatalf("reading spec.inheritedMetadata.labels: %v", err)
	}
	if _, ok := inherited["app.kubernetes.io/managed-by"]; ok {
		t.Errorf("inheritedMetadata.labels carries managed-by (%v); CNPG's own PersistentVolumeClaims would then "+
			"be listed as add-on data claims a reinstall adopts, which is false for this mechanism", inherited)
	}
	if inherited["burrow.cloud/environment"] != "staging" {
		t.Errorf("inheritedMetadata.labels = %v, want the descriptive environment label so an operator "+
			"reading the namespace can tell two environments' instances apart", inherited)
	}
	annotations, _, err := unstructured.NestedStringMap(u.Object, "spec", "inheritedMetadata", "annotations")
	if err != nil {
		t.Fatalf("reading spec.inheritedMetadata.annotations: %v", err)
	}
	if annotations["prometheus.io/scrape"] != "true" || annotations["prometheus.io/port"] != "9187" {
		t.Errorf("inheritedMetadata.annotations = %v, want the scrape annotations the metrics add-on "+
			"discovers an instance by (ADR-0051)", annotations)
	}
}

// TestCloudNativePGSuperuserSecretIsBasicAuthAndNeverInTheCluster covers the two halves of the
// credential: the shape CNPG's `passwordSecret` reference requires, and the rule that the value
// itself never leaves the Secret. The generated password is not in the custom resource, which is
// the object anyone with read access to the namespace can print.
func TestCloudNativePGSuperuserSecretIsBasicAuthAndNeverInTheCluster(t *testing.T) {
	ctx := context.Background()
	client, dyn := cnpgReadyCluster()
	a := kube.New(client, "burrow").WithDynamicClient(dyn)

	instance, err := controlplane.AddonInstanceName(controlplane.AddonPostgres, controlplane.DefaultEnvironment)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.DeployAddon(ctx, postgresSpec(t), controlplane.DefaultEnvironment, nil); err != nil {
		t.Fatalf("DeployAddon: %v", err)
	}

	sec, err := client.CoreV1().Secrets(cnpgTestNamespace).Get(ctx, instance, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("reading the superuser secret: %v", err)
	}
	if sec.Type != corev1.SecretTypeBasicAuth {
		t.Errorf("secret type = %q, want %q: CNPG's role reference requires it", sec.Type, corev1.SecretTypeBasicAuth)
	}
	if string(sec.Data["username"]) != kube.PostgresSuperuser {
		t.Errorf("username = %q, want %q", sec.Data["username"], kube.PostgresSuperuser)
	}
	// The password lives under the key ADR-0031 specified, which is why the provisioner and the
	// backup Jobs reach this instance with no special case at all.
	password := string(sec.Data[kube.PostgresPasswordKey])
	if password == "" {
		t.Fatalf("the secret has no %q key, so nothing can open this database", kube.PostgresPasswordKey)
	}

	encoded, err := json.Marshal(getCluster(t, dyn, instance).Object)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), password) {
		t.Error("the generated superuser password appears in the Cluster object; it must exist only in the Secret")
	}
}

// TestDeployAddonOnCloudNativePGRefusesWithoutTheOperator asserts an install does not write a
// `Cluster` onto a cluster that has no CloudNativePG, and that the refusal says how to fix it.
func TestDeployAddonOnCloudNativePGRefusesWithoutTheOperator(t *testing.T) {
	ctx := context.Background()
	client := fake.NewSimpleClientset()
	client.Resources = []*metav1.APIResourceList{{GroupVersion: "apps/v1"}}
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{cnpgClusterGVR: "ClusterList"})
	a := kube.New(client, "burrow").WithDynamicClient(dyn)

	_, err := a.DeployAddon(ctx, postgresSpec(t), controlplane.DefaultEnvironment, nil)
	if !errors.Is(err, controlplane.ErrInvalid) {
		t.Fatalf("DeployAddon error = %v, want ErrInvalid", err)
	}
	if !strings.Contains(err.Error(), "burrow cluster postgres install") {
		t.Errorf("the refusal does not name the command that fixes it: %v", err)
	}
	// It has to say WHY that command is not something the agent can run, or the reader's next move
	// is to hand the agent a wider credential (ADR-0066 Consequences).
	if !strings.Contains(err.Error(), "cluster-admin") {
		t.Errorf("the refusal does not say the fix needs cluster-admin: %v", err)
	}
	// And it must not surface as a Kubernetes error about an unknown resource type: a dynamic client
	// does no discovery, so a Create against an absent CRD returns a 404 indistinguishable from a
	// missing object, and "clusters.postgresql.cnpg.io not found" tells the reader nothing to act on.
	if strings.Contains(err.Error(), "clusters.postgresql.cnpg.io") {
		t.Errorf("the refusal reads as a Kubernetes not-found rather than a missing prerequisite: %v", err)
	}
	if _, err := dyn.Resource(cnpgClusterGVR).Namespace(cnpgTestNamespace).Get(ctx, "burrow-postgres", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Error("a Cluster was written onto a cluster with no CloudNativePG at all")
	}
}

// TestDeployAddonOnCloudNativePGRefusesOrphanedCRDs is the Present/Ready split doing its job on the
// path that matters. CRDs outlive the operator that installed them, so a cluster whose cnpg-system
// namespace was deleted ACCEPTS this write and then reconciles it with nothing: an add-on that
// installs cleanly and has no database under it.
func TestDeployAddonOnCloudNativePGRefusesOrphanedCRDs(t *testing.T) {
	ctx := context.Background()
	client := fake.NewSimpleClientset()
	client.Resources = cnpgGroupServed()
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{cnpgClusterGVR: "ClusterList"})
	a := kube.New(client, "burrow").WithDynamicClient(dyn)

	_, err := a.DeployAddon(ctx, postgresSpec(t), controlplane.DefaultEnvironment, nil)
	if !errors.Is(err, controlplane.ErrInvalid) {
		t.Fatalf("DeployAddon error = %v, want ErrInvalid on a cluster with CRDs and no controller", err)
	}
	if _, err := dyn.Resource(cnpgClusterGVR).Namespace(cnpgTestNamespace).Get(ctx, "burrow-postgres", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Error("a Cluster was written despite the refusal; nothing may be created before the controller is confirmed running")
	}
}

// TestDeployAddonOnCloudNativePGRefusesADeploymentEraSuperuserSecret covers the one artefact a
// Burrow release that ran the add-on as its own Deployment can leave behind in this instance's way:
// an OPAQUE superuser Secret, which a data-keeping removal deliberately kept beside the volume it
// opens (ADR-0064 §1). A Secret's type is immutable, so it cannot be converted; a `Cluster` written
// beside it would be rejected by CNPG's role management on one hand and would come up EMPTY beside
// data that is still on the old claim on the other. The refusal is what keeps that from reading as
// a successful install.
func TestDeployAddonOnCloudNativePGRefusesADeploymentEraSuperuserSecret(t *testing.T) {
	ctx := context.Background()
	instance, err := controlplane.AddonInstanceName(controlplane.AddonPostgres, controlplane.DefaultEnvironment)
	if err != nil {
		t.Fatal(err)
	}
	client, dyn := cnpgReadyCluster(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: instance, Namespace: cnpgTestNamespace},
		Type:       corev1.SecretTypeOpaque,
		Data:       map[string][]byte{kube.PostgresPasswordKey: []byte("a-password-from-the-deployment-era")},
	})
	a := kube.New(client, "burrow").WithDynamicClient(dyn)

	_, err = a.DeployAddon(ctx, postgresSpec(t), controlplane.DefaultEnvironment, nil)
	if !errors.Is(err, controlplane.ErrInvalid) {
		t.Fatalf("DeployAddon error = %v, want ErrInvalid", err)
	}
	if _, err := dyn.Resource(cnpgClusterGVR).Namespace(cnpgTestNamespace).Get(ctx, instance, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Error("a Cluster was created beside a superuser Secret that opens a volume it does not adopt")
	}
}

// TestDeployAddonOnCloudNativePGIsIdempotent asserts a re-install lands on the instance that is
// already there rather than failing or rotating the credential of a running database — the same
// property the Deployment path has.
func TestDeployAddonOnCloudNativePGIsIdempotent(t *testing.T) {
	ctx := context.Background()
	client, dyn := cnpgReadyCluster()
	a := kube.New(client, "burrow").WithDynamicClient(dyn)
	spec := postgresSpec(t)

	if _, err := a.DeployAddon(ctx, spec, controlplane.DefaultEnvironment, nil); err != nil {
		t.Fatalf("first install: %v", err)
	}
	instance, err := controlplane.AddonInstanceName(controlplane.AddonPostgres, controlplane.DefaultEnvironment)
	if err != nil {
		t.Fatal(err)
	}
	first, err := client.CoreV1().Secrets(cnpgTestNamespace).Get(ctx, instance, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := a.DeployAddon(ctx, spec, controlplane.DefaultEnvironment, nil); err != nil {
		t.Fatalf("re-install: %v", err)
	}
	again, err := client.CoreV1().Secrets(cnpgTestNamespace).Get(ctx, instance, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if string(again.Data[kube.PostgresPasswordKey]) != string(first.Data[kube.PostgresPasswordKey]) {
		t.Error("the re-install rotated the superuser password, which would lock burrowd out of a running database")
	}
	list, err := dyn.Resource(cnpgClusterGVR).Namespace(cnpgTestNamespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Items) != 1 {
		t.Errorf("%d Clusters after two installs, want 1", len(list.Items))
	}
}

// TestAddonReadyReadsTheClusterWhenThereIsNoDeployment is the guard against a CloudNativePG-backed
// instance reading as an ADR-0074 §6 discrepancy.
//
// §6's diagnosis is an ABSENCE: the registry says this add-on exists and the cluster does not have
// it. The failure observer makes that call from this seam's answer, so a probe that only ever looked
// for a Deployment would report a serving database as not running — once a minute, forever — and the
// ledger would fill with AddonNotRunning rows indistinguishable from real ones. What makes the
// difference is that the seam resolves WHICH object the add-on is, rather than assuming.
func TestAddonReadyReadsTheClusterWhenThereIsNoDeployment(t *testing.T) {
	instance := "burrow-postgres"
	for _, tc := range []struct {
		name  string
		ready int64
		exist bool
		want  bool
	}{
		{name: "a serving Cluster is ready", ready: 1, exist: true, want: true},
		{name: "a Cluster with no ready instance is not", ready: 0, exist: true, want: false},
		{name: "no Cluster and no Deployment is not, and is not an error", exist: false, want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			client, dyn := cnpgReadyCluster()
			if tc.exist {
				obj := &unstructured.Unstructured{Object: map[string]any{
					"apiVersion": "postgresql.cnpg.io/v1",
					"kind":       "Cluster",
					"metadata":   map[string]any{"name": instance, "namespace": cnpgTestNamespace},
					"status":     map[string]any{"readyInstances": tc.ready, "instances": int64(1)},
				}}
				if _, err := dyn.Resource(cnpgClusterGVR).Namespace(cnpgTestNamespace).Create(ctx, obj, metav1.CreateOptions{}); err != nil {
					t.Fatal(err)
				}
			}
			a := kube.New(client, "burrow").WithDynamicClient(dyn)
			got, err := a.AddonReady(ctx, instance)
			if err != nil {
				t.Fatalf("AddonReady: %v", err)
			}
			if got != tc.want {
				t.Errorf("AddonReady = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestAddonReadyWithoutADynamicClientIsUnchanged asserts the readiness probe on an adapter that
// cannot address custom resources behaves exactly as it did before this existed: a missing
// Deployment is "not ready", not an error. That is what keeps every existing embedder, and every
// test that builds an adapter with New, on the path they were on.
func TestAddonReadyWithoutADynamicClientIsUnchanged(t *testing.T) {
	a := kube.New(fake.NewSimpleClientset(), "burrow")
	ready, err := a.AddonReady(context.Background(), "burrow-postgres")
	if err != nil {
		t.Fatalf("AddonReady: %v", err)
	}
	if ready {
		t.Error("AddonReady = true with nothing in the cluster")
	}
}

// TestControllerPlacementReachesTheCluster is ADR-0077 §2's seam arriving at the pod it was built
// for. Until this slice the translation had no caller: the policy was validated at wiring time and
// then written into nothing. The database is the pod ADR-0073 calls the platform hook's "most
// dangerous reach", and on a cluster whose only capacity is tainted this toleration is the
// difference between an instance that schedules and one that sits Pending reporting a slow start.
func TestControllerPlacementReachesTheCluster(t *testing.T) {
	ctx := context.Background()
	client, dyn := cnpgReadyCluster()
	a, err := kube.New(client, "burrow").WithDynamicClient(dyn).WithControllerPodPlacement(kube.PodPlacement{
		Tolerations: []corev1.Toleration{{Key: "node-role.kubernetes.io/control-plane", Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoSchedule}},
	})
	if err != nil {
		t.Fatalf("WithControllerPodPlacement: %v", err)
	}

	if _, err := a.DeployAddon(ctx, postgresSpec(t), controlplane.DefaultEnvironment, nil); err != nil {
		t.Fatalf("DeployAddon: %v", err)
	}
	u := getCluster(t, dyn, "burrow-postgres")
	tolerations, found, err := unstructured.NestedSlice(u.Object, "spec", "affinity", "tolerations")
	if err != nil || !found || len(tolerations) != 1 {
		t.Fatalf("spec.affinity.tolerations = %v (found %v, err %v), want the wired policy", tolerations, found, err)
	}
	tol, _ := tolerations[0].(map[string]any)
	if tol["key"] != "node-role.kubernetes.io/control-plane" {
		t.Errorf("the carried toleration is %v, want the one that was wired", tol)
	}
	// "Tolerate this taint, touch nothing else" (ADR-0077 §5) must stay exactly that: an explicit
	// empty nodeSelector on a database whose local-path volume is bound to one node is policy
	// nobody asked for.
	if _, found, _ := unstructured.NestedFieldNoCopy(u.Object, "spec", "affinity", "nodeSelector"); found {
		t.Error("spec.affinity.nodeSelector was written by a policy that set no node selector")
	}
}

// nestedString reads a string at path, failing the test if it is absent.
func nestedString(t *testing.T, u *unstructured.Unstructured, path ...string) string {
	t.Helper()
	s, found, err := unstructured.NestedString(u.Object, path...)
	if err != nil || !found {
		t.Fatalf("%s: found %v, err %v", strings.Join(path, "."), found, err)
	}
	return s
}

// nestedInt reads an integer at path, failing the test if it is absent.
func nestedInt(t *testing.T, u *unstructured.Unstructured, path ...string) int64 {
	t.Helper()
	n, found, err := unstructured.NestedInt64(u.Object, path...)
	if err != nil || !found {
		t.Fatalf("%s: found %v, err %v", strings.Join(path, "."), found, err)
	}
	return n
}

// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/clientcmd/api"

	"github.com/burrow-cloud/burrow/connect"
	"github.com/burrow-cloud/burrow/localconfig"
)

// existingInstall builds a fake cluster that looks like a completed `burrow install` in
// namespace ns, deploying apps into appNS. Extra container env vars (e.g. the add-on
// namespace) can be appended to model installs from different eras.
func existingInstall(ns, appNS string, extraEnv ...corev1.EnvVar) *fake.Clientset {
	env := append([]corev1.EnvVar{{Name: "BURROW_NAMESPACE", Value: appNS}}, extraEnv...)
	return fake.NewSimpleClientset(
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "burrowd-api-token", Namespace: ns},
			Data:       map[string][]byte{"token": []byte("existing-token")},
		},
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "burrowd-db", Namespace: ns},
			Data:       map[string][]byte{"password": []byte("existing-pw")},
		},
		&appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: "burrowd", Namespace: ns},
			Spec: appsv1.DeploymentSpec{
				Template: corev1.PodTemplateSpec{
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{{
							Name: "burrowd",
							Env:  env,
						}},
					},
				},
			},
		},
	)
}

func TestUpgradeOptionsPreservesState(t *testing.T) {
	cs := existingInstall("burrow", "apps", corev1.EnvVar{Name: "BURROW_ADDON_NAMESPACE", Value: "addons"})
	opts, err := upgradeOptions(context.Background(), cs, "burrow", "registry.example.com/burrowd:v0.1.2")
	if err != nil {
		t.Fatalf("upgradeOptions: %v", err)
	}
	if opts.Token != "existing-token" {
		t.Errorf("token not preserved: got %q", opts.Token)
	}
	if opts.DBPassword != "existing-pw" {
		t.Errorf("db password not preserved: got %q", opts.DBPassword)
	}
	if opts.AppNamespace != "apps" {
		t.Errorf("app namespace not preserved: got %q", opts.AppNamespace)
	}
	if opts.AddonNamespace != "addons" {
		t.Errorf("add-on namespace not preserved: got %q", opts.AddonNamespace)
	}
	if opts.Image != "registry.example.com/burrowd:v0.1.2" {
		t.Errorf("image not set to the upgrade target: got %q", opts.Image)
	}
}

// TestUpgradeOptionsDefaultsAddonNamespace covers an install that predates add-ons: the
// running Deployment carries no BURROW_ADDON_NAMESPACE env, so the upgrade falls back to the
// default add-on namespace rather than re-rendering an empty one.
func TestUpgradeOptionsDefaultsAddonNamespace(t *testing.T) {
	cs := existingInstall("burrow", "apps")
	opts, err := upgradeOptions(context.Background(), cs, "burrow", "img:2")
	if err != nil {
		t.Fatalf("upgradeOptions: %v", err)
	}
	if opts.AddonNamespace != connect.DefaultAddonNamespace {
		t.Errorf("add-on namespace not defaulted: got %q, want %q", opts.AddonNamespace, connect.DefaultAddonNamespace)
	}
}

// emptyMetaField matches a rendered name/namespace field with no value (a trailing space may
// remain after the colon, so \s* before the line end matters).
var emptyMetaField = regexp.MustCompile(`(?m)^\s*(name|namespace):\s*$`)

// TestUpgradeOptionsRendersNoEmptyFields guards every installOptions field an upgrade must
// carry forward: rendering the manifests from upgradeOptions must never leave a name or
// namespace field blank, whichever era the install came from. A blank field is what made
// server-side apply reject the upgrade ("applying namespace/: name is required").
func TestUpgradeOptionsRendersNoEmptyFields(t *testing.T) {
	fixtures := map[string]*fake.Clientset{
		"with add-on env":   existingInstall("burrow", "apps", corev1.EnvVar{Name: "BURROW_ADDON_NAMESPACE", Value: "addons"}),
		"predating add-ons": existingInstall("burrow", "apps"),
	}
	for name, cs := range fixtures {
		t.Run(name, func(t *testing.T) {
			opts, err := upgradeOptions(context.Background(), cs, "burrow", "img:2")
			if err != nil {
				t.Fatalf("upgradeOptions: %v", err)
			}
			manifests, err := renderManifests(opts)
			if err != nil {
				t.Fatalf("renderManifests: %v", err)
			}
			if m := emptyMetaField.FindString(manifests); m != "" {
				t.Errorf("rendered manifests contain an empty name/namespace field: %q", m)
			}
		})
	}
}

func TestUpgradeOptionsNotInstalled(t *testing.T) {
	cs := fake.NewSimpleClientset()
	_, err := upgradeOptions(context.Background(), cs, "burrow", "img:2")
	if err == nil {
		t.Fatal("expected an error upgrading with nothing installed")
	}
	if !strings.Contains(err.Error(), "not installed") {
		t.Errorf("error should explain Burrow is not installed, got: %v", err)
	}
}

// TestUpgradeBackfillsAgentCredential asserts the upgrade's local-side backfill provisions the
// scoped agent kubeconfig onto the operator's existing handle for the upgraded cluster (ADR-0038 §4),
// so a control plane installed before the scoped credential existed gains the local kubeconfig.
func TestUpgradeBackfillsAgentCredential(t *testing.T) {
	tempConfig(t)
	kc := kubeconfigWithCurrent(t, "dev", "dev")
	cfg := &localconfig.Config{Environments: []localconfig.Environment{{Name: "dev", Context: "dev", ControlPlaneNamespace: "burrow"}}}
	if err := cfg.Save(); err != nil {
		t.Fatalf("save: %v", err)
	}
	calls := stubJoinAgentCredential(t, func(envName string) (string, string, error) {
		return "/tmp/agents/" + envName, agentKubeContextName, nil
	})

	var out bytes.Buffer
	backfillAgentCredential(context.Background(), kc, "", "burrow", &out)

	got, err := localconfig.Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	env, _ := got.Lookup("dev")
	if env.AgentKubeconfig != "/tmp/agents/dev" || env.AgentContext != agentKubeContextName {
		t.Errorf("upgrade did not backfill the scoped credential onto the dev handle: %+v", env)
	}
	if len(*calls) != 1 {
		t.Errorf("a pre-credential handle should trigger exactly one join, got %d", len(*calls))
	}
	if !strings.Contains(out.String(), "Backfilled the scoped agent credential") {
		t.Errorf("missing the backfill confirmation:\n%s", out.String())
	}
}

// TestUpgradeBackfillSilentWhenCredentialPresent asserts the every-upgrade "Backfilled…" noise is
// gone: a handle that already carries the scoped credential (the common case, any current install)
// is a no-op — no re-join and no output — so a routine upgrade never re-prints the backfill line.
func TestUpgradeBackfillSilentWhenCredentialPresent(t *testing.T) {
	tempConfig(t)
	kc := kubeconfigWithCurrent(t, "dev", "dev")
	cfg := &localconfig.Config{Environments: []localconfig.Environment{{
		Name:                  "dev",
		Context:               "dev",
		ControlPlaneNamespace: "burrow",
		AgentKubeconfig:       "/tmp/agents/dev",
		AgentContext:          agentKubeContextName,
	}}}
	if err := cfg.Save(); err != nil {
		t.Fatalf("save: %v", err)
	}
	calls := stubJoinAgentCredential(t, func(envName string) (string, string, error) {
		return "/tmp/agents/" + envName, agentKubeContextName, nil
	})

	var out bytes.Buffer
	backfillAgentCredential(context.Background(), kc, "", "burrow", &out)

	if len(*calls) != 0 {
		t.Errorf("a handle with a credential must not re-join, got %d join call(s)", len(*calls))
	}
	if out.String() != "" {
		t.Errorf("a routine upgrade must stay silent, got output:\n%s", out.String())
	}
}

// TestUpgradeBackfillBestEffort asserts the backfill never fails the upgrade: when the join cannot
// run it warns and leaves the handle unchanged, returning normally.
func TestUpgradeBackfillBestEffort(t *testing.T) {
	tempConfig(t)
	kc := kubeconfigWithCurrent(t, "dev", "dev")
	cfg := &localconfig.Config{Environments: []localconfig.Environment{{Name: "dev", Context: "dev", ControlPlaneNamespace: "burrow"}}}
	if err := cfg.Save(); err != nil {
		t.Fatalf("save: %v", err)
	}
	stubJoinAgentCredential(t, func(string) (string, string, error) {
		return "", "", errors.New("agent token secret unreadable")
	})

	var out bytes.Buffer
	backfillAgentCredential(context.Background(), kc, "", "burrow", &out) // must not panic or fail

	got, err := localconfig.Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	env, _ := got.Lookup("dev")
	if env.AgentKubeconfig != "" {
		t.Errorf("a failed backfill must leave the handle without a cred, got %+v", env)
	}
	if !strings.Contains(out.String(), "Warning") {
		t.Errorf("a failed backfill should warn:\n%s", out.String())
	}
}

func TestAlreadyInstalled(t *testing.T) {
	t.Run("present", func(t *testing.T) {
		got, err := alreadyInstalled(context.Background(), existingInstall("burrow", "default"), "burrow")
		if err != nil {
			t.Fatalf("alreadyInstalled: %v", err)
		}
		if !got {
			t.Error("expected an existing install to be detected")
		}
	})
	t.Run("absent", func(t *testing.T) {
		got, err := alreadyInstalled(context.Background(), fake.NewSimpleClientset(), "burrow")
		if err != nil {
			t.Fatalf("alreadyInstalled: %v", err)
		}
		if got {
			t.Error("expected no install to be detected in an empty cluster")
		}
	})
}

// installConfigMap is the ConfigMap an install records its own id in (ADR-0084 §5), for fixtures
// modelling a cluster that already carries one.
func installConfigMap(ns, id string) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: connect.DefaultInstallConfigMap, Namespace: ns},
		Data:       map[string]string{connect.DefaultInstallIDKey: id},
	}
}

// TestUpgradeOptionsPreservesInstallID is the property that makes install ids safe to rely on: a
// routine upgrade must re-render the id the install already has. Minting a new one would make every
// target pointed at this install report a mismatch immediately after upgrading — breaking exactly
// the people who had it configured correctly (ADR-0084 §5).
func TestUpgradeOptionsPreservesInstallID(t *testing.T) {
	cs := existingInstall("burrow", "apps")
	if _, err := cs.CoreV1().ConfigMaps("burrow").Create(context.Background(), installConfigMap("burrow", "install-abc"), metav1.CreateOptions{}); err != nil {
		t.Fatalf("seeding the install ConfigMap: %v", err)
	}

	opts, err := upgradeOptions(context.Background(), cs, "burrow", "img:2")
	if err != nil {
		t.Fatalf("upgradeOptions: %v", err)
	}
	if opts.InstallID != "install-abc" {
		t.Errorf("install id not preserved: got %q, want install-abc", opts.InstallID)
	}
	manifests, err := renderManifests(opts)
	if err != nil {
		t.Fatalf("renderManifests: %v", err)
	}
	if !strings.Contains(manifests, `id: "install-abc"`) {
		t.Errorf("the re-rendered manifests do not carry the existing install id:\n%s", manifests)
	}
}

// TestUpgradeOptionsMintsAnInstallIDWhenAbsent covers upgrading an install that predates install ids:
// there is no ConfigMap to read, so the upgrade is where the install acquires one. Nothing can
// mismatch against an id no target has seen yet.
func TestUpgradeOptionsMintsAnInstallIDWhenAbsent(t *testing.T) {
	cs := existingInstall("burrow", "apps")

	opts, err := upgradeOptions(context.Background(), cs, "burrow", "img:2")
	if err != nil {
		t.Fatalf("upgradeOptions: %v", err)
	}
	if opts.InstallID == "" {
		t.Fatal("upgrading an install that predates install ids must mint one, got an empty id")
	}
	manifests, err := renderManifests(opts)
	if err != nil {
		t.Fatalf("renderManifests: %v", err)
	}
	if !strings.Contains(manifests, `id: "`+opts.InstallID+`"`) {
		t.Errorf("the minted install id was not rendered into the manifests:\n%s", manifests)
	}
}

// TestUpgradeOptionsMintsAnInstallIDWhenTheKeyIsEmpty covers a ConfigMap that exists but carries no
// id — a partially applied or hand-edited install. An empty id would be rendered into burrowd's
// environment and disable the check silently, so it is treated as absent and replaced.
func TestUpgradeOptionsMintsAnInstallIDWhenTheKeyIsEmpty(t *testing.T) {
	cs := existingInstall("burrow", "apps")
	if _, err := cs.CoreV1().ConfigMaps("burrow").Create(context.Background(), installConfigMap("burrow", ""), metav1.CreateOptions{}); err != nil {
		t.Fatalf("seeding the install ConfigMap: %v", err)
	}

	opts, err := upgradeOptions(context.Background(), cs, "burrow", "img:2")
	if err != nil {
		t.Fatalf("upgradeOptions: %v", err)
	}
	if opts.InstallID == "" {
		t.Error("an empty id in the ConfigMap must be replaced with a minted one")
	}
}

// stubUpgrade replaces the cluster-touching seams for a full `burrow cluster upgrade` run against a
// fake cluster, and points $BURROW_CONFIG at a temp file. cs is the fake cluster the upgrade reads
// and re-renders from. It returns a pointer to everything the upgrade applied, so the id recorded
// locally can be checked against the id the cluster was actually given. All seams are restored on
// cleanup.
func stubUpgrade(t *testing.T, cs kubernetes.Interface) *strings.Builder {
	t.Helper()
	t.Setenv("BURROW_CONFIG", filepath.Join(t.TempDir(), "config"))

	origCS := clientsetFn
	clientsetFn = func(string, string) (kubernetes.Interface, error) { return cs, nil }

	var applied strings.Builder
	origApply := applyFn
	applyFn = func(_ context.Context, _, _ string, manifests string, _ bool, _, _ io.Writer) error {
		applied.WriteString(manifests)
		return nil
	}

	// The scoped-credential backfill needs a real token Secret and REST config; it is exercised in
	// its own tests. Here it records a fixed path so the upgrade completes.
	origJoin := joinAgentCredentialFn
	joinAgentCredentialFn = func(_ context.Context, _, _, _, envName string) (string, string, error) {
		return filepath.Join(t.TempDir(), "agents", envName), agentKubeContextName, nil
	}

	t.Cleanup(func() {
		clientsetFn = origCS
		applyFn = origApply
		joinAgentCredentialFn = origJoin
	})
	return &applied
}

// upgradeKubeconfig writes a kubeconfig whose current context is named ctxName, so an upgrade — which
// acts on the ambient current context — resolves to a context the test has registered locally.
func upgradeKubeconfig(t *testing.T, ctxName string) string {
	t.Helper()
	cfg := api.NewConfig()
	cfg.Clusters["c"] = &api.Cluster{Server: "https://cluster.example:6443", InsecureSkipTLSVerify: true}
	cfg.AuthInfos["user"] = &api.AuthInfo{Token: "t"}
	cfg.Contexts[ctxName] = &api.Context{Cluster: "c", AuthInfo: "user"}
	cfg.CurrentContext = ctxName
	return writeKubeconfig(t, cfg)
}

// TestUpgradeRecordsAMintedInstallIDLocally is the end-to-end claim recordUpgradedInstallID makes:
// upgrading a control plane that predates install ids both mints one cluster-side AND records it on
// the local records pointed at that cluster. Without the local half the id would exist only on the
// cluster, and the check would protect nothing on an install that predates it (ADR-0084 §5).
func TestUpgradeRecordsAMintedInstallIDLocally(t *testing.T) {
	applied := stubUpgrade(t, existingInstall("burrow", "apps"))
	kubeconfig := upgradeKubeconfig(t, "do-nyc3-burrow")

	// A target and a handle both pointed at the upgraded cluster: the CLI resolves through one and
	// burrow-agent through the other, so both have to learn the id.
	cfg, err := localconfig.Load()
	if err != nil {
		t.Fatalf("loading config: %v", err)
	}
	if err := cfg.SetTarget(localconfig.KubernetesTarget("do-nyc3-burrow")); err != nil {
		t.Fatalf("SetTarget: %v", err)
	}
	if err := cfg.Add(localconfig.Environment{Name: "prod", Context: "do-nyc3-burrow", AppNamespace: "apps"}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := cfg.Save(); err != nil {
		t.Fatalf("saving config: %v", err)
	}

	var out, errb bytes.Buffer
	if err := run(context.Background(), []string{"cluster", "upgrade", "--kubeconfig", kubeconfig, "--burrowd-image", "img:2", "--wait=false"}, &out, &errb); err != nil {
		t.Fatalf("cluster upgrade: %v\n%s", err, errb.String())
	}

	cfg, err = localconfig.Load()
	if err != nil {
		t.Fatalf("reloading config: %v", err)
	}
	tgt, ok := cfg.LookupTarget("do-nyc3-burrow")
	if !ok {
		t.Fatalf("the target went missing: %+v", cfg.Targets)
	}
	if tgt.InstallID == "" {
		t.Fatal("upgrading an install that predates ids recorded none on the target")
	}
	env, ok := cfg.Lookup("prod")
	if !ok {
		t.Fatalf("the handle went missing: %+v", cfg.Environments)
	}
	if env.InstallID != tgt.InstallID {
		t.Errorf("handle InstallID = %q, target = %q; both point at the same cluster and must agree", env.InstallID, tgt.InstallID)
	}
	if !strings.Contains(applied.String(), `id: "`+tgt.InstallID+`"`) {
		t.Errorf("the id recorded locally (%q) is not the one applied to the cluster", tgt.InstallID)
	}
}

// TestUpgradePreservesTheRecordedInstallIDLocally covers the routine case: the cluster already has an
// id, so the upgrade re-records the same value and nothing a target was checking against changes.
func TestUpgradePreservesTheRecordedInstallIDLocally(t *testing.T) {
	cs := existingInstall("burrow", "apps")
	if _, err := cs.CoreV1().ConfigMaps("burrow").Create(context.Background(), installConfigMap("burrow", "install-abc"), metav1.CreateOptions{}); err != nil {
		t.Fatalf("seeding the install ConfigMap: %v", err)
	}
	stubUpgrade(t, cs)
	kubeconfig := upgradeKubeconfig(t, "do-nyc3-burrow")

	cfg, err := localconfig.Load()
	if err != nil {
		t.Fatalf("loading config: %v", err)
	}
	if err := cfg.SetTarget(localconfig.KubernetesTarget("do-nyc3-burrow")); err != nil {
		t.Fatalf("SetTarget: %v", err)
	}
	if err := cfg.Save(); err != nil {
		t.Fatalf("saving config: %v", err)
	}

	var out, errb bytes.Buffer
	if err := run(context.Background(), []string{"cluster", "upgrade", "--kubeconfig", kubeconfig, "--burrowd-image", "img:2", "--wait=false"}, &out, &errb); err != nil {
		t.Fatalf("cluster upgrade: %v\n%s", err, errb.String())
	}

	cfg, err = localconfig.Load()
	if err != nil {
		t.Fatalf("reloading config: %v", err)
	}
	if tgt, _ := cfg.LookupTarget("do-nyc3-burrow"); tgt.InstallID != "install-abc" {
		t.Errorf("target InstallID = %q, want the preserved install-abc", tgt.InstallID)
	}
}

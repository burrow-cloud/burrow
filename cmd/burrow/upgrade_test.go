// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package main

import (
	"bytes"
	"context"
	"errors"
	"regexp"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

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
	backfillAgentCredential(context.Background(), kc, "burrow", &out)

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
	backfillAgentCredential(context.Background(), kc, "burrow", &out)

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
	backfillAgentCredential(context.Background(), kc, "burrow", &out) // must not panic or fail

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

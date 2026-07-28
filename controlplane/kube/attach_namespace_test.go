// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package kube_test

import (
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sfake "k8s.io/client-go/kubernetes/fake"

	cp "github.com/burrow-cloud/burrow/controlplane"
	cpfake "github.com/burrow-cloud/burrow/controlplane/internal/fake"
	"github.com/burrow-cloud/burrow/controlplane/kube"
)

// TestAttachWritesTheAppSecretIntoTheEnvironmentsNamespace pins where an attach's DATABASE_URL
// lands, through the REAL Kubernetes adapter (ADR-0029/0035 phase 2b, ADR-0067 §1).
//
// An attach now writes the connection string into the namespace the ENGINE resolves for the
// operation's environment, not into whatever namespace the adapter happens to be bound to. The two
// agree in production — burrowd passes BURROW_NAMESPACE to both — so the distinction only shows up
// when they disagree, which is why it is pinned here with them deliberately different.
//
// The failure mode this guards against is quiet in the worst way: a Secret written to the wrong
// namespace does not error, and a pod that mounts a missing Secret does not fail either — it sits in
// CreateContainerConfigError and never starts, so the only symptom is a workload that waits forever.
func TestAttachWritesTheAppSecretIntoTheEnvironmentsNamespace(t *testing.T) {
	ctx := context.Background()
	client := k8sfake.NewSimpleClientset()

	// Deliberately mismatched: the adapter is bound to one namespace, the engine knows another as
	// its app namespace. The environment's namespace is the one that must win.
	adapter := kube.New(client, "adapter-ns").WithAddonNamespace("burrow-addons")
	db := cpfake.NewDatabase()
	engine, err := cp.New(cp.Deps{
		Kubernetes:          adapter,
		Database:            db,
		Clock:               cpfake.NewClock(time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)),
		IDs:                 cpfake.NewIDs(),
		Resolver:            cpfake.NewResolver(),
		Credentials:         cpfake.NewCredentials(),
		DNS:                 cpfake.NewDNSFactory(),
		DatabaseProvisioner: cpfake.NewProvisioner(),
		AppNamespace:        "engine-apps",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if _, err := engine.AttachAddon(ctx, cp.AddonPostgres, "shop", ""); err != nil {
		t.Fatalf("AttachAddon: %v", err)
	}
	secretName := cp.AppSecretName("shop")
	if _, err := client.CoreV1().Secrets("engine-apps").Get(ctx, secretName, metav1.GetOptions{}); err != nil {
		t.Errorf("the app Secret is not in the default environment's namespace engine-apps: %v", err)
	}
	for _, ns := range []string{"adapter-ns", "default"} {
		if _, err := client.CoreV1().Secrets(ns).Get(ctx, secretName, metav1.GetOptions{}); err == nil {
			t.Errorf("the app Secret was written into %s; an attach must follow the environment's namespace", ns)
		}
	}

	// A second environment routes to its own namespace, which is the other half of keeping two
	// environments' identically-named apps apart: separate database, separate credential Secret.
	if err := db.CreateEnvironment(ctx, "staging", "engine-apps-staging"); err != nil {
		t.Fatalf("CreateEnvironment: %v", err)
	}
	if _, err := engine.AttachAddon(ctx, cp.AddonPostgres, "shop", "staging"); err != nil {
		t.Fatalf("AttachAddon(staging): %v", err)
	}
	if _, err := client.CoreV1().Secrets("engine-apps-staging").Get(ctx, secretName, metav1.GetOptions{}); err != nil {
		t.Errorf("staging's app Secret is not in staging's namespace: %v", err)
	}
}

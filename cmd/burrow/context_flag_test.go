// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/clientcmd/api"
)

// writeKubeconfig writes cfg to a temp file and returns its path.
func writeKubeconfig(t *testing.T, cfg *api.Config) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "kubeconfig")
	if err := clientcmd.WriteToFile(*cfg, path); err != nil {
		t.Fatalf("write kubeconfig: %v", err)
	}
	return path
}

// twoContextConfig builds a kubeconfig with a current context "staging" and a non-current "prod".
func twoContextConfig(serverStaging, serverProd string) *api.Config {
	cfg := api.NewConfig()
	cfg.Clusters["c-staging"] = &api.Cluster{Server: serverStaging, InsecureSkipTLSVerify: true}
	cfg.Clusters["c-prod"] = &api.Cluster{Server: serverProd, InsecureSkipTLSVerify: true}
	cfg.AuthInfos["user"] = &api.AuthInfo{Token: "t"}
	cfg.Contexts["staging"] = &api.Context{Cluster: "c-staging", AuthInfo: "user"}
	cfg.Contexts["prod"] = &api.Context{Cluster: "c-prod", AuthInfo: "user"}
	cfg.CurrentContext = "staging"
	return cfg
}

// fakeBurrowdCluster is a fake API server standing in for one cluster: it serves the install token
// Secret and any proxied control-plane call (here, app status), recording that it was hit.
func fakeBurrowdCluster(hit *bool) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*hit = true
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "/secrets/") {
			_ = json.NewEncoder(w).Encode(&corev1.Secret{
				TypeMeta:   metav1.TypeMeta{Kind: "Secret", APIVersion: "v1"},
				ObjectMeta: metav1.ObjectMeta{Name: "burrowd-api-token", Namespace: "burrow"},
				Data:       map[string][]byte{"token": []byte("s3cr3t")},
			})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"app": "web", "has_release": true, "running": true,
			"release":  map[string]any{"id": "r1", "image": "img:1", "status": "deployed"},
			"workload": map[string]any{"desired_replicas": 3, "ready_replicas": 3, "available": true},
		})
	}))
}

// TestContextFlagWired confirms the global --context flag reaches connect.Options: a command run
// with --context targets that context's cluster, not the kubeconfig's current context. The flag
// survives the retirement of the `burrow context` command (ADR-0036).
func TestContextFlagWired(t *testing.T) {
	t.Setenv("BURROW_CONTROL_PLANE_URL", "")
	t.Setenv("BURROW_API_TOKEN", "")

	var stagingHit, prodHit bool
	staging := fakeBurrowdCluster(&stagingHit)
	prod := fakeBurrowdCluster(&prodHit)
	defer staging.Close()
	defer prod.Close()

	path := writeKubeconfig(t, twoContextConfig(staging.URL, prod.URL))

	var out, errb bytes.Buffer
	err := run(context.Background(), []string{"app", "status", "web", "--context", "prod", "--kubeconfig", path}, &out, &errb)
	if err != nil {
		t.Fatalf("run: %v\n%s", err, errb.String())
	}
	if !prodHit {
		t.Errorf("--context prod did not reach the prod cluster's burrowd")
	}
	if stagingHit {
		t.Errorf("the current context (staging) was contacted; --context should redirect to prod")
	}
	if !strings.Contains(out.String(), "workload: 3/3 replicas ready, available") {
		t.Errorf("status output = %q", out.String())
	}
}

// TestContextCommandRetired confirms the `burrow context` command is gone (ADR-0036): only the
// low-level --context flag survives, so invoking `context` as a subcommand is an unknown command.
func TestContextCommandRetired(t *testing.T) {
	for _, c := range newRootCmd().Commands() {
		if c.Name() == "context" {
			t.Fatalf("the `burrow context` command should be retired, but it is still registered")
		}
	}

	var out, errb bytes.Buffer
	if err := run(context.Background(), []string{"context", "list"}, &out, &errb); err == nil {
		t.Errorf("`burrow context list` should error now that the command is retired; got nil")
	}
}

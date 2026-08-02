// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package main

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/burrow-cloud/burrow/internal/cloudcred"
	"github.com/burrow-cloud/burrow/localconfig"
)

// The credentials these tests store. Distinct, and distinctive, so a test can assert the right one
// was spent and that neither ever reached an output stream.
const (
	cloudCLIToken   = "tok_cli_credential_for_the_person"
	cloudAgentToken = "tok_agent_credential_for_burrow_agent"
)

// signedInToCloud puts a machine in the state `burrow auth login` leaves it in: the managed product
// selected as the active target, and both credentials on disk. It returns nothing because everything
// a command needs is read from that state, which is the point.
func signedInToCloud(t *testing.T) {
	t.Helper()
	tempConfig(t)
	t.Setenv("BURROW_CONTROL_PLANE_URL", "")
	t.Setenv("BURROW_API_TOKEN", "")

	cfg, err := localconfig.Load()
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if err := cfg.SetTarget(localconfig.CloudTarget()); err != nil {
		t.Fatalf("SetTarget: %v", err)
	}
	if err := cfg.Save(); err != nil {
		t.Fatalf("save config: %v", err)
	}
	if _, err := cloudcred.Store(cloudcred.KindCLI, cloudcred.Credential{
		Endpoint: localconfig.CloudEndpoint, Kind: cloudcred.KindCLI,
		TenantID: "t_1234", CredentialID: "cred_cli_1", Token: cloudCLIToken,
	}); err != nil {
		t.Fatalf("store the person's credential: %v", err)
	}
	if _, err := cloudcred.Store(cloudcred.KindAgent, cloudcred.Credential{
		Endpoint: localconfig.CloudEndpoint, Kind: cloudcred.KindAgent,
		TenantID: "t_1234", CredentialID: "cred_agent_1", Token: cloudAgentToken,
	}); err != nil {
		t.Fatalf("store the agent's credential: %v", err)
	}
}

// pointCloudAt redirects every managed-product call at a local server for the duration of the test,
// so nothing here can reach a real burrow-cloud.dev.
func pointCloudAt(t *testing.T, url string) {
	t.Helper()
	orig := cloudBaseURL
	cloudBaseURL = url
	t.Cleanup(func() { cloudBaseURL = orig })
}

// TestSignedInCloudTargetCanBeOperatedThrough is the whole point of the change: sign in, and the
// ordinary read verbs work against the tenant, authenticated by the stored credential, with no
// cluster and no kubeconfig anywhere in the path.
func TestSignedInCloudTargetCanBeOperatedThrough(t *testing.T) {
	signedInToCloud(t)

	var gotAuth, gotPath, gotClient string
	var gotBurrowToken string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotPath = r.Header.Get("Authorization"), r.URL.Path
		gotClient = r.Header.Get("X-Burrow-Client")
		gotBurrowToken = r.Header.Get("X-Burrow-Token")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"apps":[{"app":"web","running":true}]}`))
	}))
	defer srv.Close()
	pointCloudAt(t, srv.URL)

	var out, errb bytes.Buffer
	if err := run(context.Background(), []string{"app", "list"}, &out, &errb); err != nil {
		t.Fatalf("burrow app list against a cloud target: %v\nstderr: %s", err, errb.String())
	}

	if want := "Bearer " + cloudCLIToken; gotAuth != want {
		t.Errorf("Authorization = %q, want the person's stored credential", gotAuth)
	}
	if gotBurrowToken != "" {
		t.Errorf("X-Burrow-Token = %q; the managed path authenticates with the bearer header only", gotBurrowToken)
	}
	if gotPath != "/v1/apps" {
		t.Errorf("path = %q, want the control-plane API path (no API-server proxy prefix)", gotPath)
	}
	if gotClient != "burrow" {
		t.Errorf("X-Burrow-Client = %q, want the CLI to identify itself (ADR-0039)", gotClient)
	}
	if !strings.Contains(out.String(), "web") {
		t.Errorf("stdout = %q, want the tenant's app listing", out.String())
	}
	// The target is named before the operation, so acting on the managed product is never silent.
	if !strings.Contains(errb.String(), localconfig.CloudEndpoint) {
		t.Errorf("stderr = %q, want it to name the target the command went to", errb.String())
	}
	assertNoToken(t, out.String(), errb.String())
}

// TestCloudTargetSpendsThePersonsCredentialAndNotTheAgents. Two credentials exist so that revoking
// one does not revoke the other; `burrow` must therefore never present the agent's.
func TestCloudTargetSpendsThePersonsCredentialAndNotTheAgents(t *testing.T) {
	signedInToCloud(t)

	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"apps":[]}`))
	}))
	defer srv.Close()
	pointCloudAt(t, srv.URL)

	var out, errb bytes.Buffer
	if err := run(context.Background(), []string{"app", "list"}, &out, &errb); err != nil {
		t.Fatalf("burrow app list: %v", err)
	}
	if strings.Contains(gotAuth, cloudAgentToken) {
		t.Error("the CLI presented burrow-agent's credential")
	}
	if !strings.Contains(gotAuth, cloudCLIToken) {
		t.Errorf("Authorization = %q, want the person's credential", gotAuth)
	}
}

// TestRevokedCloudCredentialIsLegible. The console can revoke a credential at any time, so a 401 on
// a machine that worked yesterday has a likely cause and a specific remedy, and both belong in the
// message. The credential's id is there so the right row can be found in the console's list.
func TestRevokedCloudCredentialIsLegible(t *testing.T) {
	signedInToCloud(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"missing or invalid authorization","code":"unauthorized"}`))
	}))
	defer srv.Close()
	pointCloudAt(t, srv.URL)

	var out, errb bytes.Buffer
	err := run(context.Background(), []string{"app", "list"}, &out, &errb)
	if err == nil {
		t.Fatal("a revoked credential produced no error")
	}
	for _, want := range []string{"revoked", "cred_cli_1", "burrow auth login"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want it to mention %q", err, want)
		}
	}
	assertNoToken(t, err.Error(), out.String(), errb.String())
}

// TestCloudTargetWithNoCredentialSaysHowToSignIn. A deleted credential file (signing this machine
// out is documented as exactly that) must produce a message naming `burrow auth login`, not a nil
// dereference and not an unexplained 401.
func TestCloudTargetWithNoCredentialSaysHowToSignIn(t *testing.T) {
	signedInToCloud(t)
	path, err := cloudcred.Path(cloudcred.KindCLI)
	if err != nil {
		t.Fatalf("Path: %v", err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatalf("remove the credential: %v", err)
	}

	var reached bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached = true
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	pointCloudAt(t, srv.URL)

	var out, errb bytes.Buffer
	err = run(context.Background(), []string{"app", "list"}, &out, &errb)
	if err == nil {
		t.Fatal("a missing credential produced no error")
	}
	if !strings.Contains(err.Error(), "burrow auth login") || !strings.Contains(err.Error(), path) {
		t.Errorf("error = %q, want it to name the missing file and the command that replaces it", err)
	}
	if reached {
		t.Error("a request was made with no credential to make it with")
	}
}

// TestCloudTargetRefusesKubeconfigFlagsByName. --context and --kubeconfig pick a cluster out of a
// kubeconfig, and the managed product has no cluster. Ignoring the flag would run the command
// somewhere the person did not ask for, so it is refused — by name, so the message is actionable.
func TestCloudTargetRefusesKubeconfigFlagsByName(t *testing.T) {
	for _, tc := range []struct{ name, flag, value string }{
		{"context", "--context", "prod-cluster"},
		{"kubeconfig", "--kubeconfig", "/some/kubeconfig"},
		{"namespace", "--namespace", "somewhere-else"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			signedInToCloud(t)
			var reached bool
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				reached = true
				w.WriteHeader(http.StatusOK)
			}))
			defer srv.Close()
			pointCloudAt(t, srv.URL)

			var out, errb bytes.Buffer
			err := run(context.Background(), []string{"app", "list", tc.flag, tc.value}, &out, &errb)
			if err == nil {
				t.Fatalf("%s was accepted against a cloud target", tc.flag)
			}
			if !strings.Contains(err.Error(), tc.flag) {
				t.Errorf("error = %q, want it to name %s", err, tc.flag)
			}
			if !strings.Contains(err.Error(), "no cluster of its own") {
				t.Errorf("error = %q, want it to say why the flag cannot apply", err)
			}
			if reached {
				t.Error("the command ran anyway")
			}
		})
	}
}

// TestClusterTargetStillMakesNoCloudCall is the regression guard on the self-hosted path. Choosing a
// cluster needs no account and must reach the managed product not at all — forbidCloud fails the
// test on any request to it, so this holds whatever the operate path does.
func TestClusterTargetStillMakesNoCloudCall(t *testing.T) {
	tempConfig(t)
	t.Setenv("BURROW_CONTROL_PLANE_URL", "")
	t.Setenv("BURROW_API_TOKEN", "")
	forbidCloud(t)

	var clusterHit bool
	cluster := fakeBurrowdCluster(&clusterHit)
	defer cluster.Close()

	kubeconfig := writeKubeconfig(t, twoContextConfig(cluster.URL, cluster.URL))
	cfg, err := localconfig.Load()
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if err := cfg.SetTarget(localconfig.KubernetesTarget("staging")); err != nil {
		t.Fatalf("SetTarget: %v", err)
	}
	if err := cfg.Save(); err != nil {
		t.Fatalf("save config: %v", err)
	}

	var out, errb bytes.Buffer
	if err := run(context.Background(), []string{"app", "status", "web", "--kubeconfig", kubeconfig}, &out, &errb); err != nil {
		t.Fatalf("burrow app status against a cluster target: %v\nstderr: %s", err, errb.String())
	}
	if !clusterHit {
		t.Error("the command did not reach the cluster it was targeted at")
	}
	if !strings.Contains(out.String(), "web") {
		t.Errorf("stdout = %q, want the app status", out.String())
	}
}

// TestClusterTargetReadsNoCloudCredential holds the other half of the separation: a self-hosted
// command must not so much as open the managed product's credential file, because a machine that
// has never signed in has none and must not be asked for one.
func TestClusterTargetReadsNoCloudCredential(t *testing.T) {
	tempConfig(t)
	t.Setenv("BURROW_CONTROL_PLANE_URL", "")
	t.Setenv("BURROW_API_TOKEN", "")
	forbidCloud(t)

	var clusterHit bool
	cluster := fakeBurrowdCluster(&clusterHit)
	defer cluster.Close()
	kubeconfig := writeKubeconfig(t, twoContextConfig(cluster.URL, cluster.URL))

	// No cloud target, no credential on disk: the pre-ADR-0078 world, which must keep working.
	var out, errb bytes.Buffer
	if err := run(context.Background(), []string{"app", "status", "web", "--kubeconfig", kubeconfig}, &out, &errb); err != nil {
		t.Fatalf("burrow app status with no target selected: %v\nstderr: %s", err, errb.String())
	}
	if !clusterHit {
		t.Error("the command did not reach the cluster")
	}
	if _, err := cloudcred.Load(cloudcred.KindCLI); err == nil {
		t.Fatal("the test fixture accidentally created a cloud credential")
	}
}

// assertNoToken fails the test if either stored credential appears anywhere it was shown. An error
// that prints a token is a security defect, not a cosmetic one.
func assertNoToken(t *testing.T, streams ...string) {
	t.Helper()
	for _, s := range streams {
		for _, token := range []string{cloudCLIToken, cloudAgentToken} {
			if strings.Contains(s, token) {
				t.Errorf("a credential was printed: %q", s)
			}
		}
	}
}

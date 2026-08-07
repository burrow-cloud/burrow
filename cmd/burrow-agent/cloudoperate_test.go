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

// The two credentials a sign-in issues, as distinct strings so a test can tell from the value alone
// which one was spent.
const (
	personsToken = "tok_the_persons_credential"
	agentsToken  = "tok_the_agents_credential"
)

// signedInToCloud puts the machine in the state `burrow auth login` leaves it in: the managed
// product selected, and both credentials on disk.
func signedInToCloud(t *testing.T) {
	t.Helper()
	writeConfig(t, `apiVersion: burrow.dev/v1
kind: Config
currentTarget: `+localconfig.CloudEndpoint+`
targets:
  - name: `+localconfig.CloudEndpoint+`
    kind: burrow-cloud
    endpoint: `+localconfig.CloudEndpoint+`
`)
	t.Setenv("BURROW_CONTROL_PLANE_URL", "")
	t.Setenv("BURROW_API_TOKEN", "")

	if _, err := cloudcred.Store(cloudcred.KindCLI, cloudcred.Credential{
		Endpoint: localconfig.CloudEndpoint, Kind: cloudcred.KindCLI,
		TenantID: "t_1234", CredentialID: "cred_cli_1", Token: personsToken,
	}); err != nil {
		t.Fatalf("store the person's credential: %v", err)
	}
	if _, err := cloudcred.Store(cloudcred.KindAgent, cloudcred.Credential{
		Endpoint: localconfig.CloudEndpoint, Kind: cloudcred.KindAgent,
		TenantID: "t_1234", CredentialID: "cred_agent_1", Token: agentsToken,
	}); err != nil {
		t.Fatalf("store the agent's credential: %v", err)
	}
}

// pointCloudAt redirects every managed-product call at a local server for the test's duration.
func pointCloudAt(t *testing.T, url string) {
	t.Helper()
	orig := cloudBaseURL
	cloudBaseURL = url
	t.Cleanup(func() { cloudBaseURL = orig })
}

// TestAgentCallsGoToTheConsoleAndNotTheApex holds the same line as the CLI's own test. The target
// this binary resolves records the product's IDENTITY (`burrow-cloud.dev`), and the origin it calls
// has to be the console: the apex serves the marketing website and answers every API path with a
// 404. The host is written out rather than derived, so redefining the constant fails here.
func TestAgentCallsGoToTheConsoleAndNotTheApex(t *testing.T) {
	const want = "https://console.burrow-cloud.dev"
	if defaultCloudBaseURL != want {
		t.Errorf("cloud base URL = %q, want %q", defaultCloudBaseURL, want)
	}
	if defaultCloudBaseURL == "https://"+localconfig.CloudEndpoint {
		t.Error("the agent calls the apex, which serves the marketing site")
	}
}

// TestAgentSpendsItsOwnCredentialAndNeverThePersons is why sign-in issues two credentials at all
// (cloud ADR-0028 §2): revoking the agent's must stop the agent without signing the person's
// terminal out, and revoking the person's must not disarm the agent. That only holds if this binary
// reads the agent's file and no other.
func TestAgentSpendsItsOwnCredentialAndNeverThePersons(t *testing.T) {
	signedInToCloud(t)

	var gotAuth, gotPath, gotClient string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotPath = r.Header.Get("Authorization"), r.URL.Path
		gotClient = r.Header.Get("X-Burrow-Client")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"apps":[{"app":"web","running":true}]}`))
	}))
	defer srv.Close()
	pointCloudAt(t, srv.URL)

	var out, errb bytes.Buffer
	if err := run(context.Background(), []string{"apps"}, &out, &errb); err != nil {
		t.Fatalf("burrow-agent apps against a cloud target: %v\nstderr: %s", err, errb.String())
	}

	if want := "Bearer " + agentsToken; gotAuth != want {
		t.Errorf("Authorization = %q, want the AGENT's stored credential", gotAuth)
	}
	if strings.Contains(gotAuth, personsToken) {
		t.Error("burrow-agent presented the person's credential")
	}
	if gotPath != "/v1/apps" {
		t.Errorf("path = %q, want the control-plane API path", gotPath)
	}
	if gotClient != "burrow-agent" {
		t.Errorf("X-Burrow-Client = %q, want burrow-agent (ADR-0039)", gotClient)
	}
	for _, s := range []string{out.String(), errb.String()} {
		if strings.Contains(s, agentsToken) || strings.Contains(s, personsToken) {
			t.Errorf("a credential was printed: %q", s)
		}
	}
}

// TestAgentWithoutItsOwnCredentialDoesNotBorrowThePersons is the negative half: with the agent's
// file gone — which is what revoking it and cleaning up looks like — the agent must fail and say so,
// not quietly fall back to the credential sitting in the next directory.
func TestAgentWithoutItsOwnCredentialDoesNotBorrowThePersons(t *testing.T) {
	signedInToCloud(t)
	path, err := cloudcred.Path(cloudcred.KindAgent)
	if err != nil {
		t.Fatalf("Path: %v", err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatalf("remove the agent credential: %v", err)
	}

	var reached bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached = true
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	pointCloudAt(t, srv.URL)

	var out, errb bytes.Buffer
	err = run(context.Background(), []string{"apps"}, &out, &errb)
	if err == nil {
		t.Fatal("burrow-agent ran with no credential of its own")
	}
	if reached {
		t.Error("a request was made without the agent's credential")
	}
	if !strings.Contains(err.Error(), path) || !strings.Contains(err.Error(), "burrow auth login") {
		t.Errorf("error = %q, want the missing file and the command that replaces it", err)
	}
	if strings.Contains(err.Error(), personsToken) {
		t.Error("the error carried the person's token")
	}
}

// TestAgentRefusesKubeconfigFlagsAgainstACloudTarget: --context picks a cluster, and a managed
// tenant has none. Ignoring it would have the agent act somewhere nobody chose.
func TestAgentRefusesKubeconfigFlagsAgainstACloudTarget(t *testing.T) {
	signedInToCloud(t)
	var out, errb bytes.Buffer
	err := run(context.Background(), []string{"apps", "--context", "prod-cluster"}, &out, &errb)
	if err == nil {
		t.Fatal("--context was accepted against a cloud target")
	}
	if !strings.Contains(err.Error(), "--context") || !strings.Contains(err.Error(), "no cluster of its own") {
		t.Errorf("error = %q, want it to name the flag and why it cannot apply", err)
	}
}

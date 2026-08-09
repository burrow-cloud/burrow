// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/burrow-cloud/burrow/client"
	"github.com/burrow-cloud/burrow/internal/clustercred"
	"github.com/burrow-cloud/burrow/localconfig"
)

// The CLI side of signing in to a self-hosted install (ADR-0084 §1). What is asserted here is the
// wiring and the wording: that a stored credential is what a command presents, that the install id
// is what finds it, and that every way the sign-in can not happen leaves a working setup and says
// which way it was.

// TestConnectPresentsTheStoredCredential is the point of the whole change: once a person has signed
// in, the token their commands carry is theirs, not the one string in the install Secret that
// everybody else also presents.
func TestConnectPresentsTheStoredCredential(t *testing.T) {
	t.Setenv("BURROW_CONFIG", filepath.Join(t.TempDir(), "config"))
	if _, err := clustercred.Store(clustercred.Credential{
		InstallID: "install-abc", Principal: "ada", Token: "ada-token",
	}); err != nil {
		t.Fatalf("Store: %v", err)
	}

	o := &commonOpts{}
	opts := o.connectOptions(target{context: "prod", installID: "install-abc"})
	if opts.Token != "ada-token" {
		t.Errorf("Token = %q, want the stored credential", opts.Token)
	}
	// The route is unchanged and still required: the kubeconfig context is how the request gets
	// there, and the credential is what says who is asking (ADR-0084 §2).
	if opts.Context != "prod" {
		t.Errorf("Context = %q, want prod — identity does not replace the route", opts.Context)
	}
	if opts.InstallID != "install-abc" {
		t.Errorf("InstallID = %q, want install-abc", opts.InstallID)
	}
}

// TestConnectFallsBackToTheSharedToken is ADR-0084's "existing installs keep working": with nobody
// signed in, or with a target that predates install ids, nothing is presented and the transport
// reads the install Secret exactly as it always has.
func TestConnectFallsBackToTheSharedToken(t *testing.T) {
	t.Setenv("BURROW_CONFIG", filepath.Join(t.TempDir(), "config"))
	o := &commonOpts{}

	if got := o.connectOptions(target{context: "prod", installID: "install-nobody-signed-in-to"}).Token; got != "" {
		t.Errorf("Token = %q, want empty so the install Secret is read", got)
	}
	if got := o.connectOptions(target{context: "prod"}).Token; got != "" {
		t.Errorf("Token = %q for a target with no install id, want empty", got)
	}
}

// TestTheCredentialIsKeyedByInstallNotContext: a credential belongs to the Burrow that issued it. A
// renamed kube context still finds it, and — the case that matters — a cluster destroyed and rebuilt
// under a name a provider generates deterministically does NOT present the previous install's token
// to the new one (ADR-0084 §5).
func TestTheCredentialIsKeyedByInstallNotContext(t *testing.T) {
	t.Setenv("BURROW_CONFIG", filepath.Join(t.TempDir(), "config"))
	if _, err := clustercred.Store(clustercred.Credential{InstallID: "install-old", Token: "old-token"}); err != nil {
		t.Fatalf("Store: %v", err)
	}
	o := &commonOpts{}

	// Same context name, renamed target: the credential follows the install.
	if got := o.connectOptions(target{context: "renamed", installID: "install-old"}).Token; got != "old-token" {
		t.Errorf("Token = %q after a rename, want the install's credential", got)
	}
	// Same context name, a rebuilt cluster running a different install: nothing is presented.
	if got := o.connectOptions(target{context: "do-nyc3-burrow", installID: "install-new"}).Token; got != "" {
		t.Errorf("Token = %q, want empty: the previous install's credential must not be presented to a new one", got)
	}
}

// stubSignInControlPlane points the sign-in at an HTTP control plane the test drives, so what the
// CLI does with each answer is asserted without a cluster. The returned recorder captures the claim
// request's body, because "what did the CLI ask for" is half of what is being pinned.
func stubSignInControlPlane(t *testing.T, status int, body string) *[]byte {
	t.Helper()
	var asked []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/auth/claim" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		asked, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)

	orig := signInTransport
	signInTransport = func(string, string, string) client.Transport {
		return client.DirectTransport{BaseURL: srv.URL, Token: "shared-install-token", Name: client.ClientNameCLI}
	}
	t.Cleanup(func() { signInTransport = orig })
	return &asked
}

// TestSignInStoresTheCredentialAndRecordsTheInstall is the sign-in path from the CLI's side: the
// token comes back once, lands in a file only its owner can read, and the install that issued it is
// recorded on the target so the next command can find it again.
func TestSignInStoresTheCredentialAndRecordsTheInstall(t *testing.T) {
	t.Setenv("BURROW_CONFIG", filepath.Join(t.TempDir(), "config"))
	asked := stubSignInControlPlane(t, http.StatusOK, `{
		"principal_id":"p-1","principal":"ada","admin":true,
		"credential_id":"c-1","kind":"user","install_id":"install-abc","token":"ada-token"}`)

	tgt := localconfig.KubernetesTarget("do-nyc1-cluster")
	got := signInToCluster(context.Background(), "", "ada", &tgt)

	if !got.Issued {
		t.Fatalf("Issued = false, want true; line = %q", got.Line)
	}
	if tgt.InstallID != "install-abc" {
		t.Errorf("target InstallID = %q, want install-abc — the credential is filed under it", tgt.InstallID)
	}
	cred, err := clustercred.Load("install-abc")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cred.Token != "ada-token" || cred.Principal != "ada" || cred.CredentialID != "c-1" {
		t.Errorf("stored %+v, want the issued credential", cred)
	}
	// The claim asks for a name and nothing else. A kind in the body would let the caller choose
	// which guardrails bind it (ADR-0084 §3).
	if strings.Contains(string(*asked), "kind") {
		t.Errorf("claim body = %s, want only the name", *asked)
	}
	if !strings.Contains(got.Line, "ada") || !strings.Contains(got.Line, "admin") {
		t.Errorf("line = %q, want it to name the principal and that they are this install's admin", got.Line)
	}
}

// TestSignInOnAClaimedInstallLeavesAWorkingSetup: the ordinary answer for the second person onwards.
// The target is still recorded, no credential is written, and the line says what to do — the shared
// install token keeps working, which is ADR-0084's "existing installs keep working".
func TestSignInOnAClaimedInstallLeavesAWorkingSetup(t *testing.T) {
	t.Setenv("BURROW_CONFIG", filepath.Join(t.TempDir(), "config"))
	stubSignInControlPlane(t, http.StatusConflict, `{"error":"already claimed","code":"already_claimed"}`)

	tgt := localconfig.KubernetesTarget("do-nyc1-cluster")
	got := signInToCluster(context.Background(), "", "grace", &tgt)

	if got.Issued {
		t.Error("Issued = true on an install that already has an admin")
	}
	if tgt.InstallID != "" {
		t.Errorf("target InstallID = %q, want empty: no id was learned, so the target stays unchecked", tgt.InstallID)
	}
	if !strings.Contains(got.Line, "admin") || !strings.Contains(got.Line, "shared token") {
		t.Errorf("line = %q, want it to name the admin and say the shared token still works", got.Line)
	}
}

// TestSignInAsksForNothingItCannotSave is the ordering that matters most here. burrowd returns a
// token once, so a write that fails afterwards destroys a credential that already exists on the
// server — and on a FIRST claim that leaves the install claimed by a principal whose only token is
// gone, which no second claim recovers. Nothing may be minted until the write is known to work.
func TestSignInAsksForNothingItCannotSave(t *testing.T) {
	// A file where the credentials directory should be: nothing can be created inside it.
	home := t.TempDir()
	t.Setenv("BURROW_CONFIG", filepath.Join(home, "config"))
	if err := os.WriteFile(filepath.Join(home, "credentials"), []byte("not a directory"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	asked := stubSignInControlPlane(t, http.StatusOK, `{"principal_id":"p-1","principal":"ada","install_id":"install-abc","token":"ada-token"}`)

	tgt := localconfig.KubernetesTarget("do-nyc1-cluster")
	got := signInToCluster(context.Background(), "", "ada", &tgt)

	if got.Issued {
		t.Error("Issued = true when the credential could not have been saved")
	}
	if len(*asked) != 0 {
		t.Errorf("a claim was sent (%s) despite there being nowhere to put the result", *asked)
	}
	if !strings.Contains(got.Line, "Nothing was changed on the cluster") {
		t.Errorf("line = %q, want it to say the cluster was not touched", got.Line)
	}
}

// TestSignInAgainstAnUnreachableClusterDoesNotFail: choosing where you use Burrow has to work
// against a cluster that is down or has no Burrow in it. The sign-in reports and returns; the
// caller records the target either way.
func TestSignInAgainstAnUnreachableClusterDoesNotFail(t *testing.T) {
	t.Setenv("BURROW_CONFIG", filepath.Join(t.TempDir(), "config"))
	orig := signInTransport
	signInTransport = func(string, string, string) client.Transport { return failingTransport{} }
	t.Cleanup(func() { signInTransport = orig })

	tgt := localconfig.KubernetesTarget("do-nyc1-cluster")
	got := signInToCluster(context.Background(), "", "ada", &tgt)

	if got.Issued {
		t.Error("Issued = true against a cluster that never answered")
	}
	if !strings.Contains(got.Line, "shared token") {
		t.Errorf("line = %q, want it to say the shared token keeps working", got.Line)
	}
	if !strings.Contains(got.Line, "do-nyc1-cluster") {
		t.Errorf("line = %q, want it to name the context to try again against", got.Line)
	}
}

// failingTransport is a control plane that cannot be reached at all.
type failingTransport struct{}

func (failingTransport) Connect(context.Context) (*client.Client, error) {
	return nil, errors.New("the cluster is unreachable")
}

// TestLoginReportsTheSignInAndStillRecordsTheTarget puts the two halves together at the command
// level: `burrow auth login --context <cluster>` prints what the sign-in did, and records the target
// either way. The target is what somebody came for; the credential is the improvement.
func TestLoginReportsTheSignInAndStillRecordsTheTarget(t *testing.T) {
	stubAuth(t, authContexts(), false)
	stubSignInControlPlane(t, http.StatusOK, `{
		"principal_id":"p-1","principal":"ada","admin":true,
		"credential_id":"c-1","kind":"user","install_id":"install-abc","token":"ada-token"}`)

	var out bytes.Buffer
	if err := runAuthLogin(context.Background(), authLoginOpts{kubeContext: "do-nyc1-cluster", name: "ada"}, strings.NewReader(""), &out); err != nil {
		t.Fatalf("runAuthLogin: %v", err)
	}
	if !strings.Contains(out.String(), "signed in as ada") {
		t.Errorf("output does not report the credential:\n%s", out.String())
	}
	cfg := loadAuthConfig(t)
	tgt, ok := cfg.LookupTarget("do-nyc1-cluster")
	if !ok {
		t.Fatalf("target was not recorded: %+v", cfg.Targets)
	}
	// The id is written on the SAME entry SetTarget wrote, not on a second pass over the config.
	if tgt.InstallID != "install-abc" {
		t.Errorf("recorded target InstallID = %q, want install-abc", tgt.InstallID)
	}
}

// TestLoginRecordsTheTargetWhenNoCredentialIsIssued is the other half, and the one that must not
// regress: choosing where you use Burrow works against a cluster that will not issue one.
func TestLoginRecordsTheTargetWhenNoCredentialIsIssued(t *testing.T) {
	stubAuth(t, authContexts(), false)
	stubSignInControlPlane(t, http.StatusConflict, `{"error":"already claimed","code":"already_claimed"}`)

	var out bytes.Buffer
	if err := runAuthLogin(context.Background(), authLoginOpts{kubeContext: "do-nyc1-cluster"}, strings.NewReader(""), &out); err != nil {
		t.Fatalf("runAuthLogin: %v", err)
	}
	if got := loadAuthConfig(t).CurrentTarget; got != "do-nyc1-cluster" {
		t.Errorf("active target = %q, want do-nyc1-cluster", got)
	}
	if !strings.Contains(out.String(), "shared token") {
		t.Errorf("output does not say the shared token keeps working:\n%s", out.String())
	}
}

// TestClaimRefusalNamesTheNextStep: each way burrowd can decline has a different next step, and a
// person reading the line is deciding what to do rather than debugging Burrow. None of them may read
// as the sign-in having broken, because in every case the target is recorded and commands work.
func TestClaimRefusalNamesTheNextStep(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want []string
	}{
		{
			// The ordinary answer for the second person onwards.
			name: "already claimed",
			err:  &client.APIError{StatusCode: http.StatusConflict, Code: client.CodeAlreadyClaimed, Message: "already claimed"},
			want: []string{"admin", "shared token"},
		},
		{
			// A control plane older than this CLI. Nothing is wrong with the install.
			name: "route absent",
			err:  &client.APIError{StatusCode: http.StatusNotFound, Code: client.CodeUnknownOperation, Message: "unknown"},
			want: []string{"burrow cluster upgrade", "shared token"},
		},
		{
			name: "anything else",
			err:  errors.New("the cluster refused the connection"),
			want: []string{"the cluster refused the connection", "shared token"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			line := claimRefusalLine(tc.err)
			for _, want := range tc.want {
				if !strings.Contains(line, want) {
					t.Errorf("line %q, want substring %q", line, want)
				}
			}
		})
	}
}

// TestARefusalNeverCarriesMoreThanALine: a note that runs to a page reads as a failure. The full
// error belongs to a command that failed, and choosing where you use Burrow did not.
func TestARefusalNeverCarriesMoreThanALine(t *testing.T) {
	line := claimRefusalLine(fmt.Errorf("first line\nsecond line\nthird line"))
	if strings.Contains(line, "second line") {
		t.Errorf("line %q should carry only the error's first line", line)
	}
}

// TestPrincipalNamePrefersTheFlag: the name is the handle an audit row reads as, so somebody whose
// shell username is not what the trail should say can set it. It authenticates nobody — burrowd
// authenticates the token it issues, never this string.
func TestPrincipalNamePrefersTheFlag(t *testing.T) {
	t.Setenv("BURROW_PRINCIPAL", "")
	t.Setenv("USER", "shell-user")

	if got := (authLoginOpts{name: " ada "}).principalName(); got != "ada" {
		t.Errorf("principalName = %q, want ada", got)
	}
	if got := (authLoginOpts{}).principalName(); got != "shell-user" {
		t.Errorf("principalName = %q, want the environment's answer", got)
	}
}

// TestPrincipalNameAlwaysAnswers: a machine with none of the usual variables set still signs in,
// under a name that is obviously a default rather than an empty string. A principal with no name is
// indistinguishable from every other one in the listing this record exists to fix.
func TestPrincipalNameAlwaysAnswers(t *testing.T) {
	for _, key := range []string{"BURROW_PRINCIPAL", "USER", "USERNAME", "LOGNAME"} {
		t.Setenv(key, "")
	}
	if got := (authLoginOpts{}).principalName(); got != "operator" {
		t.Errorf("principalName = %q, want operator", got)
	}
}

// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package main

import (
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/burrow-cloud/burrow/client"
	"github.com/burrow-cloud/burrow/internal/clustercred"
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

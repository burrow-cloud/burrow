// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/burrow-cloud/burrow/client"
	"github.com/burrow-cloud/burrow/internal/clustercred"
	"github.com/burrow-cloud/burrow/localconfig"
)

// The CLI's side of giving a second person access (ADR-0084 §2).
//
// The invited person's whole path runs here: they hold an invitation and a kubeconfig context, and
// what they end up with is a credential of their own, filed under the install that issued it, with
// the invitation spent server-side. What these pin is the part the code can get subtly wrong — that
// the INVITATION is what is presented on the wire, and the CREDENTIAL is what is written to disk.

// stubExchange stands up a control plane that answers the exchange, records the token presented to
// it, and returns body with status. The transport forwards whatever token the sign-in was given, so
// the test can assert that the invitation is what travelled.
// It also answers the agent-credential route the sign-in asks for next, so the second call is the
// one it is on a real install rather than a 404 the test would have to reason about.
func stubExchange(t *testing.T, status int, body string) *string {
	t.Helper()
	var presented string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/auth/redeem":
			// Recorded here and not for every request: what matters is what was spent on the
			// exchange, and the call after it deliberately presents something else.
			presented = r.Header.Get("X-Burrow-Token")
			w.WriteHeader(status)
			_, _ = w.Write([]byte(body))
		case "/v1/auth/agent":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"principal_id":"p-2","principal":"ada","credential_id":"c-3",` +
				`"kind":"agent","install_id":"install-abc","token":"ada-agent-token"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)

	orig := signInTransport
	signInTransport = func(_, _, token string) client.Transport {
		return client.DirectTransport{BaseURL: srv.URL, Token: token, Name: client.ClientNameCLI}
	}
	t.Cleanup(func() { signInTransport = orig })
	return &presented
}

// TestAcceptingAnInvitationStoresTheCredentialItWasGiven: the invitation goes out, a credential
// comes back, and it is the credential that is written down.
func TestAcceptingAnInvitationStoresTheCredentialItWasGiven(t *testing.T) {
	t.Setenv("BURROW_CONFIG", filepath.Join(t.TempDir(), "config"))
	presented := stubExchange(t, http.StatusOK, `{
		"principal_id":"p-2","principal":"ada","admin":false,
		"credential_id":"c-2","kind":"user","install_id":"install-abc","token":"ada-credential"}`)

	tgt := localconfig.KubernetesTarget("do-nyc1-cluster")
	got, err := acceptInvitation(context.Background(), "", "the-invitation", &tgt)
	if err != nil {
		t.Fatalf("acceptInvitation: %v", err)
	}

	if !got.Issued {
		t.Fatalf("Issued = false, want true; line = %q", got.Line)
	}
	if *presented != "the-invitation" {
		t.Errorf("presented %q on the wire, want the invitation: the second person has no shared token to fall back on", *presented)
	}
	if tgt.InstallID != "install-abc" {
		t.Errorf("target InstallID = %q, want install-abc — it is what names the credential file", tgt.InstallID)
	}
	cred, err := clustercred.Load(clustercred.KindCLI, "install-abc")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cred.Token != "ada-credential" {
		t.Errorf("stored token is not the one the exchange returned: %+v", cred)
	}
	if cred.Token == "the-invitation" {
		t.Error("the invitation was stored as the credential; the exchange exists so that it is not")
	}
	if !strings.Contains(got.Line, "ada") || !strings.Contains(got.Line, "spent") {
		t.Errorf("line = %q, want it to name the principal and say the invitation is spent", got.Line)
	}

	// And the agent's, which the same sign-in issues (ADR-0084 §3). It is a SEPARATE file, so that
	// revoking one does not stop the other.
	if !got.Agent.Issued {
		t.Fatalf("the agent got no credential of its own; line = %q", got.Agent.Line)
	}
	agentCred, err := clustercred.Load(clustercred.KindAgent, "install-abc")
	if err != nil {
		t.Fatalf("Load(agent): %v", err)
	}
	if agentCred.Token != "ada-agent-token" {
		t.Errorf("the agent's stored token is %+v, want the one the agent route issued", agentCred)
	}
	if agentCred.Token == cred.Token {
		t.Error("the agent and the person are holding the same token; revoking either would stop both")
	}
	if agentPath, _ := clustercred.Path(clustercred.KindAgent, "install-abc"); agentPath == mustPath(t, clustercred.KindCLI, "install-abc") {
		t.Error("both credentials are in the same file")
	}
}

func mustPath(t *testing.T, kind clustercred.Kind, installID string) string {
	t.Helper()
	p, err := clustercred.Path(kind, installID)
	if err != nil {
		t.Fatalf("Path(%s): %v", kind, err)
	}
	return p
}

// TestAnInvitationThatIsRefusedFailsTheCommand: unlike a claim, there is nothing to fall back to.
// Recording a target this person cannot authenticate to would leave them with a Burrow that answers
// every command with a 401 and nothing to explain it.
func TestAnInvitationThatIsRefusedFailsTheCommand(t *testing.T) {
	t.Setenv("BURROW_CONFIG", filepath.Join(t.TempDir(), "config"))
	stubExchange(t, http.StatusUnauthorized, `{"error":"this credential expired on 2026-08-07T09:00:00Z","code":"credential_not_live"}`)

	tgt := localconfig.KubernetesTarget("do-nyc1-cluster")
	_, err := acceptInvitation(context.Background(), "", "stale-invitation", &tgt)
	if err == nil {
		t.Fatal("a refused exchange returned no error, so the login would record a target with no way in")
	}
	if !strings.Contains(err.Error(), "another") {
		t.Errorf("error = %q, want it to say to ask for another invitation", err)
	}
	if tgt.InstallID != "" {
		t.Errorf("target InstallID = %q, want empty: nothing was exchanged", tgt.InstallID)
	}
}

// TestAnInvitationAgainstAnOldControlPlaneSaysSo: a Burrow without the exchange route is not a
// broken invitation, and the remedy is an upgrade rather than a second invitation.
func TestAnInvitationAgainstAnOldControlPlaneSaysSo(t *testing.T) {
	t.Setenv("BURROW_CONFIG", filepath.Join(t.TempDir(), "config"))
	stubExchange(t, http.StatusNotFound, `{"error":"unknown operation","code":"unknown_operation"}`)

	tgt := localconfig.KubernetesTarget("do-nyc1-cluster")
	_, err := acceptInvitation(context.Background(), "", "the-invitation", &tgt)
	if err == nil {
		t.Fatal("an exchange against a control plane without the route returned no error")
	}
	if !strings.Contains(err.Error(), "cluster upgrade") {
		t.Errorf("error = %q, want it to name the upgrade", err)
	}
}

// TestInviteFlagCombinationsAreRefusedByName: somebody exchanging an invitation is doing it for the
// first time, from instructions. A flag accepted and ignored would leave them signed in some other
// way with nothing said about it.
func TestInviteFlagCombinationsAreRefusedByName(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts authLoginOpts
		want string
	}{
		{"cloud", authLoginOpts{invite: "x", cloud: true}, "managed product"},
		{"no context", authLoginOpts{invite: "x"}, "--context"},
		{"with a name", authLoginOpts{invite: "x", kubeContext: "do-nyc1", name: "ada"}, "--name"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.opts.checkInvite()
			if err == nil {
				t.Fatalf("checkInvite() = nil, want a refusal naming %q", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("checkInvite() = %q, want it to name %q", err, tc.want)
			}
		})
	}

	ok := authLoginOpts{invite: "x", kubeContext: "do-nyc1"}
	if err := ok.checkInvite(); err != nil {
		t.Errorf("an invitation with a context was refused: %v", err)
	}
	if err := (authLoginOpts{cloud: true}).checkInvite(); err != nil {
		t.Errorf("a sign-in with no invitation was refused: %v", err)
	}
}

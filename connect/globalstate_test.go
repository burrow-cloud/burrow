// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package connect

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd/api"
)

// plainKubeconfig builds a single-context kubeconfig whose cluster is a plain-HTTP endpoint with no
// credentials and no CA — the shape `kubectl proxy` produces, and the one where client-go's
// rest.HTTPClientFor resolves to the DEFAULT transport and therefore hands back the process-global
// http.DefaultClient rather than a client of its own. Every test in this file uses it because it is
// the shape under which a write to the returned client escapes the connection that made it.
func plainKubeconfig(t *testing.T, server string) string {
	t.Helper()
	cfg := api.NewConfig()
	cfg.Clusters["plain"] = &api.Cluster{Server: server}
	cfg.AuthInfos["none"] = &api.AuthInfo{}
	cfg.Contexts["plain"] = &api.Context{Cluster: "plain", AuthInfo: "none"}
	cfg.CurrentContext = "plain"
	return writeKubeconfig(t, cfg)
}

// burrowdServer is a fake that answers the token-Secret Get with tok and records the headers of
// every request that arrives on the service-proxy path — that is, of the control-plane calls the
// returned Client makes.
func burrowdServer(tok string, seen *[]http.Header) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The token-Secret Get is a plain API call; anything routed through the service proxy is a
		// control-plane request the returned Client made, and its headers are what is under test.
		if strings.Contains(r.URL.Path, "/proxy") {
			*seen = append(*seen, r.Header.Clone())
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(&corev1.Secret{
			TypeMeta:   metav1.TypeMeta{Kind: "Secret", APIVersion: "v1"},
			ObjectMeta: metav1.ObjectMeta{Name: "burrowd-api-token", Namespace: "burrow"},
			Data:       map[string][]byte{"token": []byte(tok)},
		})
	}))
}

// TestClientLeavesTheDefaultHTTPClientAlone is the disclosure check for issue #459. Client used to
// install its token RoundTripper on whatever *http.Client rest.HTTPClientFor returned, and for a
// credential-free plain-HTTP kubeconfig that IS http.DefaultClient — so after one connect, every
// unrelated request the process made through the default client carried the control-plane API token
// to whoever it was addressed to (`burrow version` reaching api.github.com, `apply` fetching a
// manifest URL, the public-IP echo service). Connecting must leave process-global state untouched.
func TestClientLeavesTheDefaultHTTPClientAlone(t *testing.T) {
	var seen []http.Header
	srv := burrowdServer("s3cr3t", &seen)
	defer srv.Close()

	before := http.DefaultClient.Transport
	t.Cleanup(func() { http.DefaultClient.Transport = before })

	path := plainKubeconfig(t, srv.URL)
	if _, err := Client(context.Background(), Options{
		Kubeconfig:    path,
		Namespace:     "burrow",
		ClientName:    "burrow",
		ClientVersion: "v9.9.9",
		InstallID:     "install-one",
	}); err != nil {
		t.Fatalf("Client: %v", err)
	}

	if http.DefaultClient.Transport != before {
		t.Errorf("connecting replaced http.DefaultClient.Transport (%T); it is process-global state and must not be written", http.DefaultClient.Transport)
	}

	// The load-bearing assertion: an unrelated outbound request, of the kind `burrow version` and
	// `apply` make, must reach a third party with no Burrow header on it.
	var third http.Header
	party := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		third = r.Header.Clone()
	}))
	defer party.Close()

	req, err := http.NewRequest(http.MethodGet, party.URL, nil)
	if err != nil {
		t.Fatalf("building the third-party request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("third-party request: %v", err)
	}
	resp.Body.Close()

	for _, h := range []string{"X-Burrow-Token", "X-Burrow-Client", "X-Burrow-Client-Version", "X-Burrow-Install"} {
		if v := third.Get(h); v != "" {
			t.Errorf("a third party received %s: %q — connecting disclosed the control-plane credential to an unrelated host", h, v)
		}
	}
}

// TestTwoConnectsDoNotShareHeaders is issue #459 as filed: a second connect in the same process must
// send its OWN token and install id, not the first connection's, and must not stack a second layer
// of header-setting on top of the first. Wrapping an already-wrapped transport is silent — the inner
// (older) round tripper runs last and overwrites what the outer one set — so the second connection
// answers with the first's credential and the first's install id, which is the check ADR-0084 §5
// exists to make trustworthy.
func TestTwoConnectsDoNotShareHeaders(t *testing.T) {
	var seenOne, seenTwo []http.Header
	one := burrowdServer("token-one", &seenOne)
	defer one.Close()
	two := burrowdServer("token-two", &seenTwo)
	defer two.Close()

	before := http.DefaultClient.Transport
	t.Cleanup(func() { http.DefaultClient.Transport = before })

	ctx := context.Background()
	cOne, err := Client(ctx, Options{Kubeconfig: plainKubeconfig(t, one.URL), Namespace: "burrow", ClientName: "burrow", ClientVersion: "v1.0.0", InstallID: "install-one"})
	if err != nil {
		t.Fatalf("first Client: %v", err)
	}
	cTwo, err := Client(ctx, Options{Kubeconfig: plainKubeconfig(t, two.URL), Namespace: "burrow", ClientName: "burrow", ClientVersion: "v1.0.0", InstallID: "install-two"})
	if err != nil {
		t.Fatalf("second Client: %v", err)
	}

	// Drive the second connection first: under the old behaviour it is the one that answers with
	// the other connection's headers.
	if _, err := cTwo.ListEnvironments(ctx); err != nil {
		t.Fatalf("second connection ListEnvironments: %v", err)
	}
	if _, err := cOne.ListEnvironments(ctx); err != nil {
		t.Fatalf("first connection ListEnvironments: %v", err)
	}

	check := func(label string, seen []http.Header, wantToken, wantInstall string) {
		t.Helper()
		if len(seen) != 1 {
			t.Fatalf("%s: burrowd saw %d control-plane requests, want 1", label, len(seen))
		}
		h := seen[0]
		if got := h.Values("X-Burrow-Token"); len(got) != 1 {
			t.Errorf("%s: X-Burrow-Token appeared %d times (%v), want exactly once", label, len(got), got)
		}
		if got := h.Get("X-Burrow-Token"); got != wantToken {
			t.Errorf("%s: X-Burrow-Token = %q, want %q — the connection sent another connection's credential", label, got, wantToken)
		}
		if got := h.Get("X-Burrow-Install"); got != wantInstall {
			t.Errorf("%s: X-Burrow-Install = %q, want %q — the install check answered for the wrong install (ADR-0084 §5)", label, got, wantInstall)
		}
	}
	check("second connection", seenTwo, "token-two", "install-two")
	check("first connection", seenOne, "token-one", "install-one")
}

// TestClientSendsTheTokenItRead is the unchanged-behaviour check: the credential burrowd is meant to
// receive still arrives, alongside the ADR-0039 handshake headers and the ADR-0084 §5 install id, on
// the transport this package now owns rather than on a borrowed one.
func TestClientSendsTheTokenItRead(t *testing.T) {
	var seen []http.Header
	srv := burrowdServer("s3cr3t", &seen)
	defer srv.Close()

	c, err := Client(context.Background(), Options{
		Kubeconfig:    plainKubeconfig(t, srv.URL),
		Namespace:     "burrow",
		ClientName:    "burrow",
		ClientVersion: "v4.5.6",
		InstallID:     "install-abc",
	})
	if err != nil {
		t.Fatalf("Client: %v", err)
	}
	if _, err := c.ListEnvironments(context.Background()); err != nil {
		t.Fatalf("ListEnvironments: %v", err)
	}
	if len(seen) != 1 {
		t.Fatalf("burrowd saw %d control-plane requests, want 1", len(seen))
	}
	for _, want := range []struct{ header, value string }{
		{"X-Burrow-Token", "s3cr3t"},
		{"X-Burrow-Client", "burrow"},
		{"X-Burrow-Client-Version", "v4.5.6"},
		{"X-Burrow-Install", "install-abc"},
	} {
		if got := seen[0].Get(want.header); got != want.value {
			t.Errorf("%s = %q, want %q", want.header, got, want.value)
		}
	}
}

// TestClientOwnsItsHTTPClient states the rule directly: the client Connect builds is its own, never
// the process-global default, whatever config it was handed. It is the invariant the two tests above
// depend on, asserted where a future refactor would trip over it.
func TestClientOwnsItsHTTPClient(t *testing.T) {
	var seen []http.Header
	srv := burrowdServer("s3cr3t", &seen)
	defer srv.Close()

	path := plainKubeconfig(t, srv.URL)
	cfg, err := RESTConfig(path, "")
	if err != nil {
		t.Fatalf("RESTConfig: %v", err)
	}
	// Guard the premise: this config is one where client-go WOULD hand back the shared default
	// client. If that ever stops being true the tests above stop testing anything, silently.
	hc, err := rest.HTTPClientFor(cfg)
	if err != nil {
		t.Fatalf("HTTPClientFor: %v", err)
	}
	if hc != http.DefaultClient {
		t.Skip("client-go no longer returns http.DefaultClient for a credential-free plain-HTTP config; the disclosure premise no longer holds")
	}

	before := http.DefaultClient.Transport
	t.Cleanup(func() { http.DefaultClient.Transport = before })

	if _, err := Client(context.Background(), Options{Kubeconfig: path, Namespace: "burrow"}); err != nil {
		t.Fatalf("Client: %v", err)
	}
	if http.DefaultClient.Transport != before {
		t.Fatalf("Client mutated http.DefaultClient rather than building a client of its own")
	}
}

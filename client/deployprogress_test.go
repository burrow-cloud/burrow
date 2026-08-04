// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package client_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/burrow-cloud/burrow/client"
)

// The client half of the deploy progress stream (issue #480). What matters here is that asking for
// progress changes NOTHING a caller can otherwise observe: the same result, the same *APIError, and
// a control plane that does not offer a stream is handled without the caller noticing.

// recordProgress collects the stages a deploy reports, as "stage:status".
func recordProgress(got *[]string) func(client.DeployProgress) {
	return func(p client.DeployProgress) { *got = append(*got, p.Stage+":"+p.Status) }
}

// ndjsonServer answers a deploy with the given already-rendered ndjson lines and records what the
// client asked for.
func ndjsonServer(t *testing.T, accept *string, lines ...string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if accept != nil {
			*accept = r.Header.Get("Accept")
		}
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.WriteHeader(http.StatusOK)
		for _, l := range lines {
			_, _ = io.WriteString(w, l+"\n")
			w.(http.Flusher).Flush()
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestDeployDecodesTheProgressStream is the happy path: the client asks for ndjson, reports each
// stage as it arrives, and returns the result from the stream's last line.
func TestDeployDecodesTheProgressStream(t *testing.T) {
	var accept string
	srv := ndjsonServer(t, &accept,
		`{"event":{"stage":"apply","status":"started"}}`,
		`{"event":{"stage":"apply","status":"done"}}`,
		`{"event":{"stage":"settle","status":"started"}}`,
		`{"event":{"stage":"settle","status":"failed"}}`,
		`{"result":{"release":{"id":"r1","app":"web","image":"img:1","status":"deployed"},"superseded_release_id":"r0"}}`,
	)

	var got []string
	c := client.NewClient(srv.URL, "tok")
	res, err := c.Deploy(context.Background(), "web", client.DeployRequest{
		Image: "img:1", Replicas: 1, Progress: recordProgress(&got),
	})
	if err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	if accept != "application/x-ndjson" {
		t.Errorf("Accept = %q, want application/x-ndjson", accept)
	}
	want := "apply:started,apply:done,settle:started,settle:failed"
	if strings.Join(got, ",") != want {
		t.Errorf("stages = %v, want %s", got, want)
	}
	if res.Release.ID != "r1" || res.SupersededReleaseID != "r0" {
		t.Errorf("result = %+v", res)
	}
}

// TestDeployWithoutProgressAsksForNoStream pins the opt-in. A caller that sets no reporter — which
// is `burrow-agent`, the one that must not be flooded with ticks — sends no ndjson Accept header and
// takes the path this client has always taken.
func TestDeployWithoutProgressAsksForNoStream(t *testing.T) {
	var accept string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		accept = r.Header.Get("Accept")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"release":{"id":"r1","app":"web","image":"img:1","status":"deployed"}}`)
	}))
	defer srv.Close()

	c := client.NewClient(srv.URL, "tok")
	res, err := c.Deploy(context.Background(), "web", client.DeployRequest{Image: "img:1", Replicas: 1})
	if err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	if strings.Contains(accept, "x-ndjson") {
		t.Errorf("Accept = %q, want no ndjson: a caller that set no Progress must not ask for one", accept)
	}
	if res.Release.ID != "r1" {
		t.Errorf("result = %+v", res)
	}
}

// TestDeployAgainstAControlPlaneWithNoStream is the compatibility case an install mid-upgrade is in:
// a server that predates this ignores the Accept header and answers with the single JSON object it
// always has. The client must take that answer, report no stages, and succeed.
func TestDeployAgainstAControlPlaneWithNoStream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"release":{"id":"r1","app":"web","image":"img:1","status":"deployed"}}`)
	}))
	defer srv.Close()

	var got []string
	c := client.NewClient(srv.URL, "tok")
	res, err := c.Deploy(context.Background(), "web", client.DeployRequest{
		Image: "img:1", Replicas: 1, Progress: recordProgress(&got),
	})
	if err != nil {
		t.Fatalf("Deploy against a control plane with no stream: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("stages = %v, want none: the server reported none", got)
	}
	if res.Release.ID != "r1" {
		t.Errorf("result = %+v", res)
	}
}

// TestHeldDeployIsAnAPIErrorEvenWithProgressSet is the load-bearing one. A guardrail hold arrives as
// an ordinary status-coded body because the control plane has written nothing yet, and the client
// must build the same *APIError it always did — NeedsConfirmation and all — or `burrow-agent` would
// relay a hold as a failure.
func TestHeldDeployIsAnAPIErrorEvenWithProgressSet(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = io.WriteString(w, `{"error":"app.deploy is held","code":"app.deploy","needs_confirmation":true}`)
	}))
	defer srv.Close()

	var got []string
	c := client.NewClient(srv.URL, "tok")
	_, err := c.Deploy(context.Background(), "web", client.DeployRequest{
		Image: "img:1", Replicas: 1, Progress: recordProgress(&got),
	})
	var api *client.APIError
	if !errors.As(err, &api) {
		t.Fatalf("Deploy err = %v, want an *APIError", err)
	}
	if api.StatusCode != http.StatusUnprocessableEntity || api.Code != "app.deploy" || !api.NeedsConfirmation {
		t.Errorf("APIError = %+v, want 422 app.deploy with needs_confirmation", api)
	}
	if len(got) != 0 {
		t.Errorf("stages = %v, want none on a refusal", got)
	}
}

// TestAnErrorLineRebuildsTheSameAPIError covers a failure raised after the first event, which cannot
// carry a status line of its own. The status and code travel inside the stream and the client
// rebuilds an *APIError indistinguishable from the non-streaming one.
func TestAnErrorLineRebuildsTheSameAPIError(t *testing.T) {
	srv := ndjsonServer(t, nil,
		`{"event":{"stage":"pre-deploy-hook","status":"started"}}`,
		`{"event":{"stage":"pre-deploy-hook","status":"failed"}}`,
		`{"error":{"status":422,"error":"the pre-deploy hook exited with exit code 1","code":"hook_failed"}}`,
	)

	var got []string
	c := client.NewClient(srv.URL, "tok")
	_, err := c.Deploy(context.Background(), "web", client.DeployRequest{
		Image: "img:1", Replicas: 1, Progress: recordProgress(&got),
	})
	var api *client.APIError
	if !errors.As(err, &api) {
		t.Fatalf("Deploy err = %v, want an *APIError", err)
	}
	if api.StatusCode != 422 || api.Code != "hook_failed" {
		t.Errorf("APIError = %+v, want 422 hook_failed", api)
	}
	if api.NeedsConfirmation {
		t.Error("needs_confirmation is set on a failed hook; there is nothing to confirm")
	}
	if !strings.Contains(api.Message, "exit code 1") {
		t.Errorf("message = %q, want the hook's own detail", api.Message)
	}
	if strings.Join(got, ",") != "pre-deploy-hook:started,pre-deploy-hook:failed" {
		t.Errorf("stages = %v", got)
	}
}

// TestATruncatedStreamIsAnError asserts a stream that ends with neither a result nor an error does
// not look like a successful deploy with an empty result. It says the deploy may still be running,
// because it may be: burrowd going away does not undo what it already did.
func TestATruncatedStreamIsAnError(t *testing.T) {
	srv := ndjsonServer(t, nil, `{"event":{"stage":"apply","status":"started"}}`)

	var got []string
	c := client.NewClient(srv.URL, "tok")
	_, err := c.Deploy(context.Background(), "web", client.DeployRequest{
		Image: "img:1", Replicas: 1, Progress: recordProgress(&got),
	})
	if err == nil {
		t.Fatal("Deploy succeeded on a stream that reported no outcome")
	}
	if !strings.Contains(err.Error(), "may still be in progress") {
		t.Errorf("err = %v, want it to say the deploy may still be running", err)
	}
}

// TestAnUnknownStageIsPassedThroughUnchanged pins that a newer control plane's stage reaches the
// caller rather than being dropped or rejected here. Rendering it is the caller's problem, and the
// CLI has an answer for it; refusing it in the client would break a new server against an old client
// for no gain.
func TestAnUnknownStageIsPassedThroughUnchanged(t *testing.T) {
	srv := ndjsonServer(t, nil,
		`{"event":{"stage":"warming-the-cache","status":"started"}}`,
		`{"event":{"stage":"warming-the-cache","status":"done"}}`,
		`{"result":{"release":{"id":"r1","app":"web","image":"img:1","status":"deployed"}}}`,
	)

	var got []string
	c := client.NewClient(srv.URL, "tok")
	if _, err := c.Deploy(context.Background(), "web", client.DeployRequest{
		Image: "img:1", Replicas: 1, Progress: recordProgress(&got),
	}); err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	if strings.Join(got, ",") != "warming-the-cache:started,warming-the-cache:done" {
		t.Errorf("stages = %v, want the unknown stage passed through", got)
	}
}

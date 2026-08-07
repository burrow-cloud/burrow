// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package api_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	cp "github.com/burrow-cloud/burrow/controlplane"
	"github.com/burrow-cloud/burrow/controlplane/api"
	"github.com/burrow-cloud/burrow/controlplane/internal/fake"
)

// The build progress stream (issue #503). A build runs for minutes and used to write nothing until it
// was over, so any proxy with a read timeout — 60 seconds by default in ingress-nginx — killed the
// call while the Job ran on to completion. What these tests carry is that the stages reach the wire
// as they happen, that one build is ONE sequence running into the deploy's stages, and that a caller
// that asked for nothing still gets exactly the single JSON object it always got.

// newBuildAPI is newAPI with a Builder wired, since the build path errors cleanly without one.
func newBuildAPI(t *testing.T) (http.Handler, *fake.Kubernetes, *fake.Database, *fake.Builder) {
	t.Helper()
	k, d, b := fake.NewKubernetes(), fake.NewDatabase(), fake.NewBuilder()
	d.SetPolicy(cp.Policy{}.With(cp.GuardrailAppDeploy, cp.DispositionAllow))
	e, err := cp.New(cp.Deps{
		Kubernetes: k, Database: d,
		Clock:       fake.NewClock(time.Date(2026, 6, 23, 12, 0, 0, 0, time.UTC)),
		IDs:         fake.NewIDs(),
		Resolver:    fake.NewResolver(),
		Credentials: fake.NewCredentials(),
		DNS:         fake.NewDNSFactory(),
		Builder:     b,
	})
	if err != nil {
		t.Fatalf("engine: %v", err)
	}
	h, err := api.New(api.Config{Engine: e, Token: token})
	if err != nil {
		t.Fatalf("api.New: %v", err)
	}
	return h, k, d, b
}

// buildBody is a valid build request: a git source and an explicit push target.
const buildBody = `{"source":{"Repo":"https://github.com/acme/web","Ref":"v1.0.0"},"target_image":"ghcr.io/acme/web:1.0.0"}`

// buildLine mirrors the three line shapes a build's stream may carry. It is the deploy stream's
// framing with the build's result in it — which is the point: one framing, two operations.
type buildLine struct {
	Event  *cp.DeployEvent `json:"event"`
	Result *cp.BuildResult `json:"result"`
	Error  *struct {
		Status            int    `json:"status"`
		Error             string `json:"error"`
		Code              string `json:"code"`
		NeedsConfirmation bool   `json:"needs_confirmation"`
	} `json:"error"`
}

// streamedBuild issues a build asking for the ndjson stream and returns the decoded lines.
func streamedBuild(t *testing.T, h http.Handler, app, body string) (*httptest.ResponseRecorder, []buildLine) {
	t.Helper()
	req := httptest.NewRequest("POST", "/v1/apps/"+app+"/build", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/x-ndjson")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if ct := rec.Header().Get("Content-Type"); ct != "application/x-ndjson" {
		return rec, nil
	}
	var lines []buildLine
	dec := json.NewDecoder(rec.Body)
	for dec.More() {
		var l buildLine
		if err := dec.Decode(&l); err != nil {
			t.Fatalf("decoding a stream line: %v", err)
		}
		lines = append(lines, l)
	}
	return rec, lines
}

// buildStageSeq renders the lines as "stage:status" so a sequence assertion reads as the build does.
func buildStageSeq(lines []buildLine) []string {
	var got []string
	for _, l := range lines {
		if l.Event != nil {
			got = append(got, l.Event.Stage+":"+l.Event.Status)
		}
	}
	return got
}

// TestBuildStreamsOneSequenceThroughToTheDeploy is the whole feature in one pass: the build's own
// stages run straight into the deploy stages of the deploy it hands off to, and the last line is the
// same BuildResult the plain call returns. Two disjoint streams would say the build and the deploy
// were separate operations; they are one call.
func TestBuildStreamsOneSequenceThroughToTheDeploy(t *testing.T) {
	h, k, _, _ := newBuildAPI(t)

	rec, lines := streamedBuild(t, h, "web", buildBody)
	if rec.Code != 200 {
		t.Fatalf("streamed build = %d %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q, want nosniff", got)
	}
	want := []string{"clone:started", "clone:done", "build:started", "build:done", "apply:started", "apply:done"}
	if got := buildStageSeq(lines); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("stages = %v, want %v", got, want)
	}
	last := lines[len(lines)-1]
	if last.Result == nil {
		t.Fatalf("the stream's last line is not the result: %+v", last)
	}
	if last.Result.Digest != fake.DefaultDigest {
		t.Errorf("result digest = %q, want %q", last.Result.Digest, fake.DefaultDigest)
	}
	if last.Result.Deploy.Release.ID == "" {
		t.Error("the result carries no release: a build ends in the deploy that shipped it")
	}
	if _, ok := k.Spec("web"); !ok {
		t.Error("the build did not deploy the image it built")
	}
}

// TestBuildStreamStartsBeforeTheBuildFinishes is the property the bug was about. The build is what
// takes minutes, so the header and the first stages must be flushed BEFORE it completes — a response
// that begins when the build ends is a response a proxy has already timed out.
func TestBuildStreamStartsBeforeTheBuildFinishes(t *testing.T) {
	h, _, _, _ := newBuildAPI(t)
	req := httptest.NewRequest("POST", "/v1/apps/web/build", strings.NewReader(buildBody))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/x-ndjson")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if !rec.Flushed {
		t.Error("the stream was never flushed; the whole response would arrive only when the build finished")
	}
	// The first line is a stage of the build itself, not the result: the caller hears about the build
	// while it runs.
	first := strings.SplitN(strings.TrimSpace(rec.Body.String()), "\n", 2)[0]
	if !strings.Contains(first, `"event"`) || !strings.Contains(first, cp.StageClone) {
		t.Errorf("first line = %s, want the build's first stage", first)
	}
}

// TestUnstreamedBuildIsUnchanged pins that a caller that does not ask for progress gets exactly the
// response this endpoint has always returned: one JSON object, application/json. It is what keeps an
// old client — and `burrow-agent`, which never asks — working byte for byte.
func TestUnstreamedBuildIsUnchanged(t *testing.T) {
	h, _, _, _ := newBuildAPI(t)
	rr := do(h, "POST", "/v1/apps/web/build", token, buildBody)
	if rr.Code != 200 {
		t.Fatalf("build = %d %s", rr.Code, rr.Body.String())
	}
	if ct := rr.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	var res cp.BuildResult
	if err := json.Unmarshal(rr.Body.Bytes(), &res); err != nil {
		t.Fatalf("decoding the plain response: %v", err)
	}
	if res.Digest != fake.DefaultDigest || res.Deploy.Release.ID == "" {
		t.Errorf("result = %+v", res)
	}
}

// TestABuildRefusedBeforeItStartsStaysStatusCoded is what the lazy header buys. Everything decided
// before the builder runs — a malformed source, an unknown environment, no target and no in-cluster
// registry — is an ordinary status-coded error even under an Accept header asking for a stream.
func TestABuildRefusedBeforeItStartsStaysStatusCoded(t *testing.T) {
	cases := []struct {
		name string
		body string
		want int
	}{
		{
			name: "a source with no ref",
			body: `{"source":{"Repo":"https://github.com/acme/web","Ref":""},"target_image":"ghcr.io/acme/web:1"}`,
			want: 400,
		},
		{
			name: "no target and no in-cluster registry to default to",
			body: `{"source":{"Repo":"https://github.com/acme/web","Ref":"v1"}}`,
			want: 400,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h, _, _, _ := newBuildAPI(t)
			rec, lines := streamedBuild(t, h, "web", c.body)
			if rec.Code != c.want {
				t.Fatalf("build = %d %s, want %d", rec.Code, rec.Body.String(), c.want)
			}
			if lines != nil {
				t.Errorf("a refusal produced stream lines: %v", lines)
			}
			if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
				t.Errorf("Content-Type = %q, want application/json", ct)
			}
		})
	}
}

// TestAFailedBuildArrivesAsAnErrorLine covers a failure raised after the first stage: the status line
// is spent, so the failure rides in the stream carrying the status and code it would otherwise have
// been written with, and the client rebuilds the identical error from it.
func TestAFailedBuildArrivesAsAnErrorLine(t *testing.T) {
	h, k, _, b := newBuildAPI(t)
	b.SetError(errors.New("step 7/12 returned a non-zero code"))

	rec, lines := streamedBuild(t, h, "web", buildBody)
	if rec.Code != 200 {
		t.Fatalf("streamed build with a failing builder = %d %s, want a 200 stream", rec.Code, rec.Body.String())
	}
	want := []string{"clone:started", "clone:done", "build:started", "build:failed"}
	if got := buildStageSeq(lines); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("stages = %v, want %v", got, want)
	}
	last := lines[len(lines)-1]
	if last.Error == nil {
		t.Fatalf("the stream's last line is not an error: %+v", last)
	}
	if last.Error.Status != 500 {
		t.Errorf("error status = %d, want the status the plain path returns", last.Error.Status)
	}
	if !strings.Contains(last.Error.Error, "step 7/12") {
		t.Errorf("error = %q, want the builder's own detail", last.Error.Error)
	}
	if _, ok := k.Spec("web"); ok {
		t.Error("a workload was applied: a failed build must not touch the deploy path")
	}
}

// TestAHeldBuildArrivesAsAnErrorLineCarryingTheHold is where a build differs from a deploy, and it
// has to. A deploy's guardrail is decided before any stage runs, so a hold is refused status-coded. A
// build's guardrail is the DEPLOY's, and the deploy comes after the build: by the time app.deploy is
// consulted the build has been running for minutes and its stages are on the wire. So the hold
// travels in the stream — with the same 422, the same code, and needs_confirmation set, which is what
// the client rebuilds an *APIError from and what `burrow-agent` classifies a hold by.
func TestAHeldBuildArrivesAsAnErrorLineCarryingTheHold(t *testing.T) {
	h, k, d, _ := newBuildAPI(t)
	d.SetPolicy(cp.Policy{}.With(cp.GuardrailAppDeploy, cp.DispositionConfirm))

	rec, lines := streamedBuild(t, h, "web", buildBody)
	if rec.Code != 200 {
		t.Fatalf("held build = %d %s, want a 200 stream: the build itself ran", rec.Code, rec.Body.String())
	}
	last := lines[len(lines)-1]
	if last.Error == nil {
		t.Fatalf("the stream's last line is not an error: %+v", last)
	}
	if last.Error.Status != 422 {
		t.Errorf("error status = %d, want the 422 a hold is refused with", last.Error.Status)
	}
	if !last.Error.NeedsConfirmation {
		t.Error("needs_confirmation is not set; the agent would classify the hold as a failure")
	}
	if last.Error.Code != string(cp.GuardrailAppDeploy) {
		t.Errorf("error code = %q, want %q", last.Error.Code, cp.GuardrailAppDeploy)
	}
	if _, ok := k.Spec("web"); ok {
		t.Error("a held build deployed a workload")
	}
}

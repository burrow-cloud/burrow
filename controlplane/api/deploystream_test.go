// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package api_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	cp "github.com/burrow-cloud/burrow/controlplane"
)

// The deploy progress stream (issue #480). Two properties carry these tests: what a caller that
// ASKED for progress receives, and — the load-bearing one — that a refusal raised before the deploy
// started doing anything is still an ordinary status-coded JSON error, because `burrow-agent`
// classifies a guardrail hold from the status and needs_confirmation.

// streamed issues a deploy asking for the ndjson stream and returns the decoded lines.
func streamed(t *testing.T, h http.Handler, app, body string) (*httptest.ResponseRecorder, []streamLine) {
	t.Helper()
	req := httptest.NewRequest("POST", "/v1/apps/"+app+"/deploy", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/x-ndjson")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if ct := rec.Header().Get("Content-Type"); ct != "application/x-ndjson" {
		return rec, nil
	}
	var lines []streamLine
	dec := json.NewDecoder(rec.Body)
	for dec.More() {
		var l streamLine
		if err := dec.Decode(&l); err != nil {
			t.Fatalf("decoding a stream line: %v", err)
		}
		lines = append(lines, l)
	}
	return rec, lines
}

// streamLine mirrors the three line shapes the stream may carry.
type streamLine struct {
	Event  *cp.DeployEvent  `json:"event"`
	Result *cp.DeployResult `json:"result"`
	Error  *struct {
		Status            int    `json:"status"`
		Error             string `json:"error"`
		Code              string `json:"code"`
		NeedsConfirmation bool   `json:"needs_confirmation"`
	} `json:"error"`
}

// stages renders the lines as "stage:status" so a sequence assertion reads as the deploy does.
func stages(lines []streamLine) []string {
	var got []string
	for _, l := range lines {
		if l.Event != nil {
			got = append(got, l.Event.Stage+":"+l.Event.Status)
		}
	}
	return got
}

// TestDeployStreamsItsStagesThenTheResult is the whole feature in one pass: a caller that asks for
// ndjson gets flushed stage events and, last, the same DeployResult the plain call returns.
func TestDeployStreamsItsStagesThenTheResult(t *testing.T) {
	h, _, _ := newAPI(t)

	rec, lines := streamed(t, h, "web", `{"image":"img:1","replicas":1}`)
	if rec.Code != 200 {
		t.Fatalf("streamed deploy = %d %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q, want nosniff", got)
	}
	// A bare app: nothing waits, so nothing but the apply is reported.
	want := []string{"apply:started", "apply:done"}
	got := stages(lines)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("stages = %v, want %v", got, want)
	}
	last := lines[len(lines)-1]
	if last.Result == nil {
		t.Fatalf("the stream's last line is not the result: %+v", last)
	}
	if last.Result.Release.Image != "img:1" {
		t.Errorf("result release image = %q, want img:1", last.Result.Release.Image)
	}
	for _, l := range lines {
		if l.Error != nil {
			t.Errorf("a successful deploy carried an error line: %+v", l.Error)
		}
	}
}

// TestDeployStreamEventsAreFlushedAsTheyHappen asserts the events reach the wire while the deploy is
// still running, rather than arriving in one buffered lump at the end. Without the flush the whole
// feature is decoration: the point is that the operator sees the stage they are waiting in.
func TestDeployStreamEventsAreFlushedAsTheyHappen(t *testing.T) {
	h, k, _ := newAPI(t)
	// A recorder counts flushes; the apply's started/done are two.
	req := httptest.NewRequest("POST", "/v1/apps/web/deploy", strings.NewReader(`{"image":"img:1","replicas":1}`))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/x-ndjson")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if !rec.Flushed {
		t.Error("the stream was never flushed; the events would arrive only when the deploy finished")
	}
	if _, ok := k.Spec("web"); !ok {
		t.Error("the deploy did not apply a workload")
	}
}

// TestUnstreamedDeployIsUnchanged pins that a caller that does not ask for progress gets exactly the
// response this endpoint has always returned: one JSON object, application/json. It is what keeps an
// old client — and `burrow-agent`, which never asks — working byte for byte.
func TestUnstreamedDeployIsUnchanged(t *testing.T) {
	h, _, _ := newAPI(t)
	rr := do(h, "POST", "/v1/apps/web/deploy", token, `{"image":"img:1","replicas":1}`)
	if rr.Code != 200 {
		t.Fatalf("deploy = %d %s", rr.Code, rr.Body.String())
	}
	if ct := rr.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	var res cp.DeployResult
	if err := json.Unmarshal(rr.Body.Bytes(), &res); err != nil {
		t.Fatalf("decoding the plain response: %v", err)
	}
	if res.Release.Image != "img:1" {
		t.Errorf("release image = %q, want img:1", res.Release.Image)
	}
}

// TestHeldDeployIsStatusCodedEvenWhenProgressWasAsked is the property the lazy header exists for. A
// guardrail hold is decided BEFORE any stage runs, so nothing has been written and the refusal is an
// ordinary 422 with needs_confirmation — the shape `burrow-agent`'s classify branches on. A hold
// delivered as a 200 stream would be relayed to the human as a failure.
func TestHeldDeployIsStatusCodedEvenWhenProgressWasAsked(t *testing.T) {
	h, k, d := newAPI(t)
	d.SetPolicy(cp.Policy{}.With(cp.GuardrailAppDeploy, cp.DispositionConfirm))

	rec, lines := streamed(t, h, "web", `{"image":"img:1","replicas":1}`)
	if rec.Code != 422 {
		t.Fatalf("held deploy = %d %s, want 422", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json — a hold is not a stream", ct)
	}
	if lines != nil {
		t.Fatalf("a held deploy produced stream lines: %v", lines)
	}
	var e errBody
	if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil {
		t.Fatalf("decoding the refusal: %v", err)
	}
	if !e.NeedsConfirmation {
		t.Error("needs_confirmation is not set; the agent would classify the hold as a failure")
	}
	if e.Code != string(cp.GuardrailAppDeploy) {
		t.Errorf("code = %q, want %q", e.Code, cp.GuardrailAppDeploy)
	}
	if _, ok := k.Spec("web"); ok {
		t.Error("a held deploy applied a workload")
	}
}

// TestDeniedAndInvalidDeploysStayStatusCoded covers the rest of what is decided before the first
// event: a denial and a validation failure are both ordinary status-coded errors under an Accept
// header asking for a stream.
func TestDeniedAndInvalidDeploysStayStatusCoded(t *testing.T) {
	cases := []struct {
		name   string
		policy cp.Policy
		body   string
		want   int
	}{
		{
			name:   "a guardrail denial",
			policy: cp.Policy{}.With(cp.GuardrailAppDeploy, cp.DispositionDeny),
			body:   `{"image":"img:1","replicas":1}`,
			want:   422,
		},
		{
			name:   "an empty image reference",
			policy: cp.Policy{}.With(cp.GuardrailAppDeploy, cp.DispositionAllow),
			body:   `{"image":"","replicas":1}`,
			want:   400,
		},
		{
			name:   "past the replica ceiling",
			policy: cp.Policy{}.With(cp.GuardrailAppDeploy, cp.DispositionAllow),
			body:   `{"image":"img:1","replicas":99}`,
			want:   422,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h, _, d := newAPI(t)
			d.SetPolicy(c.policy)
			rec, lines := streamed(t, h, "web", c.body)
			if rec.Code != c.want {
				t.Fatalf("deploy = %d %s, want %d", rec.Code, rec.Body.String(), c.want)
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

// TestFailureAfterTheFirstEventArrivesAsAnErrorLine is the other half of the lazy header: once a
// stage has been reported the status line is spent, so a later failure rides in the stream carrying
// the status and code it would otherwise have been written with. A failing `pre-deploy` hook is the
// case that reaches it — the hook stage is reported, then the hook aborts the deploy.
func TestFailureAfterTheFirstEventArrivesAsAnErrorLine(t *testing.T) {
	h, k, _ := newAPI(t)
	if rr := do(h, "PUT", "/v1/apps/web/hooks/pre-deploy", token, `{"command":["./migrate"]}`); rr.Code != 200 {
		t.Fatalf("hook set = %d %s", rr.Code, rr.Body.String())
	}
	k.SetRunResult(cp.RunResult{ExitCode: 1, Stdout: "migration 003 failed\n"})

	rec, lines := streamed(t, h, "web", `{"image":"img:1","replicas":1}`)
	// The transport succeeded — the stream started before the failure was known.
	if rec.Code != 200 {
		t.Fatalf("streamed deploy with a failing hook = %d %s, want a 200 stream", rec.Code, rec.Body.String())
	}
	want := []string{"pre-deploy-hook:started", "pre-deploy-hook:failed"}
	if got := stages(lines); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("stages = %v, want %v", got, want)
	}
	last := lines[len(lines)-1]
	if last.Error == nil {
		t.Fatalf("the stream's last line is not an error: %+v", last)
	}
	if last.Error.Status != 422 {
		t.Errorf("error status = %d, want the 422 the plain path returns", last.Error.Status)
	}
	if last.Error.Code != "hook_failed" {
		t.Errorf("error code = %q, want hook_failed", last.Error.Code)
	}
	if last.Error.NeedsConfirmation {
		t.Error("needs_confirmation is set on a failed hook; there is nothing to confirm")
	}
	if !strings.Contains(last.Error.Error, "migration 003 failed") {
		t.Errorf("error = %q, want the command's own output", last.Error.Error)
	}
	if _, ok := k.Spec("web"); ok {
		t.Error("a workload was applied: a failed pre-deploy hook must abort the deploy")
	}
}

// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/burrow-cloud/burrow/client"
	"github.com/burrow-cloud/burrow/controlplane"
)

// What `burrow app deploy` prints while it runs (issue #480). The two properties that matter: the
// stages read as the tick style the rest of the CLI already uses, and none of them lands on stdout,
// which is where the result — and, under --json, a document a caller parses — lives.

// ndjsonDeploy answers a deploy with the given already-rendered stream lines, and records whether the
// CLI asked for the stream.
func ndjsonDeploy(accept *string, lines ...string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if accept != nil {
			*accept = r.Header.Get("Accept")
		}
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.WriteHeader(http.StatusOK)
		for _, l := range lines {
			_, _ = io.WriteString(w, l+"\n")
			w.(http.Flusher).Flush()
		}
	}
}

// deployStreamLines is the stream a fully-featured app's deploy produces, ending in the result.
func deployStreamLines() []string {
	return []string{
		`{"event":{"stage":"pre-deploy-hook","status":"started"}}`,
		`{"event":{"stage":"pre-deploy-hook","status":"done"}}`,
		`{"event":{"stage":"apply","status":"started"}}`,
		`{"event":{"stage":"apply","status":"done"}}`,
		`{"event":{"stage":"settle","status":"started"}}`,
		`{"event":{"stage":"settle","status":"done"}}`,
		`{"event":{"stage":"dependency-check","status":"started"}}`,
		`{"event":{"stage":"dependency-check","status":"failed"}}`,
		`{"result":{"release":{"id":"r1","app":"web","image":"img:1","status":"deployed","replicas":1}}}`,
	}
}

// TestDeployPrintsItsStagesOnStderr is the whole rendered surface in one assertion, plus the
// stdout/stderr split the --json contract rests on.
func TestDeployPrintsItsStagesOnStderr(t *testing.T) {
	var accept string
	stdout, stderr, err := runCLI(t, ndjsonDeploy(&accept, deployStreamLines()...),
		"app", "deploy", "web", "--image", "img:1")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.Contains(accept, "x-ndjson") {
		t.Errorf("Accept = %q, want the CLI to ask for the progress stream", accept)
	}
	for _, want := range []string{
		"running the pre-deploy hook",
		"writing the Deployment",
		"waiting for the rollout to settle",
		"checking the dependencies Burrow provisioned",
	} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr = %q, want it to name the stage %q", stderr, want)
		}
		if strings.Contains(stdout, want) {
			t.Errorf("stdout carried the stage %q; progress belongs on stderr", want)
		}
	}
	if !strings.Contains(stderr, okGlyph) || !strings.Contains(stderr, failGlyph) {
		t.Errorf("stderr = %q, want a ✓ on the stages that finished and a ✗ on the one that did not", stderr)
	}
	// Off a terminal the marks stay plain: nothing escaped leaks into captured output.
	if strings.Contains(stderr, "\033[") {
		t.Errorf("stderr carries escape codes off a terminal: %q", stderr)
	}
	if !strings.Contains(stdout, "deployed web as release r1") {
		t.Errorf("stdout = %q, want the result line", stdout)
	}
}

// TestDeployJSONStdoutCarriesOnlyTheDocument is the contract --json makes: stdout is a single JSON
// document a caller parses, whatever the command narrated along the way.
func TestDeployJSONStdoutCarriesOnlyTheDocument(t *testing.T) {
	stdout, stderr, err := runCLI(t, ndjsonDeploy(nil, deployStreamLines()...),
		"app", "deploy", "web", "--image", "img:1", "--json")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	var res client.DeployResult
	if err := json.Unmarshal([]byte(stdout), &res); err != nil {
		t.Fatalf("stdout is not one JSON document (%v): %q", err, stdout)
	}
	if res.Release.ID != "r1" {
		t.Errorf("release = %+v", res.Release)
	}
	if !strings.Contains(stderr, "writing the Deployment") {
		t.Errorf("stderr = %q, want the stages still reported under --json", stderr)
	}
}

// TestDeployAgainstAControlPlaneWithNoStream keeps a new CLI working against an older control plane:
// it answers plain JSON, the CLI prints no stages, and the deploy still reports its result.
func TestDeployAgainstAControlPlaneWithNoStream(t *testing.T) {
	stdout, stderr, err := runCLI(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"release": map[string]any{"id": "r1", "app": "web", "image": "img:1", "status": "deployed", "replicas": 1},
		})
	}, "app", "deploy", "web", "--image", "img:1")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if strings.Contains(stderr, "writing the Deployment") {
		t.Errorf("stderr = %q, want no stages from a control plane that reported none", stderr)
	}
	if !strings.Contains(stdout, "deployed web as release r1") {
		t.Errorf("stdout = %q, want the result line", stdout)
	}
}

// TestDeployHoldIsStillTheConfirmError pins that the confirm flow is untouched by the stream: the
// hold arrives status-coded before any stage, so the CLI reports it as the error it always did.
func TestDeployHoldIsStillTheConfirmError(t *testing.T) {
	_, stderr, err := runCLI(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = io.WriteString(w, `{"error":"app.deploy is held for confirmation","code":"app.deploy","needs_confirmation":true}`)
	}, "app", "deploy", "web", "--image", "img:1")
	if err == nil {
		t.Fatal("a held deploy succeeded")
	}
	if !strings.Contains(err.Error(), "--confirm") {
		t.Errorf("err = %v, want the confirm remedy", err)
	}
	if strings.Contains(stderr, "writing the Deployment") {
		t.Errorf("stderr = %q, want no stages before the guardrail decided", stderr)
	}
}

// TestDeployStageLabelsCoverTheControlPlanesVocabulary is the guard against the two halves drifting:
// a stage the control plane can report and this CLI has no words for would print a raw token.
func TestDeployStageLabelsCoverTheControlPlanesVocabulary(t *testing.T) {
	for _, stage := range controlplane.DeployStages() {
		if _, ok := deployStageLabels[stage]; !ok {
			t.Errorf("no human label for the stage %q", stage)
		}
	}
}

// TestAnUnknownStageRendersHarmlessly is the forward-compatibility case: a control plane newer than
// this binary reports a stage added after it was built. It must read as English rather than as a
// raw token that looks like a bug — and the line must still close.
func TestAnUnknownStageRendersHarmlessly(t *testing.T) {
	var buf bytes.Buffer
	clock := stepClock(2 * time.Second)
	p := newDeployProgressPrinter(&buf, clock)
	p(client.DeployProgress{Stage: "warming-the-cache", Status: "started"})
	p(client.DeployProgress{Stage: "warming-the-cache", Status: "done"})
	got := buf.String()
	if !strings.Contains(got, "warming the cache") {
		t.Errorf("output = %q, want the stage rendered as words", got)
	}
	if strings.Contains(got, "warming-the-cache") {
		t.Errorf("output = %q, want no raw stage token", got)
	}
	if !strings.Contains(got, okGlyph) {
		t.Errorf("output = %q, want the line closed with a mark", got)
	}
}

// TestUnknownStatusIsIgnored pins the other direction of the same forward compatibility: a status
// this binary cannot place is neither the start nor the end of a line, so it prints nothing.
func TestUnknownStatusIsIgnored(t *testing.T) {
	var buf bytes.Buffer
	p := newDeployProgressPrinter(&buf, stepClock(time.Second))
	p(client.DeployProgress{Stage: "apply", Status: "retrying"})
	if buf.String() != "" {
		t.Errorf("output = %q, want nothing for a status this binary does not know", buf.String())
	}
}

// TestStageElapsedIsAppended is the point of the line: the duration is what tells a reader that a
// slow deploy was a slow image pull rather than a stuck command.
func TestStageElapsedIsAppended(t *testing.T) {
	var buf bytes.Buffer
	p := newDeployProgressPrinter(&buf, stepClock(12*time.Second))
	p(client.DeployProgress{Stage: "settle", Status: "started"})
	p(client.DeployProgress{Stage: "settle", Status: "done"})
	if !strings.Contains(buf.String(), "(12s)") {
		t.Errorf("output = %q, want the elapsed time", buf.String())
	}
}

// stepClock advances by step on every reading, so a test reads a fixed elapsed time rather than a
// real one.
func stepClock(step time.Duration) func() time.Time {
	base := time.Date(2026, 6, 23, 12, 0, 0, 0, time.UTC)
	n := 0
	return func() time.Time {
		t := base.Add(time.Duration(n) * step)
		n++
		return t
	}
}

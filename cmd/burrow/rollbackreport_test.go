// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// What `burrow app rollback` prints is issue #548 as a person meets it: one sentence saying the app
// was rolled back and the release it replaced superseded, printed whether or not the older image ever
// served. These tests pin the sentence to the outcome, and pin the two things a failed rollback has to
// say that a failed deploy does not.

// rollbackResponse writes a rollback result with the given rollout report attached. r2 is the release
// being rolled back away from — the one still serving when the rollout does not come up.
func rollbackResponse(rollout map[string]any) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, _ *http.Request) {
		body := map[string]any{
			"release":                   map[string]any{"id": "r3", "app": "web", "image": "img:1", "status": "deployed", "replicas": 1},
			"rolled_back_to_release_id": "r1",
			"superseded_release_id":     "r2",
		}
		if rollout != nil {
			body["rollout"] = rollout
		}
		_ = json.NewEncoder(w).Encode(body)
	}
}

// TestRollbackWithAWedgedRolloutSaysSoAndFails is the defect. The output must not say the app was
// rolled back or that r2 was superseded — r2 is the release the operator was fleeing and the one
// Kubernetes is still running — must name the pod's reason, and must exit non-zero.
func TestRollbackWithAWedgedRolloutSaysSoAndFails(t *testing.T) {
	out, _, err := runCLI(t, rollbackResponse(map[string]any{
		"settled": false,
		"reason":  "CrashLoopBackOff",
		"detail":  "0 of 1 replicas updated, 0 ready",
		"issue":   "container \"web\" is crash-looping (exit 1): migration 0041 is already applied",
	}), "app", "rollback", "web")

	if err == nil {
		t.Fatal("rollback returned nil: a rollout that never became ready must exit non-zero")
	}
	if !errors.Is(err, errRolloutNotReady) {
		t.Errorf("error = %v, want one that unwraps to errRolloutNotReady", err)
	}
	for _, forbidden := range []string{"superseded release", "rolled web back"} {
		if strings.Contains(out, forbidden) {
			t.Errorf("output claims %q for a rollback whose image never became ready:\n%s", forbidden, out)
		}
	}
	for _, want := range []string{"CrashLoopBackOff", "migration 0041 is already applied", "burrow app status web"} {
		if !strings.Contains(out, want) {
			t.Errorf("output does not mention %q:\n%s", want, out)
		}
	}
}

// TestAFailedRollbackNamesWhatIsStillServingAndTheWayOut is what this report has that the deploy's
// does not. The operator is mid-incident: they need to know that the release they were running away
// from is the one still up, and that reaching for rollback again — the obvious next move — returns to
// exactly that release rather than going further back.
func TestAFailedRollbackNamesWhatIsStillServingAndTheWayOut(t *testing.T) {
	out, _, err := runCLI(t, rollbackResponse(map[string]any{
		"settled": false,
		"reason":  "ImagePullBackOff",
	}), "app", "rollback", "web")

	if err == nil {
		t.Fatal("rollback returned nil")
	}
	if !strings.Contains(out, "r2") || !strings.Contains(out, "may still be serving") {
		t.Errorf("output does not say the release being rolled away from may still be serving:\n%s", out)
	}
	if !strings.Contains(out, "rolling back again returns to release r2") {
		t.Errorf("output does not warn that a second rollback returns to the release just left:\n%s", out)
	}
	if !strings.Contains(out, "burrow app history web") {
		t.Errorf("output does not name the way out — choosing a release deliberately:\n%s", out)
	}
}

// TestRollbackPastTheDeadlineSaysItIsStillRollingOut separates the two negative outcomes, as the
// deploy's report does: an expired bound with nothing blocking reported is a rollout still going, not
// one that will never finish.
func TestRollbackPastTheDeadlineSaysItIsStillRollingOut(t *testing.T) {
	out, _, err := runCLI(t, rollbackResponse(map[string]any{
		"settled": false,
		"reason":  "DeadlineExceeded",
		"detail":  `waited 5m0s; pod "web-abc" is Running but not ready`,
	}), "app", "rollback", "web")

	if err == nil {
		t.Fatal("rollback returned nil: a rollout that had not become ready must exit non-zero")
	}
	for _, want := range []string{"had not become ready when Burrow stopped waiting", "Running but not ready", "still rolling it out", "deploy.settle_timeout"} {
		if !strings.Contains(out, want) {
			t.Errorf("output does not mention %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "is not becoming ready") {
		t.Errorf("an expired bound is reported as a rollout that will never finish:\n%s", out)
	}
}

// TestRollbackThatSettledKeepsItsLine is the ordinary case, unchanged. The recovery worked, so the
// sentence rollback has always printed is the right one — now printed only when it is true.
func TestRollbackThatSettledKeepsItsLine(t *testing.T) {
	out, _, err := runCLI(t, rollbackResponse(map[string]any{"settled": true}), "app", "rollback", "web")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	for _, want := range []string{"rolled web back to release r1", "superseded release r2"} {
		if !strings.Contains(out, want) {
			t.Errorf("output does not contain %q:\n%s", want, out)
		}
	}
}

// TestRollbackWithoutWaitingReportsAnUnknownOutcome is what --wait=false must read as, and the wire
// shape it must take: the opt-out is sent only when asked for, so a control plane that has never
// heard of it waits.
func TestRollbackWithoutWaitingReportsAnUnknownOutcome(t *testing.T) {
	var got url.Values
	out, _, err := runCLI(t, func(w http.ResponseWriter, r *http.Request) {
		got = r.URL.Query()
		rollbackResponse(nil)(w, r)
	}, "app", "rollback", "web", "--wait=false")

	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if got.Get("no_wait") != "true" {
		t.Errorf("request query no_wait = %q, want true", got.Get("no_wait"))
	}
	if !strings.Contains(out, "not waited for") || !strings.Contains(out, "unknown") {
		t.Errorf("output does not say the outcome is unknown:\n%s", out)
	}
	if strings.Contains(out, "superseded release") {
		t.Errorf("output claims the previous release was superseded without having looked:\n%s", out)
	}
}

// TestRollbackByDefaultAsksTheControlPlaneToWait pins the default at the wire.
func TestRollbackByDefaultAsksTheControlPlaneToWait(t *testing.T) {
	var got url.Values
	if _, _, err := runCLI(t, func(w http.ResponseWriter, r *http.Request) {
		got = r.URL.Query()
		rollbackResponse(map[string]any{"settled": true})(w, r)
	}, "app", "rollback", "web"); err != nil {
		t.Fatalf("run: %v", err)
	}
	if got.Has("no_wait") {
		t.Errorf("request carries no_wait = %q; waiting is the default and is not opted into", got.Get("no_wait"))
	}
}

// TestRollbackJSONCarriesTheRolloutStructurally is the agent's side of the same report.
func TestRollbackJSONCarriesTheRolloutStructurally(t *testing.T) {
	out, _, err := runCLI(t, rollbackResponse(map[string]any{
		"settled": false,
		"reason":  "ImagePullBackOff",
	}), "app", "rollback", "web", "--json")

	if err == nil {
		t.Fatal("rollback returned nil with --json: the exit code must carry the same verdict as the prose")
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("stdout is not the JSON result (%v): %s", err, out)
	}
	rollout, ok := got["rollout"].(map[string]any)
	if !ok {
		t.Fatalf("result carries no rollout: %s", out)
	}
	if rollout["settled"] != false || rollout["reason"] != "ImagePullBackOff" {
		t.Errorf("rollout = %+v, want the unsettled observation", rollout)
	}
}

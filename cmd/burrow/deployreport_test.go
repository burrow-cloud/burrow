// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
)

// What `burrow app deploy` prints is the part of issue #546 a person actually met: one sentence
// saying a release was deployed and the previous one superseded, printed whether or not a single new
// pod was serving. These tests pin the sentence to the outcome.

// deployResponse writes a deploy result with the given rollout report attached.
func deployResponse(rollout map[string]any) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, _ *http.Request) {
		body := map[string]any{
			"release":               map[string]any{"id": "r1", "app": "web", "image": "img:2", "status": "deployed", "replicas": 1},
			"superseded_release_id": "r0",
		}
		if rollout != nil {
			body["rollout"] = rollout
		}
		_ = json.NewEncoder(w).Encode(body)
	}
}

// TestDeployWithAWedgedRolloutSaysSoAndFails is the defect, at the surface it was reported from. The
// output must not claim the release is deployed or that the previous one is superseded — "superseded"
// is the word that stops a reader looking at the old pod — must name the pod's reason, and must exit
// non-zero so an agent or a script branching on the result is not told this worked.
func TestDeployWithAWedgedRolloutSaysSoAndFails(t *testing.T) {
	out, _, err := runCLI(t, deployResponse(map[string]any{
		"settled": false,
		"reason":  "CrashLoopBackOff",
		"detail":  "0 of 1 replicas updated, 0 ready",
		"issue":   "container \"web\" is crash-looping (exit 1): listing tenants is forbidden",
	}), "app", "deploy", "web", "--image", "img:2")

	if err == nil {
		t.Fatal("deploy returned nil: a rollout that never became ready must exit non-zero")
	}
	if !errors.Is(err, errRolloutNotReady) {
		t.Errorf("error = %v, want one that unwraps to errRolloutNotReady", err)
	}
	for _, forbidden := range []string{"superseded release", "deployed web as release"} {
		if strings.Contains(out, forbidden) {
			t.Errorf("output claims %q for a rollout that never became ready:\n%s", forbidden, out)
		}
	}
	for _, want := range []string{"CrashLoopBackOff", "listing tenants is forbidden", "may still be serving", "burrow app status web"} {
		if !strings.Contains(out, want) {
			t.Errorf("output does not mention %q:\n%s", want, out)
		}
	}
}

// TestDeployPastTheDeadlineSaysItIsStillRollingOut separates the two negative outcomes. A bound that
// ran out with nothing blocking reported is a rollout still in progress — a large image pull
// legitimately is — so the report says that, names the setting that raises the bound, and does not
// tell the reader the deploy will never finish.
func TestDeployPastTheDeadlineSaysItIsStillRollingOut(t *testing.T) {
	out, _, err := runCLI(t, deployResponse(map[string]any{
		"settled": false,
		"reason":  "DeadlineExceeded",
		"detail":  `waited 5m0s; pod "web-abc" is Running but not ready`,
	}), "app", "deploy", "web", "--image", "img:2")

	if err == nil {
		t.Fatal("deploy returned nil: a rollout that had not become ready must exit non-zero")
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

// TestDeployWithoutWaitingReportsAnUnknownOutcome is what --wait=false must read as. The deploy
// happened and the report says so; what it must not do is fall back to the old sentence, which
// asserted an outcome nobody observed.
func TestDeployWithoutWaitingReportsAnUnknownOutcome(t *testing.T) {
	var gotBody map[string]any
	out, _, err := runCLI(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		deployResponse(nil)(w, r)
	}, "app", "deploy", "web", "--image", "img:2", "--wait=false")

	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if gotBody["no_wait"] != true {
		t.Errorf("request body no_wait = %v, want true", gotBody["no_wait"])
	}
	if !strings.Contains(out, "not waited for") || !strings.Contains(out, "unknown") {
		t.Errorf("output does not say the outcome is unknown:\n%s", out)
	}
	if strings.Contains(out, "superseded release") {
		t.Errorf("output claims the previous release was superseded without having looked:\n%s", out)
	}
}

// TestDeployByDefaultAsksTheControlPlaneToWait pins the default at the wire, where it matters: the
// request carries no opt-out, so a control plane that has never heard of the flag still waits.
func TestDeployByDefaultAsksTheControlPlaneToWait(t *testing.T) {
	var gotBody map[string]any
	if _, _, err := runCLI(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		deployResponse(map[string]any{"settled": true})(w, r)
	}, "app", "deploy", "web", "--image", "img:2"); err != nil {
		t.Fatalf("run: %v", err)
	}
	if _, present := gotBody["no_wait"]; present {
		t.Errorf("request body carries no_wait = %v; waiting is the default and is not opted into", gotBody["no_wait"])
	}
}

// TestDeployJSONCarriesTheRolloutStructurally is the agent's side of the same report: --json keeps
// its contract on stdout, the rollout is in it, and the process still exits non-zero.
func TestDeployJSONCarriesTheRolloutStructurally(t *testing.T) {
	out, _, err := runCLI(t, deployResponse(map[string]any{
		"settled": false,
		"reason":  "ImagePullBackOff",
	}), "app", "deploy", "web", "--image", "img:2", "--json")

	if err == nil {
		t.Fatal("deploy returned nil with --json: the exit code must carry the same verdict as the prose")
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

// TestHistoryQualifiesAReleaseWhoseRolloutNeverCameUp is the same honesty in the record's own
// surface. `deployed` records that Burrow applied the release, and a history that shows nothing else
// invites the reading that its pods came up — so a row whose rollout did not settle carries the
// reason it did not, while an ordinary row is left exactly as it was.
func TestHistoryQualifiesAReleaseWhoseRolloutNeverCameUp(t *testing.T) {
	out, _, err := runCLI(t, func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"releases": []map[string]any{
				{"id": "r2", "app": "web", "image": "img:2", "status": "deployed", "trigger": "manual",
					"rollout": "unsettled", "rollout_reason": "CrashLoopBackOff", "created_at": "2026-06-23T12:00:01Z"},
				{"id": "r1", "app": "web", "image": "img:1", "status": "superseded", "trigger": "manual",
					"rollout": "settled", "created_at": "2026-06-23T12:00:00Z"},
			},
		})
	}, "app", "history", "web")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.Contains(out, "deployed (not ready: CrashLoopBackOff)") {
		t.Errorf("history does not qualify the release whose rollout never came up:\n%s", out)
	}
	// A settled row says nothing extra: that is the ordinary case, and a column of confirmations is
	// noise that would make the qualified row harder to spot rather than easier.
	if strings.Contains(out, "superseded (") {
		t.Errorf("history annotates a healthy row:\n%s", out)
	}
}

// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package main

import (
	"errors"
	"fmt"
	"strings"

	"github.com/burrow-cloud/burrow/client"
	"github.com/burrow-cloud/burrow/controlplane"
)

// What `burrow app deploy` says when it is finished, and what it exits with (ADR-0092 §2).
//
// It used to say one sentence for every outcome: "deployed <app> as release <id> ...; superseded
// release <id>", exit 0, whether or not a single new pod was serving. On the deploy that produced
// issue #546 that sentence was wrong in both halves — the new pod never passed its readiness probe
// and the release reported as superseded was the one answering every request. "Superseded" is the
// damaging half, because it is the word that stops a reader going to look at the old pod.
//
// So the deploy now reports the ROLLOUT, and the shape of the line follows the outcome rather than
// the submission:
//
//   - the rollout settled — the sentence it has always printed, which is now true when it is printed
//   - the rollout did not settle — a report that never uses "deployed" or "superseded", names the
//     reason and what Burrow saw, says plainly which image is still serving, and exits non-zero
//   - the rollout was not observed — the deploy was asked not to wait, or the control plane is older
//     than the field; the line says the outcome is unknown, which is neither of the above
//
// The exit code is part of the report. A script or an agent that branches on it must not be told a
// wedged rollout succeeded, and `--json` carries the same fact structurally in `rollout`.

// deployHuman renders a finished deploy for a person: the release line, and — when the rollout did
// not settle or was never observed — what actually happened to it.
func deployHuman(app string, res client.DeployResult) string {
	rel := res.Release
	head := fmt.Sprintf("deployed %s as release %s (image %s, %d replica(s), %s)",
		app, rel.ID, rel.Image, rel.Replicas, rel.Status)

	switch {
	case res.Rollout == nil:
		// Nothing was observed, so nothing is claimed. The release line still names what was applied —
		// that part did happen — and the note says the part that is unknown, rather than leaving the
		// reader to assume the old meaning of the sentence.
		if res.SupersededReleaseID != "" {
			head += fmt.Sprintf("; it replaces release %s", res.SupersededReleaseID)
		}
		return head + "\n\n" + "The rollout was not waited for, so whether the new pods are serving is unknown.\n" +
			"  check it with: burrow app status " + app
	case res.Rollout.Settled:
		if res.SupersededReleaseID != "" {
			head += fmt.Sprintf("; superseded release %s", res.SupersededReleaseID)
		}
		return head
	default:
		return rolloutFailureHuman(app, res)
	}
}

// rolloutFailureHuman is the report for a rollout that did not become ready. It deliberately shares
// no wording with the success line: a reader skimming for "deployed ... superseded" must not find a
// near-match here.
//
// It distinguishes the two negative outcomes, because the reader's next action differs. The BACKSTOP
// reason (ReasonDeadlineExceeded) means the bound ran out with nothing blocking reported — a rollout
// that is still going, which a large image pull legitimately is — so it names the bound's setting
// and says the rollout continues. Every other reason is a pod reporting a blocking condition, which
// is a rollout that will not finish on its own.
func rolloutFailureHuman(app string, res client.DeployResult) string {
	rel, out := res.Release, res.Rollout
	var b strings.Builder
	if out.Reason == controlplane.ReasonDeadlineExceeded {
		fmt.Fprintf(&b, "applied %s release %s (image %s, %d replica(s)), but the rollout had not become ready when Burrow stopped waiting.\n",
			app, rel.ID, rel.Image, rel.Replicas)
	} else {
		fmt.Fprintf(&b, "applied %s release %s (image %s, %d replica(s)), but the rollout is not becoming ready: %s.\n",
			app, rel.ID, rel.Image, rel.Replicas, out.Reason)
	}
	if out.Detail != "" {
		fmt.Fprintf(&b, "  what Burrow saw: %s\n", out.Detail)
	}
	// The pod's own reason, in the words `burrow app status` uses for it. This is the line that
	// explains the failure rather than reporting it, so it goes last, where a reader stops.
	if out.Issue != "" {
		fmt.Fprintf(&b, "  %s\n", indentContinuation(out.Issue))
	}
	// Which image is serving. Kubernetes does not take the old ReplicaSet down until the new pods are
	// ready, so the previous release is still answering — wholly, at one replica, and partly above
	// that. Saying "may still be serving" is the honest form of that: Burrow observed the rollout,
	// not the traffic.
	if res.SupersededReleaseID != "" {
		fmt.Fprintf(&b, "  release %s (the one this replaces) may still be serving; nothing was rolled back.\n", res.SupersededReleaseID)
	}
	if out.Reason == controlplane.ReasonDeadlineExceeded {
		b.WriteString("  Kubernetes is still rolling it out. Raise the bound with `burrow cluster config set deploy.settle_timeout <duration>` if this app legitimately takes longer.\n")
	}
	fmt.Fprintf(&b, "  next: burrow app status %s, burrow app logs %s", app, app)
	return b.String()
}

// indentContinuation keeps a multi-line Issue inside the indented block it is printed in, so a
// message that wraps onto its own lines does not read as a new top-level statement.
func indentContinuation(s string) string {
	return strings.ReplaceAll(strings.TrimRight(s, "\n"), "\n", "\n  ")
}

// rolloutExitError is the error a deploy — or a rollback (ADR-0093 §2) — returns when its rollout did
// not settle: the CLI has already printed (or emitted as JSON) the full report, so this is one short
// line for main's "burrow: ..." prefix and, above all, a non-zero exit code. Nil for every other
// outcome, including an unobserved rollout — declining to wait is a choice the caller made, not a
// failure.
func rolloutExitError(app string, out *client.RolloutReport) error {
	if !out.Failed() {
		return nil
	}
	return fmt.Errorf("%s: %w (%s)", app, errRolloutNotReady, out.Reason)
}

// errRolloutNotReady is what a failed rollout's error unwraps to, so a test — and any caller
// composing these commands — can tell "the deploy could not be made" from "the deploy was made and
// the rollout did not come up". A rollback whose restored image did not come up is the same fact
// about a different operation and unwraps to the same sentinel.
var errRolloutNotReady = errors.New("rollout did not become ready")

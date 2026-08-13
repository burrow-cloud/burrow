// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package main

import (
	"fmt"
	"strings"

	"github.com/burrow-cloud/burrow/client"
	"github.com/burrow-cloud/burrow/controlplane"
)

// What `burrow app rollback` says when it is finished, and what it exits with (ADR-0093 §2).
//
// It is the deploy report's sibling and deliberately not the same file: the sentences differ where
// the situation differs, and the situation differs a lot. A deploy that does not come up leaves the
// KNOWN-GOOD version serving. A rollback that does not come up leaves the release the operator was
// running away from serving, because Kubernetes keeps the previous pods up until the restored image
// is ready — and the line the CLI used to print named that release as `superseded`, which is the
// word that stops somebody looking at it.
//
// Three outcomes, three shapes, as for a deploy:
//
//   - the rollout settled — the sentence rollback has always printed, now checked before it is said
//   - the rollout did not settle — a report that uses neither "rolled back" nor "superseded", names
//     the reason and the pod's own explanation, says which release is still serving, says what the
//     way out is, and exits non-zero
//   - the rollout was not observed — `--wait=false`, or a control plane older than the field; the
//     outcome is unknown, which is neither of the other two

// rollbackHuman renders a finished rollback for a person: the result line, and — when the rollout did
// not settle or was never observed — what actually happened to it.
//
// managed is the kind of target this rollback went to, and reaches the settle-timeout line only, as
// it does in deployHuman: the same bound, so the same sentence (targethints.go).
func rollbackHuman(app string, res client.RollbackResult, managed bool) string {
	rel := res.Release
	head := fmt.Sprintf("rolled %s back to release %s (image %s) as release %s",
		app, res.RolledBackToReleaseID, rel.Image, rel.ID)

	switch {
	case res.Rollout == nil:
		// Nothing was observed, so nothing is claimed. The line still names what was applied — that
		// part did happen — and says which part is unknown, rather than leaving the reader with the
		// old sentence's implication that the app is back on the older image and serving.
		if res.SupersededReleaseID != "" {
			head += fmt.Sprintf("; it replaces release %s", res.SupersededReleaseID)
		}
		return head + "\n\n" + "The rollout was not waited for, so whether the restored image is serving is unknown.\n" +
			"  check it with: burrow app status " + app
	case res.Rollout.Settled:
		if res.SupersededReleaseID != "" {
			head += fmt.Sprintf("; superseded release %s", res.SupersededReleaseID)
		}
		return head
	default:
		return rollbackFailureHuman(app, res, managed)
	}
}

// rollbackFailureHuman is the report for a rollback whose restored image did not become ready. It
// shares no wording with the success line — a reader skimming for "rolled ... back" must not find a
// near-match here — and it carries the two facts that belong to a rollback and to nothing else.
//
// THE FIRST IS WHICH RELEASE IS STILL SERVING. It is the one being rolled back away from, and on the
// path this exists for it is broken; the operator believes they have left it. Saying "may still be
// serving" is the honest form: Kubernetes keeps the previous pods up until the restored image is
// ready, wholly at one replica and partly above that, and Burrow observed the rollout rather than
// the traffic.
//
// THE SECOND IS THAT ANOTHER ROLLBACK IS NOT THE WAY OUT, which is the operator's next instinct.
// Rollback walks back from the newest `deployed` release and re-applies what THAT one supersedes;
// this failed rollback is now that release, and what it supersedes is the release just fled. So a
// second rollback returns to exactly where this one started. The escape is a deploy of a release
// chosen deliberately, which is what `burrow app history` is for.
func rollbackFailureHuman(app string, res client.RollbackResult, managed bool) string {
	rel, out := res.Release, res.Rollout
	var b strings.Builder
	if out.Reason == controlplane.ReasonDeadlineExceeded {
		fmt.Fprintf(&b, "applied %s release %s to return to release %s (image %s), but the rollout had not become ready when Burrow stopped waiting.\n",
			app, rel.ID, res.RolledBackToReleaseID, rel.Image)
	} else {
		fmt.Fprintf(&b, "applied %s release %s to return to release %s (image %s), but the rollout is not becoming ready: %s.\n",
			app, rel.ID, res.RolledBackToReleaseID, rel.Image, out.Reason)
	}
	if out.Detail != "" {
		fmt.Fprintf(&b, "  what Burrow saw: %s\n", out.Detail)
	}
	// The pod's own reason, in the words `burrow app status` uses for it — the line that explains the
	// failure rather than reporting it.
	if out.Issue != "" {
		fmt.Fprintf(&b, "  %s\n", indentContinuation(out.Issue))
	}
	if res.SupersededReleaseID != "" {
		fmt.Fprintf(&b, "  release %s — the one this rollback was moving away from — may still be serving; it has not been replaced.\n",
			res.SupersededReleaseID)
		fmt.Fprintf(&b, "  rolling back again returns to release %s, not further back. To leave it, pick a release with `burrow app history %s` and deploy its image.\n",
			res.SupersededReleaseID, app)
	}
	if out.Reason == controlplane.ReasonDeadlineExceeded {
		b.WriteString("  " + settleTimeoutHint(managed))
	}
	fmt.Fprintf(&b, "  next: burrow app status %s, burrow app logs %s", app, app)
	return b.String()
}

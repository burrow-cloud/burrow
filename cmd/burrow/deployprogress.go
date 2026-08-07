// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package main

import (
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/burrow-cloud/burrow/client"
	"github.com/burrow-cloud/burrow/controlplane"
)

// A deploy takes ten to twenty seconds and used to say nothing across all of it, so a slow image
// pull and a hung command looked identical (issue #480). A build takes minutes and had the same
// silence (issue #503). The control plane now reports the stages of both — a build's are its own
// followed by the deploy's, as one sequence — and this renders them, the same way for either.
//
// IT PRINTS TO STDERR, NEVER STDOUT. `--json` is a contract — a caller pipes stdout into a parser —
// and the targeting line already goes to stderr for exactly this reason. Progress is commentary on a
// command in flight, not its result.
//
// IT MATCHES THE TICK STYLE ALREADY IN THE TREE. `burrow cluster upgrade` and
// `burrow cluster postgres install` print "  label ..." and close the line with a ✓ or a ✗, through
// the same okMark/failMark helpers, so the glyphs stay TTY-gated and captured output stays free of
// escape codes. A deploy is run more often than either and had no reason to look different.

// deployStageLabels is what each stage of a deploy or a build is called in front of a person. The
// stage names themselves are the control plane's closed vocabulary, meant for a program; these say
// what the operation is actually doing. A build reports its own stages and then the deploy's as one
// sequence (issue #503), so one table covers both.
var deployStageLabels = map[string]string{
	controlplane.StageClone:           "cloning the source in the cluster",
	controlplane.StageBuild:           "building and pushing the image",
	controlplane.StageApply:           "writing the Deployment",
	controlplane.StagePreDeployHook:   "running the pre-deploy hook",
	controlplane.StageSettle:          "waiting for the rollout to settle",
	controlplane.StageDependencyCheck: "checking the dependencies Burrow provisioned",
	controlplane.StagePostDeployHook:  "running the post-deploy hook",
}

// deployStageLabel renders a stage for a person.
//
// A STAGE THIS BINARY DOES NOT KNOW STILL READS AS ENGLISH. A control plane newer than the CLI may
// report a stage added after this binary was built; printing the raw token would read as a bug, and
// refusing it would drop a line whose start has already been printed. Turning the hyphens into
// spaces turns a stage name into the phrase it was named after, which is the whole reason the
// vocabulary is written in words.
func deployStageLabel(stage string) string {
	if label, ok := deployStageLabels[stage]; ok {
		return label
	}
	return strings.ReplaceAll(stage, "-", " ")
}

// newDeployProgressPrinter builds the reporter a deploy hands to the client. now is the clock the
// elapsed time is measured on, injected so a test reads a fixed duration rather than a real one.
//
// THE ELAPSED TIME IS MEASURED HERE, client-side, and it is the point of the whole line: it is what
// tells a reader that a slow deploy was a slow image pull rather than a stuck command. The control
// plane deliberately reports no duration — one clock is enough, and it is the clock of the process
// the person is waiting on.
func newDeployProgressPrinter(out io.Writer, now func() time.Time) func(client.DeployProgress) {
	var startedAt time.Time
	open := false
	return func(p client.DeployProgress) {
		switch p.Status {
		case controlplane.DeployStarted:
			// A stage that starts while another is open would leave a half-written line; close it
			// rather than running two stages into one. The engine reports stages in sequence, so this
			// is a guard against a future that reports otherwise, not a case seen today.
			if open {
				fmt.Fprintln(out)
			}
			fmt.Fprintf(out, "  %s ...", deployStageLabel(p.Stage))
			startedAt = now()
			open = true
		case controlplane.DeployProgressing:
			// The stage is still running. A build's is minutes long (issue #503), and a line that has
			// sat unchanged for four minutes tells a reader nothing about whether anything is alive —
			// so each report extends it by a dot. It neither opens nor closes a line: with no line
			// open there is nothing to extend.
			if !open {
				return
			}
			fmt.Fprint(out, ".")
		case controlplane.DeployDone, controlplane.DeployFailed:
			if !open {
				return // a close with no open line: print nothing rather than a stray mark
			}
			mark := okMark(out)
			if p.Status == controlplane.DeployFailed {
				mark = failMark(out)
			}
			fmt.Fprintln(out, " "+mark+" ("+formatStageElapsed(now().Sub(startedAt))+")")
			open = false
		}
		// A status this binary does not know is ignored: it cannot be rendered as either the start or
		// the end of a line, and inventing a third shape for it would be worse than silence.
	}
}

// formatStageElapsed renders how long a stage took, rounded to something a person reads at a glance:
// whole seconds once there is a second to round, hundredths below that so a fast stage does not
// report "0s".
func formatStageElapsed(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	if d >= time.Second {
		return d.Round(time.Second).String()
	}
	return d.Round(10 * time.Millisecond).String()
}

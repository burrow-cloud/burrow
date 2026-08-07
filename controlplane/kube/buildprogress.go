// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package kube

import (
	"context"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/burrow-cloud/burrow/controlplane"
)

// An in-cluster build clones, resolves dependencies, and runs every Dockerfile step — minutes,
// routinely — and until issue #503 it said nothing across all of it. That is not only an
// observability gap: every reverse proxy in front of a control plane has a read timeout measured
// BETWEEN TWO SUCCESSIVE READS (60 seconds is ingress-nginx's default), so a build that reports
// nothing while it works outlives the connection carrying its result, and the caller learns nothing
// even when the build succeeds.
//
// This is where the build learns what it is doing. The wait loop is the only place that reads the
// cluster while the Job runs, so it is the only place that can report — and the report is made from
// what the pod actually shows: the Job clones in an init container and builds in the main container,
// so those two stages are observations, not a script of what a build is supposed to do. The push is a
// step inside the builder container's own script and is not observable from outside it, so it is not
// reported (see controlplane.StageBuild).
//
// REPORTING NEVER CHANGES WHAT HAPPENS. The reporter is called beside a poll the wait loop was
// already making, it reads the pod only when it is about to say something, and an unreadable pod
// leaves the last known stage standing rather than failing the build.

// buildProgressInterval is how often a still-running stage repeats itself. It is chosen against the
// timeout it exists to survive: ingress-nginx's default proxy read timeout is 60 seconds, so a report
// every few tens of seconds keeps the connection alive with room to spare, while staying quiet enough
// that a half-hour build does not produce hundreds of lines.
const buildProgressInterval = 10 * time.Second

// buildReporter turns readings of the build Job into the stage events a caller sees. It is a small
// state machine over one value — the stage believed to be running — because that is all the
// vocabulary carries: a reading that names a new stage closes the old one and opens the new, and a
// reading that names the same stage is the stage saying it is still there.
type buildReporter struct {
	progress func(controlplane.DeployEvent)
	// now is the clock the interval is measured on, a field so a test reads a fixed one.
	now      func() time.Time
	interval time.Duration
	stage    string
	lastAt   time.Time
}

// newBuildReporter returns a reporter that emits to progress, which must not be nil (the seam
// normalises a caller that asked for nothing into a no-op).
func newBuildReporter(progress func(controlplane.DeployEvent)) *buildReporter {
	return &buildReporter{progress: progress, now: time.Now, interval: buildProgressInterval}
}

func (r *buildReporter) emit(stage, status string) {
	r.progress(controlplane.DeployEvent{Stage: stage, Status: status})
	r.lastAt = r.now()
}

// start opens the report with the clone, at the moment the Job exists. The Job's first container is
// the clone, so this is a statement about the Job that was just created and not a prediction: what is
// not yet known is how far it will get, and every path below closes whatever stage is open.
func (r *buildReporter) start() {
	r.stage = controlplane.StageClone
	r.emit(controlplane.StageClone, controlplane.DeployStarted)
}

// due reports whether enough time has passed to say something again. The caller checks it BEFORE
// reading the pod, so a poll that has nothing to say costs no API call either.
func (r *buildReporter) due() bool {
	return r.now().Sub(r.lastAt) >= r.interval
}

// enter moves the report to phase, closing the stage that ended. An empty or unchanged phase moves
// nothing — a pod that could not be read is not evidence that anything changed.
func (r *buildReporter) enter(phase string) {
	if phase == "" || phase == r.stage {
		return
	}
	r.emit(r.stage, controlplane.DeployDone)
	r.stage = phase
	r.emit(phase, controlplane.DeployStarted)
}

// observe folds one reading of the build pod into the report: a new phase is a transition, and the
// same phase is the heartbeat that keeps the response on the wire.
func (r *buildReporter) observe(phase string) {
	if phase != "" && phase != r.stage {
		r.enter(phase)
		return
	}
	r.emit(r.stage, controlplane.DeployProgressing)
}

// finish closes the report on a Job that reached a terminal state.
//
// A FAILED Job fails the stage that was running, which is why the caller reads the pod one last time
// before calling this: a clone that could not authenticate and a Dockerfile step that exited non-zero
// are different failures, and the stage is what says which happened.
//
// A SUCCEEDED Job ran both of its containers by definition, so a success that never saw the builder
// container — a pod already reaped, a pod list that never read — still reports the build stage rather
// than ending on the clone. Nothing is invented by that: the Job's success is the evidence.
func (r *buildReporter) finish(ok bool) {
	if !ok {
		r.emit(r.stage, controlplane.DeployFailed)
		return
	}
	if r.stage == controlplane.StageClone {
		r.emit(controlplane.StageClone, controlplane.DeployDone)
		r.stage = controlplane.StageBuild
		r.emit(controlplane.StageBuild, controlplane.DeployStarted)
	}
	r.emit(r.stage, controlplane.DeployDone)
}

// buildPhase reads which half of the build Job is running: the clone init container, or the builder
// container behind it. The init container's own status is the whole signal — the pod cannot reach the
// builder until the clone has terminated successfully, and Kubernetes reports exactly that.
//
// It is BEST-EFFORT, like every other pod read on this path: an unreadable list, a pod that does not
// exist yet, and a status not yet populated all return "", and the caller keeps the stage it already
// had. A build must never fail because the thing describing it could not be read.
func (b *BuildAdapter) buildPhase(ctx context.Context, jobName string) string {
	pods, err := b.client.CoreV1().Pods(b.namespace).List(ctx, metav1.ListOptions{LabelSelector: nameLabel + "=" + jobName})
	if err != nil || len(pods.Items) == 0 {
		return ""
	}
	for i := range pods.Items {
		for _, cs := range pods.Items[i].Status.InitContainerStatuses {
			if cs.Name != cloneContainerName {
				continue
			}
			if t := cs.State.Terminated; t != nil && t.ExitCode == 0 {
				return controlplane.StageBuild
			}
			return controlplane.StageClone
		}
	}
	// A pod exists but says nothing about the clone yet: it is being scheduled or its image is being
	// pulled, and nothing behind the clone can have started.
	return controlplane.StageClone
}

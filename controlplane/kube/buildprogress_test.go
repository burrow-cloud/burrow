// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package kube

import (
	"context"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/burrow-cloud/burrow/controlplane"
)

// The build's own report (issue #503). Two things are under test: that the stages come from what the
// Job's pod actually shows, and that a stage that is still working says so often enough for the
// response to survive a proxy read timeout.

// recordEvents collects reported events as "stage:status".
func recordEvents(got *[]string) func(controlplane.DeployEvent) {
	return func(ev controlplane.DeployEvent) { *got = append(*got, ev.Stage+":"+ev.Status) }
}

// fixedClock is a clock a test advances by hand, so the interval between reports is asserted rather
// than waited out.
type fixedClock struct{ t time.Time }

func (c *fixedClock) now() time.Time          { return c.t }
func (c *fixedClock) advance(d time.Duration) { c.t = c.t.Add(d) }

// newTestReporter returns a reporter on a hand-advanced clock.
func newTestReporter(got *[]string) (*buildReporter, *fixedClock) {
	clock := &fixedClock{t: time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)}
	r := newBuildReporter(recordEvents(got))
	r.now = clock.now
	return r, clock
}

// TestBuildReporterRepeatsARunningStage is the property the whole fix rests on: a stage that runs for
// minutes must put bytes on the wire while it runs. A reverse proxy's read timeout is measured
// between two successive reads, so a build that reports only its transitions is a build that dies at
// 60 seconds inside its longest stage however well it is going.
func TestBuildReporterRepeatsARunningStage(t *testing.T) {
	var got []string
	r, clock := newTestReporter(&got)
	r.start()

	// Poll after poll while nothing changes: only the polls past the interval say anything.
	reports := 0
	for range 20 {
		clock.advance(3 * time.Second) // the build Job's poll interval
		if r.due() {
			r.observe(controlplane.StageClone)
			reports++
		}
	}
	if reports == 0 {
		t.Fatal("a stage running for a minute reported nothing; the response would not survive a proxy read timeout")
	}
	// Sixty seconds is the timeout this exists to survive, so the gap between reports must be well
	// inside it — and the reports must not be every poll either, or a half-hour build is a thousand
	// lines.
	gap := (60 * time.Second) / time.Duration(reports)
	if gap > 30*time.Second {
		t.Errorf("%d reports per minute (a gap of %s); want a report well inside a 60s read timeout", reports, gap)
	}
	if reports > 12 {
		t.Errorf("%d reports per minute; want a heartbeat, not a stream of noise", reports)
	}
	for _, e := range got[1:] {
		if e != "clone:progressing" {
			t.Fatalf("events = %v, want the running stage repeated as progressing", got)
		}
	}
	if got[0] != "clone:started" {
		t.Errorf("first event = %q, want clone:started", got[0])
	}
}

// TestBuildReporterFollowsThePod asserts the stages track the pod rather than a script: the report
// moves to the build when — and only when — the clone container has terminated successfully.
func TestBuildReporterFollowsThePod(t *testing.T) {
	var got []string
	r, clock := newTestReporter(&got)
	r.start()

	clock.advance(buildProgressInterval)
	r.observe(controlplane.StageClone) // still cloning
	clock.advance(buildProgressInterval)
	r.observe(controlplane.StageBuild) // the clone terminated; the builder is running
	clock.advance(buildProgressInterval)
	r.observe(controlplane.StageBuild)
	r.finish(true)

	want := "clone:started,clone:progressing,clone:done,build:started,build:progressing,build:done"
	if strings.Join(got, ",") != want {
		t.Errorf("events = %v, want %s", got, want)
	}
}

// TestBuildReporterKeepsItsStageWhenThePodCannotBeRead pins the best-effort posture: an unreadable
// pod is not evidence that anything changed, so the report holds the stage it had and keeps it alive.
// Enrichment must never be the thing that changes what a build looks like.
func TestBuildReporterKeepsItsStageWhenThePodCannotBeRead(t *testing.T) {
	var got []string
	r, clock := newTestReporter(&got)
	r.start()
	clock.advance(buildProgressInterval)
	r.observe("") // the pod list failed, or there is no pod yet

	want := "clone:started,clone:progressing"
	if strings.Join(got, ",") != want {
		t.Errorf("events = %v, want %s", got, want)
	}
}

// TestBuildReporterFailsTheStageThatWasRunning is what makes the report worth reading on a failure: a
// clone that could not authenticate and a Dockerfile step that exited non-zero are different
// failures, and the stage says which.
func TestBuildReporterFailsTheStageThatWasRunning(t *testing.T) {
	for _, c := range []struct {
		name  string
		phase string
		want  string
	}{
		{name: "the clone failed", phase: controlplane.StageClone, want: "clone:started,clone:failed"},
		{name: "the build failed", phase: controlplane.StageBuild, want: "clone:started,clone:done,build:started,build:failed"},
	} {
		t.Run(c.name, func(t *testing.T) {
			var got []string
			r, _ := newTestReporter(&got)
			r.start()
			r.enter(c.phase)
			r.finish(false)
			if strings.Join(got, ",") != c.want {
				t.Errorf("events = %v, want %s", got, c.want)
			}
		})
	}
}

// TestBuildReporterAlwaysReportsABuildStageOnSuccess covers the Job that finished before anything was
// read from its pod — a fast build, a reaped pod. The Job succeeded, so both of its containers ran:
// reporting a build stage there is reading the Job's success, not inventing a stage.
func TestBuildReporterAlwaysReportsABuildStageOnSuccess(t *testing.T) {
	var got []string
	r, _ := newTestReporter(&got)
	r.start()
	r.finish(true)

	want := "clone:started,clone:done,build:started,build:done"
	if strings.Join(got, ",") != want {
		t.Errorf("events = %v, want %s", got, want)
	}
}

// TestBuildPhaseReadsTheClonesContainerStatus pins where the stage comes from: the Job's init
// container is the clone, and the builder cannot start until it has terminated successfully, so that
// one status decides the phase. A pod that cannot be read at all yields nothing at all.
func TestBuildPhaseReadsTheClonesContainerStatus(t *testing.T) {
	const jobName = "burrow-build-abc123"
	cases := []struct {
		name  string
		state corev1.ContainerState
		want  string
	}{
		{
			name:  "the clone is running",
			state: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
			want:  controlplane.StageClone,
		},
		{
			name:  "the clone is waiting on its image",
			state: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "PodInitializing"}},
			want:  controlplane.StageClone,
		},
		{
			name:  "the clone terminated cleanly, so the builder is what is running",
			state: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}},
			want:  controlplane.StageBuild,
		},
		{
			name:  "the clone terminated badly, so the clone is still the stage that matters",
			state: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 128}},
			want:  controlplane.StageClone,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			client := fake.NewSimpleClientset()
			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name: jobName + "-xyz", Namespace: buildNamespace,
					Labels: map[string]string{nameLabel: jobName},
				},
				Status: corev1.PodStatus{
					InitContainerStatuses: []corev1.ContainerStatus{{Name: cloneContainerName, State: c.state}},
				},
			}
			if _, err := client.CoreV1().Pods(buildNamespace).Create(context.Background(), pod, metav1.CreateOptions{}); err != nil {
				t.Fatalf("seed pod: %v", err)
			}
			if got := NewBuilder(client).buildPhase(context.Background(), jobName); got != c.want {
				t.Errorf("buildPhase = %q, want %q", got, c.want)
			}
		})
	}

	t.Run("no pod at all", func(t *testing.T) {
		if got := NewBuilder(fake.NewSimpleClientset()).buildPhase(context.Background(), jobName); got != "" {
			t.Errorf("buildPhase = %q, want empty when there is no pod to read", got)
		}
	})
}

// TestBuildWithProgressReportsAndReturnsTheSameResult is the adapter end to end: the stages are
// reported and the digest is exactly what Build returns, because reporting sits beside the build and
// is never part of it.
func TestBuildWithProgressReportsAndReturnsTheSameResult(t *testing.T) {
	ctx := context.Background()
	source := controlplane.SourceRef{Repo: "https://github.com/acme/shop", Ref: "abc123"}
	const target = "reg.burrow.svc/acme/shop:2"
	client, _ := buildFakeSucceeding(t, source, target, validDigest)

	var got []string
	digest, err := NewBuilder(client).BuildWithProgress(ctx, source, target, false, controlplane.SourceCredential{}, recordEvents(&got))
	if err != nil {
		t.Fatalf("BuildWithProgress: %v", err)
	}
	if digest != validDigest {
		t.Errorf("digest = %q, want %q", digest, validDigest)
	}
	want := "clone:started,clone:done,build:started,build:done"
	if strings.Join(got, ",") != want {
		t.Errorf("events = %v, want %s", got, want)
	}
}

// TestBuildReportsNothingBeforeTheJobExists pins where the report starts. Everything above the Job —
// a malformed source, an empty target, a cluster with no room for the build pod — is a refusal the
// caller must receive as an ordinary error, and the streaming response's header is written on the
// first event. An event emitted before the build has committed to running would turn every one of
// those refusals into a success with an error inside it.
func TestBuildReportsNothingBeforeTheJobExists(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name   string
		source controlplane.SourceRef
		target string
		b      *BuildAdapter
	}{
		{
			name:   "a malformed source",
			source: controlplane.SourceRef{Repo: "", Ref: ""},
			target: "reg/acme/shop:1",
			b:      NewBuilder(fake.NewSimpleClientset()),
		},
		{
			name:   "an empty target",
			source: controlplane.SourceRef{Repo: "https://github.com/acme/shop", Ref: "v1"},
			target: "",
			b:      NewBuilder(fake.NewSimpleClientset()),
		},
		{
			name:   "a cluster with no room for the build pod",
			source: controlplane.SourceRef{Repo: "https://github.com/acme/shop", Ref: "v1"},
			target: "reg/acme/shop:1",
			b:      NewBuilder(fake.NewSimpleClientset()).WithCapacityProber(stubCapacity{state: noHeadroom()}),
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var got []string
			if _, err := c.b.BuildWithProgress(ctx, c.source, c.target, false, controlplane.SourceCredential{}, recordEvents(&got)); err == nil {
				t.Fatal("BuildWithProgress succeeded on a request that should be refused")
			}
			if len(got) != 0 {
				t.Errorf("events = %v, want none before the build Job exists", got)
			}
		})
	}
}

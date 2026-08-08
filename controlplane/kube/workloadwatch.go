// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package kube

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"

	"github.com/burrow-cloud/burrow/controlplane"
)

// The workload watch (ADR-0079 §1): the cluster reporting what happened to a workload as it happens,
// instead of Burrow asking once a minute whether anything did.
//
// IT DERIVES THROUGH workloadStatus AND NOWHERE ELSE. A watch event is a trigger, not evidence: what
// it delivers is exactly what ListWorkloads would have returned for that app, produced by the same
// function. That is deliberate and it is the sweep's best property, kept — a ledger row and a
// `burrow app status` answer are still derived from one place, so the two cannot disagree about one
// pod.
//
// IT WATCHES PODS AS WELL AS DEPLOYMENTS, because almost every reason in ADR-0074 §2's vocabulary is
// a container or scheduling condition. A Deployment's own object barely moves during a crash loop:
// the replica counts settle and the interesting change is in a pod's status. Watching only
// Deployments would be a watch that misses most of what the ledger exists to record.
//
// IT SAYS WHEN IT LOST ITS PLACE. A reconnect that resumes from the resourceVersion it last saw
// missed nothing and passes in silence; one that cannot — the server has expired that version, or
// the connection failed outright — has to re-list, and a re-list reports current state rather than
// what happened while it was away. That is a gap, and ADR-0079 §4 requires it be visible as plainly
// as a restart.

// watchRelistBackoff is how long a watch that lost its place waits before re-listing. It is a fixed
// pause rather than an exponential one: the failure being backed off from is an API server that is
// unavailable or restarting, one burrowd is the only caller, and a backoff that grows would leave
// the coverage gap open long after the cluster came back — which is the wrong error to make on the
// surface whose job is to be watching.
const watchRelistBackoff = 5 * time.Second

// WatchWorkloads starts a watch over the Burrow-managed workloads in this view's namespace and
// delivers what the cluster says about them until ctx is cancelled (ADR-0079 §1). The initial list
// runs synchronously, so a namespace that cannot be watched at all is an error the caller can report
// and retry rather than a goroutine failing forever in a log.
func (a *Adapter) WatchWorkloads(ctx context.Context, events chan<- controlplane.WorkloadEvent) error {
	deps, err := a.listManagedDeployments(ctx)
	if err != nil {
		return err
	}
	go a.followWorkloads(ctx, events, deps)
	return nil
}

// followWorkloads is the watch's whole life: report the listing it was established with, follow both
// object streams, and on losing its place say so, wait, and re-list.
func (a *Adapter) followWorkloads(ctx context.Context, events chan<- controlplane.WorkloadEvent, first *appsv1.DeploymentList) {
	deps := first
	for {
		if deps == nil {
			var err error
			if deps, err = a.listManagedDeployments(ctx); err != nil {
				if ctx.Err() != nil {
					return
				}
				slog.WarnContext(ctx, "re-listing workloads for the watch failed", "namespace", a.namespace, "error", err)
				if !sleepCtx(ctx, watchRelistBackoff) {
					return
				}
				continue
			}
		}
		// The listing is the complete current picture, and saying so is what RESUMES coverage.
		grace := time.Duration(0) // see WatchWorkloads on the seam: the dwell is the consumer's, spent once
		for i := range deps.Items {
			if !send(ctx, events, controlplane.WorkloadEvent{
				Kind:      controlplane.WorkloadChanged,
				Namespace: a.namespace,
				Status:    a.workloadStatus(ctx, &deps.Items[i], grace),
			}) {
				return
			}
		}
		if !send(ctx, events, controlplane.WorkloadEvent{Kind: controlplane.WorkloadSynced, Namespace: a.namespace}) {
			return
		}

		detail := a.followStreams(ctx, events, deps.ResourceVersion)
		if ctx.Err() != nil {
			return
		}
		if !send(ctx, events, controlplane.WorkloadEvent{
			Kind:      controlplane.WorkloadDropped,
			Namespace: a.namespace,
			Detail:    detail,
		}) {
			return
		}
		deps = nil
		if !sleepCtx(ctx, watchRelistBackoff) {
			return
		}
	}
}

// followStreams follows the Deployment and Pod streams until one of them cannot be resumed, and
// returns the line describing why. A stream that merely ENDS — the API server's own watch timeout,
// the usual case — is reopened from the resourceVersion it last delivered, which loses nothing and
// is therefore not a gap.
func (a *Adapter) followStreams(ctx context.Context, events chan<- controlplane.WorkloadEvent, depRV string) string {
	streams := []*objectStream{
		{
			what: "Deployment",
			rv:   depRV,
			open: func(ctx context.Context, rv string) (watch.Interface, error) {
				return a.client.AppsV1().Deployments(a.namespace).Watch(ctx, managedListOptions(rv))
			},
			app: deploymentApp,
		},
		{
			what: "Pod",
			open: func(ctx context.Context, rv string) (watch.Interface, error) {
				return a.client.CoreV1().Pods(a.namespace).Watch(ctx, managedListOptions(rv))
			},
			app: podApp,
		},
	}
	defer func() {
		for _, s := range streams {
			s.stop()
		}
	}()

	dirty := make(map[string]bool)
	for {
		for _, s := range streams {
			if err := s.ensure(ctx); err != nil {
				return fmt.Sprintf("the %s watch could not be resumed (%v)", s.what, err)
			}
		}
		if detail, ok := collect(ctx, streams, dirty); !ok {
			return detail
		}
		if !a.reportDirty(ctx, events, dirty) {
			return ""
		}
	}
}

// collect blocks for one event and then takes whatever else has already arrived, so a rolling
// update's burst of pod events becomes one derivation per app rather than one per event. It reports
// false with a line when a stream ended and the outer loop should decide what that means.
func collect(ctx context.Context, streams []*objectStream, dirty map[string]bool) (string, bool) {
	// Two streams, so the wait is written out rather than built with reflect.Select. A third would be
	// the moment to change that; there will not be a third.
	for first := true; ; first = false {
		var (
			ev   watch.Event
			from *objectStream
			ok   bool
		)
		if first {
			select {
			case <-ctx.Done():
				return "", false
			case ev, ok = <-streams[0].result():
				from = streams[0]
			case ev, ok = <-streams[1].result():
				from = streams[1]
			}
		} else {
			select {
			case ev, ok = <-streams[0].result():
				from = streams[0]
			case ev, ok = <-streams[1].result():
				from = streams[1]
			default:
				return "", true
			}
		}
		if !ok {
			// The server closed this stream. Reopening it from the version it last delivered is
			// lossless when the server still has that version; ensure decides, and only a refusal
			// there is a gap.
			from.reopen()
			return "", true
		}
		if ev.Type == watch.Error {
			return fmt.Sprintf("the %s watch reported %s", from.what, watchErrorDetail(ev.Object)), false
		}
		from.observe(ev)
		if app := from.app(ev.Object); app != "" {
			dirty[app] = true
		}
	}
}

// reportDirty re-derives every app a stream said something about and sends what it found, then
// empties the set. A Deployment that is gone is reported as gone rather than as an error: whether
// its absence is a FAILURE is a comparison against the registry, and that is the periodic pass's
// question (ADR-0074 §6).
func (a *Adapter) reportDirty(ctx context.Context, events chan<- controlplane.WorkloadEvent, dirty map[string]bool) bool {
	if len(dirty) == 0 {
		return true
	}
	for app := range dirty {
		delete(dirty, app)
		dep, err := a.client.AppsV1().Deployments(a.namespace).Get(ctx, app, metav1.GetOptions{})
		switch {
		case apierrors.IsNotFound(err):
			if !send(ctx, events, controlplane.WorkloadEvent{
				Kind:      controlplane.WorkloadGone,
				Namespace: a.namespace,
				Status:    controlplane.WorkloadStatus{App: app},
			}) {
				return false
			}
		case err != nil:
			// One unreadable object is not a lost watch. The next event about this app re-derives it,
			// and the periodic pass reads it independently.
			slog.WarnContext(ctx, "re-reading a changed workload failed", "namespace", a.namespace, "app", app, "error", err)
		default:
			if !send(ctx, events, controlplane.WorkloadEvent{
				Kind:      controlplane.WorkloadChanged,
				Namespace: a.namespace,
				Status:    a.workloadStatus(ctx, dep, 0),
			}) {
				return false
			}
		}
	}
	return true
}

// objectStream is one of the two watches a workload watch follows, with the bookkeeping that decides
// whether an interruption lost anything.
type objectStream struct {
	what string
	// rv is the resourceVersion last delivered, which a reopen resumes from. Empty asks the server
	// for the current state, which is a re-list and therefore only used before anything was seen.
	rv   string
	w    watch.Interface
	open func(ctx context.Context, rv string) (watch.Interface, error)
	app  func(runtime.Object) string
}

func (s *objectStream) ensure(ctx context.Context) error {
	if s.w != nil {
		return nil
	}
	w, err := s.open(ctx, s.rv)
	if err != nil {
		return err
	}
	s.w = w
	return nil
}

func (s *objectStream) result() <-chan watch.Event {
	if s.w == nil {
		return nil
	}
	return s.w.ResultChan()
}

// observe records the resourceVersion this event carried, which is where a reopen resumes from. An
// object with none — a Status, a type this stream does not model — leaves the mark where it was,
// because resuming from a version that was never delivered is how a watch skips what it has not seen.
func (s *objectStream) observe(ev watch.Event) {
	o, err := meta.Accessor(ev.Object)
	if err != nil || o.GetResourceVersion() == "" {
		return
	}
	s.rv = o.GetResourceVersion()
}

func (s *objectStream) reopen() {
	s.stop()
}

func (s *objectStream) stop() {
	if s.w != nil {
		s.w.Stop()
		s.w = nil
	}
}

// managedListOptions selects the objects Burrow authored, at a resourceVersion. It is the same
// selector ListWorkloads uses, so the watch covers exactly the set the listing does.
func managedListOptions(rv string) metav1.ListOptions {
	return metav1.ListOptions{
		LabelSelector:   managedByLabel + "=" + managedByValue,
		ResourceVersion: rv,
	}
}

func (a *Adapter) listManagedDeployments(ctx context.Context) (*appsv1.DeploymentList, error) {
	deps, err := a.client.AppsV1().Deployments(a.namespace).List(ctx, metav1.ListOptions{
		LabelSelector: managedByLabel + "=" + managedByValue,
	})
	if err != nil {
		return nil, fmt.Errorf("kube: watching workloads in %q: %w", a.namespace, err)
	}
	return deps, nil
}

// deploymentApp is the app a Deployment event is about: its own name.
func deploymentApp(obj runtime.Object) string {
	dep, ok := obj.(*appsv1.Deployment)
	if !ok {
		return ""
	}
	return dep.Name
}

// podApp is the app a Pod event is about, read off the label the workload sets — the same label
// podIssue selects by, so the watch and the inspection agree on which pods belong to which app.
func podApp(obj runtime.Object) string {
	pod, ok := obj.(*corev1.Pod)
	if !ok {
		return ""
	}
	return pod.Labels[nameLabel]
}

// watchErrorDetail renders the Status object a watch.Error event carries, for the one line that says
// why coverage stopped.
func watchErrorDetail(obj runtime.Object) string {
	if st, ok := obj.(*metav1.Status); ok && st.Message != "" {
		return st.Message
	}
	return "an error with no message"
}

// send delivers one event, blocking until the consumer takes it or ctx ends. It BLOCKS rather than
// dropping: a dropped event would leave the consumer's latch believing a condition that has cleared,
// and back-pressure here eventually costs the watch its place, which is a gap that says so.
func send(ctx context.Context, events chan<- controlplane.WorkloadEvent, ev controlplane.WorkloadEvent) bool {
	select {
	case events <- ev:
		return true
	case <-ctx.Done():
		return false
	}
}

// sleepCtx waits d, reporting false if ctx ended first.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}

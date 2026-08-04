// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

// Package kube is the production controlplane.Kubernetes adapter, built on the official
// client-go SDK (ADR-0011). It translates the workload seam into Kubernetes Deployments
// and reads their status, scales, streams logs, and deletes them. It is a thin
// translation layer — no orchestration logic, which lives in the engine. v0.1 supports
// only WorkloadDeployment.
//
// It lives under controlplane/ (not controlplane/internal) so cmd/burrowd and the
// managed module can wire it; it is licensed Apache-2.0.
package kube

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/retry"

	"github.com/burrow-cloud/burrow/controlplane"
)

var _ controlplane.Kubernetes = (*Adapter)(nil)

const (
	nameLabel      = "app.kubernetes.io/name"
	managedByLabel = "app.kubernetes.io/managed-by"
	managedByValue = "burrow"
	// defaultIngressClass is the IngressClass `burrow ingress install` creates (ingress-nginx).
	// The exposed app's Ingress is bound to it so the controller adopts and routes it.
	defaultIngressClass = "nginx"
)

// defaultAddonNamespace is where add-ons land when none is configured (local/test). In a
// real install it is always set explicitly via WithAddonNamespace from BURROW_ADDON_NAMESPACE;
// connect.DefaultAddonNamespace is the authoritative value the install manifest renders.
const defaultAddonNamespace = "burrow-addons"

// Adapter operates Burrow workloads in a single app namespace, and provisions add-ons in a
// separate add-on namespace (ADR-0025) so backing services don't mix with user workloads.
type Adapter struct {
	client kubernetes.Interface
	// dynamic addresses CUSTOM resources — today the CloudNativePG `Cluster` a Postgres add-on
	// instance can be backed by (ADR-0066 §1). It is nil unless wired (WithDynamicClient), and every
	// path that touches a custom resource treats nil as "this build cannot address them", which is
	// indistinguishable from a cluster that has none. That keeps an Adapter built with New — every
	// unit test, and any embedder that has not wired one — exactly as it was.
	dynamic        dynamic.Interface
	namespace      string
	addonNamespace string
	// podMutator is the ADR-0061 deploy-path extension point: an OPTIONAL hook applied to every pod
	// spec this adapter authors for an app — the Deployment's pod template and the one-off run Job
	// (ADR-0048) — after it is constructed and before the object is sent to the API server. It
	// carries cluster requirements the engine cannot know about — a toleration for a tainted node
	// pool, a mandated runtimeClassName, a priority or topology policy. nil (the default) leaves the
	// constructed object exactly as it is. Wired via WithPodMutator.
	podMutator func(*corev1.PodSpec)
	// platformPodMutator is the ADR-0073 §2 extension point for the other half of the split: an
	// OPTIONAL hook applied to every pod spec this adapter authors that runs BURROW's own image
	// rather than the app's — the add-on instances, the log and metrics collectors, and the backup
	// and restore Jobs. It is a second hook rather than a widening of podMutator because the two
	// sets take genuinely different placement policy: an operator may want the tenant's image
	// sandboxed on tenant-only nodes and their own Postgres and collectors somewhere the tenant's
	// code is not. nil (the default) leaves the constructed object exactly as it is. Wired via
	// WithPlatformPodMutator.
	platformPodMutator func(*corev1.PodSpec)
	// controllerPlacement is the ADR-0077 §2 third seam: placement policy for pods this adapter
	// causes to exist but does not author, where a third-party controller composes the pod from a
	// custom resource Burrow creates. It is a VALUE rather than a hook because there is no
	// constructed pod spec to hand a hook — see PodPlacement. The zero value carries no policy.
	// Wired via WithControllerPodPlacement, which refuses policy the target cannot carry.
	controllerPlacement PodPlacement
	// shipperImage is the image the backup Job's shipping container runs — burrowd's own, under the
	// ship-backup subcommand (ADR-0063 §7). Empty (the default) leaves the floating
	// defaultShipperImage; a released burrowd pins its own version, and BURROW_SHIPPER_IMAGE
	// overrides both for a dev or e2e cluster where no published image applies. Wired via
	// WithShipperImage.
	shipperImage string
	// limits reads the operator-set operational configuration for the cluster-tier limits this
	// adapter applies (ADR-0068 §6): the unschedulable grace the pod inspection waits out, and the
	// metrics add-on's sample retention. nil (the default) resolves every limit to its built-in
	// default, which is exactly the behaviour these had as constants. Wired via
	// WithOperationalLimits.
	limits controlplane.ClusterConfigFunc
}

// New returns an Adapter over the given clientset and namespace (defaulting to
// "default"). Tests inject a fake clientset; production injects a real one
// (see NewFromConfig).
func New(client kubernetes.Interface, namespace string) *Adapter {
	if namespace == "" {
		namespace = "default"
	}
	return &Adapter{client: client, namespace: namespace, addonNamespace: defaultAddonNamespace}
}

// WithShipperImage overrides the image the backup Job's shipping container runs (ADR-0063 §7). An
// empty value leaves the default, so a caller can pass an unresolved override through without
// having to branch on it. Returns the Adapter for chaining.
func (a *Adapter) WithShipperImage(image string) *Adapter {
	if image != "" {
		a.shipperImage = image
	}
	return a
}

// WithOperationalLimits registers the source of the operator-set operational limits this adapter
// reads (ADR-0068 §6). It is read at the moment the adapter acts rather than captured here, so
// `burrow cluster config set` takes effect without restarting burrowd. A nil supplier (the default)
// resolves every limit to its built-in default. Returns the Adapter for chaining.
func (a *Adapter) WithOperationalLimits(f controlplane.ClusterConfigFunc) *Adapter {
	a.limits = f
	return a
}

// WithAddonNamespace sets the namespace Burrow deploys add-ons (and their collectors) into,
// kept separate from the app namespace and the credential-holding control-plane namespace
// (ADR-0025). An empty value leaves the default. Returns the Adapter for chaining.
func (a *Adapter) WithAddonNamespace(ns string) *Adapter {
	if ns != "" {
		a.addonNamespace = ns
	}
	return a
}

// WithPodMutator registers a hook the adapter applies to the pod specs it authors for an app, after
// each is constructed and before the object is sent to the API server (ADR-0061). It is the
// deploy-path counterpart of BuildAdapter.WithBuildPodMutator (ADR-0053 §6).
//
// Its reach is every pod this adapter runs the app's own image in: the Deployment's pod template,
// and the one-off command Job of ADR-0048. A run is the app's image, in the app's namespace, with
// the app's environment — the same workload for one command — so it is admitted and scheduled under
// the same cluster constraints, and a hook that covered only the Deployment would leave `burrow app
// run` unschedulable on precisely the clusters this seam exists for. Add-ons, collectors, and the
// backup and restore Jobs are NOT covered: they run images Burrow chooses rather than the app's, so
// they take WithPlatformPodMutator (ADR-0073 §2). The build Job has its own hook again
// (BuildAdapter.WithBuildPodMutator), for Burrow's builder image over the app's source.
//
// It exists for cluster requirements the engine cannot know about, because they are properties of a
// cluster rather than of Burrow: a toleration for a tainted node pool (a GPU pool, spot capacity, a
// pool reserved for one team), a mandated runtimeClassName, a priorityClassName, a
// topologySpreadConstraint, a nodeSelector, an image-pull secret for a private base registry. Burrow
// hard-codes none of these — the operator embedding the engine supplies what their cluster requires.
// A nil mutator (the default) leaves every object this adapter constructs exactly as-is.
//
// Unlike the build seam, whose Job is created once, this hook runs on EVERY write of the pod
// template — creates and updates alike, so a rollout does not drop what the deploy was given
// (ADR-0061 §2), and once more per run. The mutator must therefore be idempotent: appending to a
// slice (tolerations, volumes, env) without first checking whether the entry is already there will
// drift across redeploys. Set or replace rather than append blindly.
//
// It must also tolerate a Job pod, not only a Deployment's: a run pod arrives with RestartPolicy
// Never already set, and a mutator that overwrites it produces a Job the API server rejects.
//
// The hook is trusted and unvalidated: it can set anything on the pod spec, including breaking it.
// It is compiled into the binary by whoever operates that binary, not supplied at runtime.
//
// Returns the adapter for chaining.
func (a *Adapter) WithPodMutator(fn func(*corev1.PodSpec)) *Adapter {
	a.podMutator = fn
	return a
}

// WithPlatformPodMutator registers a hook the adapter applies to the pod specs it authors that run
// BURROW's own images, after each is constructed and before the object is sent to the API server
// (ADR-0073 §2, §6). It is the platform-side counterpart of WithPodMutator, which covers the app's
// own image.
//
// Its reach is every pod this adapter runs on Burrow's behalf rather than the app's: the add-on
// instance Deployment (Postgres, the logs and metrics stores, the cache), the log-collector
// DaemonSet, the metrics-collector Deployment, and the backup and restore Jobs. Without it those
// pods carry no placement fields at all, so on the tainted-pool cluster ADR-0061 was written for an
// operator has working deploys and a backup that never runs — and each failure is quiet. A Pending
// Job leaves both Failed and Succeeded at zero, so the waiter burns its full timeout and reports a
// timeout rather than an unschedulable pod; a Pending add-on reports zero ready replicas, which
// reads like a slow start. The worst case is the restore, discovered during an incident.
//
// Two hooks rather than one, because the two sets take genuinely different policy: a managed
// operator may want the tenant's image under a sandboxed runtime on tenant-only nodes, and their
// own Postgres and collectors somewhere the tenant's code is not. One hook could serve that only by
// having the operator key off a container image or a label to reconstruct a classification this
// package already has, and a wrong branch puts the tenant's code on the platform pool. Which hook a
// path gets is decided here, and stated at each call site.
//
// The mutator runs over the FULLY-constructed pod spec and on every write of it, so two obligations
// follow. It must be **idempotent** — appending to a slice (tolerations, volumes, env) without
// first checking whether the entry is already there will drift; set or replace rather than append
// blindly. And it must **tolerate pod specs it did not expect**: a backup or restore Job pod
// arrives with RestartPolicy Never already set, and the log-collector DaemonSet pod arrives with a
// blanket `Operator: Exists` toleration it is meant to keep (ADR-0073 §3 — a collector that skips
// tainted nodes silently loses exactly those nodes' logs). Overwriting either produces an object
// the API server rejects or a collector that stops collecting.
//
// This hook is more dangerous than the app one, because it reaches STATEFUL workloads: the Postgres
// add-on holds tenant data, and a mutator that moves that pod to a pool where its volume cannot
// attach breaks the add-on rather than one deploy. The trust model is unchanged — the hook is
// compiled into the binary by whoever operates that binary, not supplied at runtime — but the blast
// radius is not.
//
// Wiring nothing sandboxes nothing (ADR-0073 §5). This is a seam, not enforcement: the engine wires
// neither hook itself, and an operator who needs isolation enforced wants admission policy. A nil
// mutator (the default) leaves every object this adapter constructs byte-for-byte as it is today
// (ADR-0073 §4).
//
// Returns the adapter for chaining.
func (a *Adapter) WithPlatformPodMutator(fn func(*corev1.PodSpec)) *Adapter {
	a.platformPodMutator = fn
	return a
}

// applyPlatformPodMutator runs the ADR-0073 §2 platform hook over a fully-constructed pod spec, if
// one is wired. Every site that authors a pod running Burrow's own image calls this as its last
// step before the object is written, so the hook sees what the engine composed and a path that
// later grows an update alongside its create carries the mutation on both (ADR-0073 §6). A nil
// mutator is a no-op, which is what makes §4's byte-for-byte guarantee hold everywhere at once.
func (a *Adapter) applyPlatformPodMutator(pod *corev1.PodSpec) {
	if a.platformPodMutator != nil {
		a.platformPodMutator(pod)
	}
}

// WithNamespace returns a copy of the Adapter whose app-resource operations act in ns instead of
// the configured app namespace — the mechanism that routes an operation to a named environment's
// namespace (ADR-0035 phase 2). The add-on namespace is unchanged, so add-ons still land in their
// own namespace. An empty ns, or ns equal to the current app namespace, returns the receiver
// unchanged, so default-environment behavior is identical to before environments existed. The copy
// is shallow: it shares the same clients (typed and dynamic) and ALL THREE placement seams — the app hook of ADR-0061,
// the platform hook of ADR-0073, and the controller placement of ADR-0077 — so an environment-scoped
// view applies the same policy, and a seam wired once at construction reaches every per-tenant view
// of the adapter. That is load-bearing: policy that survived only on the receiver would work in a
// single-namespace install and silently stop applying the moment an operation was routed to a named
// environment. No new connection is made per operation.
func (a *Adapter) WithNamespace(ns string) controlplane.Kubernetes {
	if ns == "" || ns == a.namespace {
		return a
	}
	cp := *a
	cp.namespace = ns
	return &cp
}

func (a *Adapter) ApplyWorkload(ctx context.Context, spec controlplane.WorkloadSpec) error {
	if spec.Kind != "" && spec.Kind != controlplane.WorkloadDeployment {
		return fmt.Errorf("kube: workload kind %q is not supported in v0.1 (Deployment only): %w", spec.Kind, controlplane.ErrNotImplemented)
	}
	deployments := a.client.AppsV1().Deployments(a.namespace)

	// Create-or-update under conflict retry: the Deployment controller continuously
	// updates the live object (its status), so a get-then-update can lose the
	// resourceVersion race and 409. We re-read and retry on conflict. The closure
	// returns raw API errors so retry.RetryOnConflict can recognize a conflict.
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		existing, err := deployments.Get(ctx, spec.App, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			_, err := deployments.Create(ctx, a.buildDeployment(spec), metav1.CreateOptions{})
			return err
		}
		if err != nil {
			return err
		}
		desired := a.buildDeployment(spec)
		desired.ResourceVersion = existing.ResourceVersion
		_, err = deployments.Update(ctx, desired, metav1.UpdateOptions{})
		return err
	})
	if err != nil {
		return fmt.Errorf("kube: applying deployment %q: %w", spec.App, err)
	}
	return nil
}

func (a *Adapter) WorkloadStatus(ctx context.Context, app string) (controlplane.WorkloadStatus, error) {
	dep, err := a.client.AppsV1().Deployments(a.namespace).Get(ctx, app, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return controlplane.WorkloadStatus{}, fmt.Errorf("kube: deployment %q: %w", app, controlplane.ErrNotFound)
	}
	if err != nil {
		return controlplane.WorkloadStatus{}, fmt.Errorf("kube: reading deployment %q: %w", app, err)
	}
	return a.workloadStatus(ctx, dep, unschedulableGrace(ctx, a.limits)), nil
}

// workloadStatus maps a Deployment to a WorkloadStatus and enriches it with the health of the
// CURRENT release. The DeploymentAvailable condition alone is not enough: Kubernetes holds it
// True throughout a rolling update as long as the PREVIOUS ReplicaSet still meets minimum
// availability, so a new release whose image cannot be pulled reads as healthy while the old
// pods keep serving (issue #307). When the newest revision has not finished rolling out, this
// inspects the app's pods for a blocking condition and checks the progress deadline: either means
// the current release is not serving, so it is reported not-available with the actionable Issue. A
// merely-in-progress rollout — a new pod still ContainerCreating, deadline not yet exceeded — has
// no blocking reason, so availability is left as reported and a normal deploy is not flagged as
// broken. Enrichment is best-effort: a pod-list error leaves the base status untouched. It is
// shared by WorkloadStatus and ListWorkloads so both surfaces agree.
func (a *Adapter) workloadStatus(ctx context.Context, dep *appsv1.Deployment, grace time.Duration) controlplane.WorkloadStatus {
	var desired int32
	if dep.Spec.Replicas != nil {
		desired = *dep.Spec.Replicas
	}
	image := ""
	if c := dep.Spec.Template.Spec.Containers; len(c) > 0 {
		image = c[0].Image
	}
	st := controlplane.WorkloadStatus{
		App:             dep.Name,
		Kind:            controlplane.WorkloadDeployment,
		Image:           image,
		DesiredReplicas: desired,
		ReadyReplicas:   dep.Status.ReadyReplicas,
		UpdatedReplicas: dep.Status.UpdatedReplicas,
		Available:       deploymentAvailable(dep, desired),
	}
	if deploymentRolledOut(dep, desired) {
		return st // the current release is fully rolled out and serving
	}
	// The newest revision has not completed. A blocking pod condition is the immediate, reliable
	// signal that the current release is wedged; an exceeded progress deadline is the general one.
	// Either downgrades availability so a broken deploy does not read as healthy on the strength of
	// the superseded release still serving. The pod condition is checked first because it names the
	// fix, where the deadline only reports that time ran out.
	if issue, reason := a.podIssue(ctx, dep.Name, grace); reason != "" {
		st.Issue, st.IssueReason = issue, reason
		st.Available = false
	} else if deploymentProgressStalled(dep) {
		// A stalled rollout with no blocking pod condition left an empty Issue before ADR-0074 §2:
		// the surface said "not available" and nothing else, which is the exact silence that record
		// is about. It carries the deadline as the reason now, and the message says plainly that
		// Burrow found nothing more specific rather than implying it knows the cause.
		ev := controlplane.IssueEvidence{Reason: controlplane.ReasonProgressDeadlineExceeded}
		st.Issue, st.IssueReason = ev.Message(), ev.Reason
		st.Available = false
	}
	return st
}

// podIssue inspects the app's pods for a blocking, human-fixable condition and, if one is found,
// returns the actionable Issue message and its reason from the closed set (ADR-0074 §2). It selects
// the app's pods by the same label the workload sets (nameLabel=app), reads each container's
// waiting and termination state, and falls back to the pod's scheduling condition when no container
// has started at all.
//
// It is best-effort by contract: a list error, or no blocking condition, yields ("", ""), so a
// status call never fails on its enrichment. Only the CRITERION decides what is reported — blocking
// and human-fixable, never self-resolving — so a transient ContainerCreating stays invisible here
// no matter how long it lasts.
func (a *Adapter) podIssue(ctx context.Context, app string, grace time.Duration) (issue, reason string) {
	best, bestPod, ok := selectPodIssue(ctx, a.client, a.namespace, nameLabel+"="+app, func(pod *corev1.Pod) (controlplane.IssueEvidence, bool) {
		return podIssueEvidence(pod, grace)
	})
	if !ok {
		return "", "" // best-effort: never fail Status on enrichment
	}
	// The log tail is fetched only once a crash loop has actually been selected, so a healthy app
	// (and every other failure class) costs no extra API call. It is the app's own output, so it is
	// bounded on the way in as well as in the message — see previousLogTail.
	if best.Reason == controlplane.ReasonCrashLoopBackOff {
		best.LogTail = a.previousLogTail(ctx, bestPod, best.Container)
	}
	return best.Message(), best.Reason
}

// selectPodIssue lists the pods matching selector and returns the single highest-ranked piece of
// evidence any of them reports, with the name of the pod it came from. It is the shared half of
// "which of these pods is broken, and why": the Deployment status path passes podIssueEvidence, and
// the Job waiters of issue #352 pass a narrower reader that only considers a pod that has not
// started. Factoring it here rather than copying the loop is deliberate — a second implementation of
// this inspection could disagree with the status surface about the same pod, which is worse than the
// bug either one fixes.
//
// A list error yields ok=false rather than an error: EVERY caller treats this as enrichment that
// must not become the thing that fails, whether it is a status read or a Job wait.
func selectPodIssue(ctx context.Context, client kubernetes.Interface, namespace, selector string, read func(*corev1.Pod) (controlplane.IssueEvidence, bool)) (ev controlplane.IssueEvidence, pod string, ok bool) {
	pods, err := client.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return controlplane.IssueEvidence{}, "", false
	}
	bestRank := 0
	for i := range pods.Items {
		e, found := read(&pods.Items[i])
		if !found {
			continue
		}
		if r := issueRank(e.Reason); r > bestRank {
			ev, pod, bestRank = e, pods.Items[i].Name, r
		}
	}
	return ev, pod, bestRank > 0
}

// issueRank orders the blocking reasons by how directly each names the fix. WorkloadStatus carries
// ONE Issue, and a broken pod routinely reports several at once — an OOM kill is also a crash loop;
// ADR-0074 §5 keeps both only because the ledger can, and the ledger is not this. So the surface
// picks, and it picks the reason that points at the actual cause: the memory limit over the restart
// it caused, the missing key over the container that could not be created for want of it. A reason
// outside the closed set ranks 0 and is not an Issue at all.
func issueRank(reason string) int {
	switch reason {
	case controlplane.ReasonStartError:
		// Ranked above the pull family because it is strictly more concrete: the image was pulled,
		// the container was created, and the command it was given is not in it. There is no more
		// direct statement of the fix available from a pod.
		return 7
	case controlplane.ReasonImagePullBackOff, controlplane.ReasonErrImagePull:
		return 6
	case controlplane.ReasonCreateContainerConfigError:
		return 5
	case controlplane.ReasonOOMKilled:
		return 4
	case controlplane.ReasonCrashLoopBackOff:
		return 3
	case controlplane.ReasonVolumeUnavailable:
		return 2
	case controlplane.ReasonUnschedulable:
		return 1
	}
	return 0
}

// podIssueEvidence reads one pod for the highest-ranked blocking condition it reports. Container
// state is preferred over the pod's scheduling condition because a pod with running containers has
// obviously been scheduled; the scheduling condition is what explains a pod with no container state
// at all.
func podIssueEvidence(pod *corev1.Pod, grace time.Duration) (controlplane.IssueEvidence, bool) {
	var best controlplane.IssueEvidence
	bestRank := 0
	for _, cs := range pod.Status.ContainerStatuses {
		ev, ok := containerIssueEvidence(pod, cs)
		if !ok {
			continue
		}
		if r := issueRank(ev.Reason); r > bestRank {
			best, bestRank = ev, r
		}
	}
	if bestRank > 0 {
		return best, true
	}
	return schedulingIssueEvidence(pod, grace)
}

// containerIssueEvidence reads one container's status for a blocking condition.
//
// An OOM kill is read from the LAST termination rather than treated as a state of its own, because
// that is where the kernel's verdict is recorded: the pod's visible state is CrashLoopBackOff and
// the kill is what names the fix. It is only reported while the container is CURRENTLY blocked —
// waiting in a back-off, or terminated and not restarting — so a container that was OOM-killed an
// hour ago and has been serving since is not reported as broken now. That is the criterion applied
// to a fact that outlives the failure it describes.
func containerIssueEvidence(pod *corev1.Pod, cs corev1.ContainerStatus) (controlplane.IssueEvidence, bool) {
	if ev, ok := waitingContainerIssueEvidence(pod, cs); ok {
		return ev, true
	}
	// A container that is terminated and staying that way (a run Job's pod, or a Deployment pod
	// between the kill and the restart) still reports the kill that ended it.
	if t := cs.State.Terminated; t != nil && t.Reason == controlplane.ReasonOOMKilled {
		return oomEvidence(pod, cs.Name), true
	}
	return controlplane.IssueEvidence{}, false
}

// waitingContainerIssueEvidence reads a container's WAITING state — the half of the inspection that
// describes a container which has not run. It is split out of containerIssueEvidence because the Job
// waiters of issue #352 need exactly this half and must not have the other: a Job pod whose
// container TERMINATED has run, and its outcome belongs to the Job's own counters (a non-zero exit
// from `burrow app run` is a result, not an error — ADR-0048 §3). Failing a Job wait on a terminated
// container would turn that result into a failure.
func waitingContainerIssueEvidence(pod *corev1.Pod, cs corev1.ContainerStatus) (controlplane.IssueEvidence, bool) {
	w := cs.State.Waiting
	if w == nil {
		return controlplane.IssueEvidence{}, false
	}
	switch {
	case controlplane.IsImagePullReason(w.Reason):
		return controlplane.IssueEvidence{Reason: w.Reason, Container: cs.Name, Image: cs.Image, Detail: w.Message}, true
	case w.Reason == controlplane.ReasonCreateContainerConfigError:
		// The kubelet's message names the missing ConfigMap/Secret and the missing KEY, which is
		// the actionable part and carries no value (ADR-0074 §9).
		return controlplane.IssueEvidence{Reason: w.Reason, Container: cs.Name, Detail: w.Message}, true
	case w.Reason == controlplane.ReasonCrashLoopBackOff:
		if t := cs.LastTerminationState.Terminated; t != nil {
			if t.Reason == controlplane.ReasonOOMKilled {
				return oomEvidence(pod, cs.Name), true
			}
			return controlplane.IssueEvidence{Reason: w.Reason, Container: cs.Name, ExitCode: t.ExitCode}, true
		}
		return controlplane.IssueEvidence{Reason: w.Reason, Container: cs.Name}, true
	}
	return controlplane.IssueEvidence{}, false
}

// startErrorEvidence reads a container status for the one blocking condition that arrives as a
// TERMINATED state: a container the kubelet created but could not execute (issue #478).
//
// It is deliberately separate from waitingContainerIssueEvidence rather than folded into it, because
// the Job waiters' rule — a terminated container HAS RUN, and its outcome is the Job's, not the
// waiter's — holds for every terminated state except this one. StartError and ContainerCannotRun are
// the kubelet's two ways of saying the command never executed, so a container in either state has
// produced no result for anyone to interpret; treating it as a run would report a workload's verdict
// where there was none. The set is closed on purpose: an ordinary non-zero exit stays a result.
//
// The kubelet's message is the diagnosis (`exec: "/burrowd": stat /burrowd: no such file or
// directory`), and it names paths and executables — never a secret value (ADR-0074 §9).
func startErrorEvidence(cs corev1.ContainerStatus) (controlplane.IssueEvidence, bool) {
	t := cs.State.Terminated
	if t == nil {
		return controlplane.IssueEvidence{}, false
	}
	switch t.Reason {
	case controlplane.ReasonStartError, "ContainerCannotRun":
		return controlplane.IssueEvidence{
			Reason:    controlplane.ReasonStartError,
			Container: cs.Name,
			Image:     cs.Image,
			Detail:    t.Message,
			ExitCode:  t.ExitCode,
		}, true
	}
	return controlplane.IssueEvidence{}, false
}

// oomEvidence names the memory limit the container was killed for exceeding, read from the pod's own
// spec — the limit IS the fix, and a message that reported only "OOMKilled" would send the reader to
// `kubectl describe` for the one number they need. An empty detail means no limit is set, which the
// message reports as node pressure rather than as a limit it could not read.
func oomEvidence(pod *corev1.Pod, container string) controlplane.IssueEvidence {
	return controlplane.IssueEvidence{Reason: controlplane.ReasonOOMKilled, Container: container, Detail: containerMemoryLimit(pod, container)}
}

func containerMemoryLimit(pod *corev1.Pod, container string) string {
	for _, c := range pod.Spec.Containers {
		if c.Name != container {
			continue
		}
		if q, ok := c.Resources.Limits[corev1.ResourceMemory]; ok {
			return q.String()
		}
	}
	return ""
}

// unschedulableGrace is how long a pod must have been unschedulable before Burrow calls it an Issue.
// The scheduler marks a pod Unschedulable after ONE failed attempt, which a rolling update can hit
// for a few seconds while the old pods still hold the capacity the new one wants, and on a cluster
// with an autoscaler while a node is being provisioned. Reporting those would flip a healthy app to
// not-available mid-deploy — the noise the criterion exists to prevent. A pod still unschedulable
// after this has stopped resolving on its own.
//
// It is cluster CONFIGURATION rather than a constant (ADR-0068 §6): how long a pod may sit
// unschedulable before something is wrong is a property of the cluster's scheduler and whether it
// has an autoscaler, which is a fact only the operator has. `status.unschedulable_grace` is where
// they set it, and its built-in default is the thirty seconds this was.
//
// It is resolved HERE, once per inspection, and threaded into schedulingIssueEvidence, which is the
// ONLY place it is applied — so the Job waiters of issue #352 inherit the same value rather than
// carrying one of their own, and ADR-0074's failure ledger inherits it too because the ledger
// records the reason this inspection produced rather than judging schedulability a second time. That
// is ADR-0068 §6's requirement, not a convenience: two surfaces that answered it differently would
// hold two definitions of "failure" for one pod at one moment.
func unschedulableGrace(ctx context.Context, limits controlplane.ClusterConfigFunc) time.Duration {
	return limits.ClusterDuration(ctx, controlplane.LimitUnschedulableGrace)
}

// schedulingIssueEvidence reads the pod's PodScheduled condition for a scheduling failure, and
// separates the volume case out of it: Kubernetes reports "no node can run this pod" for both a
// cluster with no room and a claim that will not bind, but the fixes have nothing in common, and an
// agent branching on the reason should not have to grep the scheduler's prose to tell them apart.
//
// grace is the configured unschedulable grace, passed in rather than read here so every caller
// applies the one value (see unschedulableGrace).
func schedulingIssueEvidence(pod *corev1.Pod, grace time.Duration) (controlplane.IssueEvidence, bool) {
	for _, c := range pod.Status.Conditions {
		if c.Type != corev1.PodScheduled || c.Status != corev1.ConditionFalse || c.Reason != corev1.PodReasonUnschedulable {
			continue
		}
		if !c.LastTransitionTime.IsZero() && time.Since(c.LastTransitionTime.Time) < grace {
			return controlplane.IssueEvidence{}, false
		}
		reason := controlplane.ReasonUnschedulable
		if isVolumeUnavailable(c.Message) {
			reason = controlplane.ReasonVolumeUnavailable
		}
		return controlplane.IssueEvidence{Reason: reason, Detail: c.Message}, true
	}
	return controlplane.IssueEvidence{}, false
}

// isVolumeUnavailable reports whether the scheduler's verdict is about a volume rather than about
// capacity or placement. Matching on the scheduler's phrasing is a heuristic, and a deliberately
// conservative one: an unmatched volume failure falls back to ReasonUnschedulable carrying the same
// message, so the reader still gets the scheduler's words — the classification degrades, the
// information does not.
func isVolumeUnavailable(message string) bool {
	m := strings.ToLower(message)
	for _, s := range []string{"persistentvolumeclaim", "volume node affinity conflict", "had volume node affinity", "no available persistent volumes"} {
		if strings.Contains(m, s) {
			return true
		}
	}
	return false
}

// previousLogTail captures the last lines of a crash-looping container's PREVIOUS run — the output
// from the run that actually failed, which the current, backing-off container no longer has. It is
// bounded twice: by tailLines at the API server, and by a LimitReader here, because the line bound
// alone bounds nothing when one line of application output can be megabytes. Best-effort: a pod
// whose previous log has already been rotated away, or a cluster that refuses the read, yields "",
// and the Issue still names the exit code.
func (a *Adapter) previousLogTail(ctx context.Context, pod, container string) string {
	if pod == "" {
		return ""
	}
	lines := int64(controlplane.IssueLogTailLines)
	stream, err := a.client.CoreV1().Pods(a.namespace).GetLogs(pod, &corev1.PodLogOptions{
		Container: container,
		Previous:  true,
		TailLines: &lines,
	}).Stream(ctx)
	if err != nil {
		return ""
	}
	defer stream.Close()
	// One byte over the budget, so the message can mark the cut rather than present a truncated
	// tail as a complete one.
	data, err := io.ReadAll(io.LimitReader(stream, controlplane.IssueLogTailBytes+1))
	if err != nil {
		return ""
	}
	return string(data)
}

func (a *Adapter) ListWorkloads(ctx context.Context) ([]controlplane.WorkloadStatus, error) {
	deps, err := a.client.AppsV1().Deployments(a.namespace).List(ctx, metav1.ListOptions{
		LabelSelector: managedByLabel + "=" + managedByValue,
	})
	if err != nil {
		return nil, fmt.Errorf("kube: listing deployments: %w", err)
	}
	// The unschedulable grace is resolved ONCE for the whole listing rather than per app: it is
	// cluster configuration, so every app in the namespace is judged against the same value, and a
	// read per app would put a database call behind every row of `burrow app list`.
	grace := unschedulableGrace(ctx, a.limits)
	out := make([]controlplane.WorkloadStatus, 0, len(deps.Items))
	for i := range deps.Items {
		// Enrich each app the same way single-app Status does, so a wedged rollout (a new
		// release stuck in ImagePullBackOff while the old pods still serve) surfaces its Issue
		// and reads not-available in `burrow app list`, not only in `burrow app logs` (#307).
		out = append(out, a.workloadStatus(ctx, &deps.Items[i], grace))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].App < out[j].App })
	return out, nil
}

func (a *Adapter) ScaleWorkload(ctx context.Context, app string, replicas int32) error {
	patch := []byte(fmt.Sprintf(`{"spec":{"replicas":%d}}`, replicas))
	_, err := a.client.AppsV1().Deployments(a.namespace).Patch(ctx, app, types.MergePatchType, patch, metav1.PatchOptions{})
	if apierrors.IsNotFound(err) {
		return fmt.Errorf("kube: deployment %q: %w", app, controlplane.ErrNotFound)
	}
	if err != nil {
		return fmt.Errorf("kube: scaling deployment %q: %w", app, err)
	}
	return nil
}

func (a *Adapter) Logs(ctx context.Context, app string, opts controlplane.LogOptions) ([]controlplane.LogLine, error) {
	// Confirm the workload exists so an unknown app is ErrNotFound, not empty logs.
	if _, err := a.client.AppsV1().Deployments(a.namespace).Get(ctx, app, metav1.GetOptions{}); apierrors.IsNotFound(err) {
		return nil, fmt.Errorf("kube: deployment %q: %w", app, controlplane.ErrNotFound)
	} else if err != nil {
		return nil, fmt.Errorf("kube: reading deployment %q: %w", app, err)
	}

	pods, err := a.client.CoreV1().Pods(a.namespace).List(ctx, metav1.ListOptions{LabelSelector: nameLabel + "=" + app})
	if err != nil {
		return nil, fmt.Errorf("kube: listing pods for %q: %w", app, err)
	}

	// Timestamps asks Kubernetes to stamp every emitted line with the instant it recorded, so a
	// consumer can order, window, and correlate entries instead of regexing whatever the
	// application happened to print (#480). The prefix is stripped back off in parseLogStream:
	// it is Burrow's metadata, not the application's output.
	podOpts := corev1.PodLogOptions{Timestamps: true}
	if opts.TailLines > 0 {
		tl := int64(opts.TailLines)
		podOpts.TailLines = &tl
	}

	var lines []controlplane.LogLine
	for _, pod := range pods.Items {
		stream, err := a.client.CoreV1().Pods(a.namespace).GetLogs(pod.Name, &podOpts).Stream(ctx)
		if err != nil {
			return nil, fmt.Errorf("kube: logs for pod %q: %w", pod.Name, err)
		}
		data, readErr := io.ReadAll(stream)
		stream.Close()
		if readErr != nil {
			return nil, fmt.Errorf("kube: reading logs for pod %q: %w", pod.Name, readErr)
		}
		lines = append(lines, parseLogStream(pod.Name, string(data))...)
	}
	return lines, nil
}

// parseLogStream splits one pod's `timestamps=true` log stream into LogLines.
//
// Kubernetes prefixes each line it emits with an RFC3339Nano instant in UTC and a single
// space. That prefix is parsed off: the instant becomes LogLine.Timestamp (kept in UTC, as
// the API emits it — never shifted to the reader's local zone) and everything after the
// space becomes LogLine.Message, so the message is exactly what the application wrote.
//
// A line whose prefix does not parse is treated as a **continuation** of the line before it.
// In a stamped stream Kubernetes stamps everything it emits, so the only way an unstamped
// line arises is a partial or malformed record — a very long line the API split, or a write
// that was cut off. Such a line keeps its raw text as the message and inherits the last
// timestamp seen *for this same pod*, which is the closest true instant available for it.
//
// LogLine.Timestamp is therefore zero only when no time could be read at all: an unparseable
// line at the very start of a pod's stream, with nothing earlier to carry forward. A zero
// timestamp is a genuine "no time was readable", not a field that was left unset.
func parseLogStream(pod, data string) []controlplane.LogLine {
	var (
		lines []controlplane.LogLine
		last  time.Time // most recent instant read from this pod, carried into continuations
	)
	for _, raw := range strings.Split(strings.TrimRight(data, "\n"), "\n") {
		if raw == "" {
			continue
		}
		ts, msg, ok := splitLogTimestamp(raw)
		if ok {
			last = ts
		} else {
			ts, msg = last, raw
		}
		if msg == "" {
			continue // a blank application line carries no information
		}
		lines = append(lines, controlplane.LogLine{Pod: pod, Timestamp: ts, Message: msg})
	}
	return lines
}

// splitLogTimestamp separates the "<RFC3339Nano> " prefix Kubernetes writes when timestamps
// are requested from the application's own text. ok is false when the line carries no
// parseable prefix.
func splitLogTimestamp(line string) (ts time.Time, msg string, ok bool) {
	stamp, rest, found := strings.Cut(line, " ")
	if !found {
		return time.Time{}, "", false
	}
	parsed, err := time.Parse(time.RFC3339Nano, stamp)
	if err != nil {
		return time.Time{}, "", false
	}
	return parsed.UTC(), rest, true
}

func (a *Adapter) DeleteWorkload(ctx context.Context, app string) error {
	err := a.client.AppsV1().Deployments(a.namespace).Delete(ctx, app, metav1.DeleteOptions{})
	if apierrors.IsNotFound(err) {
		return fmt.Errorf("kube: deployment %q: %w", app, controlplane.ErrNotFound)
	}
	if err != nil {
		return fmt.Errorf("kube: deleting deployment %q: %w", app, err)
	}
	return nil
}

func (a *Adapter) Expose(ctx context.Context, spec controlplane.ExposeSpec) error {
	services := a.client.CoreV1().Services(a.namespace)
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		existing, err := services.Get(ctx, spec.App, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			_, err := services.Create(ctx, a.buildService(spec), metav1.CreateOptions{})
			return err
		}
		if err != nil {
			return err
		}
		desired := a.buildService(spec)
		desired.ResourceVersion = existing.ResourceVersion
		desired.Spec.ClusterIP = existing.Spec.ClusterIP // ClusterIP is immutable
		_, err = services.Update(ctx, desired, metav1.UpdateOptions{})
		return err
	})
	if err != nil {
		return fmt.Errorf("kube: applying service %q: %w", spec.App, err)
	}

	ingresses := a.client.NetworkingV1().Ingresses(a.namespace)
	err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
		existing, err := ingresses.Get(ctx, spec.App, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			_, err := ingresses.Create(ctx, a.buildIngress(spec), metav1.CreateOptions{})
			return err
		}
		if err != nil {
			return err
		}
		desired := a.buildIngress(spec)
		desired.ResourceVersion = existing.ResourceVersion
		_, err = ingresses.Update(ctx, desired, metav1.UpdateOptions{})
		return err
	})
	if err != nil {
		return fmt.Errorf("kube: applying ingress %q: %w", spec.App, err)
	}
	return nil
}

func (a *Adapter) Unexpose(ctx context.Context, app string) error {
	ingErr := a.client.NetworkingV1().Ingresses(a.namespace).Delete(ctx, app, metav1.DeleteOptions{})
	svcErr := a.client.CoreV1().Services(a.namespace).Delete(ctx, app, metav1.DeleteOptions{})

	// Treat the operation as not-found only when neither resource existed; otherwise we
	// removed at least one and report any real failure.
	if apierrors.IsNotFound(ingErr) && apierrors.IsNotFound(svcErr) {
		return fmt.Errorf("kube: exposure for %q: %w", app, controlplane.ErrNotFound)
	}
	if ingErr != nil && !apierrors.IsNotFound(ingErr) {
		return fmt.Errorf("kube: deleting ingress %q: %w", app, ingErr)
	}
	if svcErr != nil && !apierrors.IsNotFound(svcErr) {
		return fmt.Errorf("kube: deleting service %q: %w", app, svcErr)
	}
	return nil
}

func (a *Adapter) ExposureStatus(ctx context.Context, app string) (controlplane.ExposureStatus, error) {
	ing, err := a.client.NetworkingV1().Ingresses(a.namespace).Get(ctx, app, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return controlplane.ExposureStatus{}, nil
	}
	if err != nil {
		return controlplane.ExposureStatus{}, fmt.Errorf("kube: reading ingress %q: %w", app, err)
	}
	out := controlplane.ExposureStatus{Exposed: true, TLS: len(ing.Spec.TLS) > 0}
	if len(ing.Spec.Rules) > 0 {
		out.Host = ing.Spec.Rules[0].Host
	}
	// The ingress controller writes the assigned external address into the Ingress status.
	for _, lb := range ing.Status.LoadBalancer.Ingress {
		if lb.IP != "" {
			out.Address = lb.IP
			break
		}
		if lb.Hostname != "" {
			out.Address = lb.Hostname
			break
		}
	}
	// When TLS was requested, cert-manager issues the certificate into the Secret named in the
	// Ingress's TLS section. A Secret holding a certificate is the readiness signal the
	// reachability chain reports, so the agent can wait on issuance rather than declare an HTTPS
	// URL live before the certificate exists.
	if name := tlsSecretName(ing); name != "" {
		sec, err := a.client.CoreV1().Secrets(a.namespace).Get(ctx, name, metav1.GetOptions{})
		switch {
		case apierrors.IsNotFound(err):
			// Certificate not issued yet: CertReady stays false.
		case err != nil:
			return controlplane.ExposureStatus{}, fmt.Errorf("kube: reading tls secret %q for %q: %w", name, app, err)
		default:
			out.CertReady = len(sec.Data[corev1.TLSCertKey]) > 0
		}
	}
	return out, nil
}

// tlsSecretName returns the Secret cert-manager populates with the Ingress's certificate, or ""
// when the Ingress requests no TLS.
func tlsSecretName(ing *networkingv1.Ingress) string {
	for _, t := range ing.Spec.TLS {
		if t.SecretName != "" {
			return t.SecretName
		}
	}
	return ""
}

// SecretKeys returns the env-var names in app's per-app Secret, sorted, never the values
// (ADR-0028/0004). A missing Secret is an app with no secrets set: empty slice, no error.
func (a *Adapter) SecretKeys(ctx context.Context, app string) ([]string, error) {
	s, err := a.client.CoreV1().Secrets(a.namespace).Get(ctx, controlplane.AppSecretName(app), metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return []string{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("kube: reading secret for %q: %w", app, err)
	}
	keys := make([]string, 0, len(s.Data))
	for k := range s.Data {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys, nil
}

// SetSecretValue upserts key=value into app's per-app Secret (controlplane.AppSecretName(app)) in
// the app namespace, creating the Secret (Opaque, Burrow labels) if absent (ADR-0029). The value
// arrives here over burrowd's authenticated control-plane API and is written to the Kubernetes
// Secret; it never reaches a log, the audit log, Postgres, or the agent control channel
// (ADR-0029/0004). The returned error names the app and key only, never the value. It retries on
// conflict since a concurrent set/unset can race the resourceVersion.
func (a *Adapter) SetSecretValue(ctx context.Context, app, key, value string) error {
	secrets := a.client.CoreV1().Secrets(a.namespace)
	name := controlplane.AppSecretName(app)
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		existing, err := secrets.Get(ctx, name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			_, err = secrets.Create(ctx, &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      name,
					Namespace: a.namespace,
					Labels:    map[string]string{nameLabel: app, managedByLabel: managedByValue},
				},
				Type: corev1.SecretTypeOpaque,
				Data: map[string][]byte{key: []byte(value)},
			}, metav1.CreateOptions{})
			return err
		}
		if err != nil {
			return err
		}
		if existing.Data == nil {
			existing.Data = map[string][]byte{}
		}
		existing.Data[key] = []byte(value)
		_, err = secrets.Update(ctx, existing, metav1.UpdateOptions{})
		return err
	})
	if err != nil {
		// The error names the app and key only — never the value — so it is safe to log/return.
		return fmt.Errorf("kube: writing secret %q for %q: %w", key, app, err)
	}
	return nil
}

// UnsetSecretKey removes one key from app's per-app Secret (get, delete the key, update). A
// missing Secret or a missing key is a no-op, not an error. The value never crosses here — the
// caller passes only the key name (ADR-0004).
func (a *Adapter) UnsetSecretKey(ctx context.Context, app, key string) error {
	secrets := a.client.CoreV1().Secrets(a.namespace)
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		s, err := secrets.Get(ctx, controlplane.AppSecretName(app), metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return nil // no Secret: nothing to unset
		}
		if err != nil {
			return err
		}
		if _, ok := s.Data[key]; !ok {
			return nil // key already absent
		}
		delete(s.Data, key)
		_, err = secrets.Update(ctx, s, metav1.UpdateOptions{})
		return err
	})
}

// RestartWorkload bumps app's pod-template restarted-at annotation to at, triggering a rolling
// update so a running app picks up a secret change that envFrom reads only at pod start
// (ADR-0028). A missing Deployment is ErrNotFound — nothing running to roll.
func (a *Adapter) RestartWorkload(ctx context.Context, app string, at time.Time) error {
	patch := []byte(fmt.Sprintf(
		`{"spec":{"template":{"metadata":{"annotations":{%q:%q}}}}}`,
		controlplane.RestartedAtAnnotation, at.UTC().Format(time.RFC3339Nano),
	))
	_, err := a.client.AppsV1().Deployments(a.namespace).Patch(ctx, app, types.StrategicMergePatchType, patch, metav1.PatchOptions{})
	if apierrors.IsNotFound(err) {
		return fmt.Errorf("kube: deployment %q: %w", app, controlplane.ErrNotFound)
	}
	if err != nil {
		return fmt.Errorf("kube: restarting deployment %q: %w", app, err)
	}
	return nil
}

// buildService is a ClusterIP Service fronting the app's Pods, forwarding port 80 to the
// app's container port.
func (a *Adapter) buildService(spec controlplane.ExposeSpec) *corev1.Service {
	labels := map[string]string{nameLabel: spec.App, managedByLabel: managedByValue}
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: spec.App, Namespace: a.namespace, Labels: labels},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{nameLabel: spec.App},
			Ports: []corev1.ServicePort{{
				Port:       80,
				TargetPort: intstr.FromInt32(spec.Port),
				Protocol:   corev1.ProtocolTCP,
			}},
		},
	}
}

// buildIngress routes spec.Host to the app's Service on port 80, optionally requesting a
// cert-manager TLS certificate for the host.
func (a *Adapter) buildIngress(spec controlplane.ExposeSpec) *networkingv1.Ingress {
	labels := map[string]string{nameLabel: spec.App, managedByLabel: managedByValue}
	pathType := networkingv1.PathTypePrefix
	meta := metav1.ObjectMeta{Name: spec.App, Namespace: a.namespace, Labels: labels}
	var tls []networkingv1.IngressTLS
	if spec.TLS {
		// cert-manager watches this annotation and issues a cert into the named Secret.
		meta.Annotations = map[string]string{"cert-manager.io/cluster-issuer": spec.Issuer}
		tls = []networkingv1.IngressTLS{{Hosts: []string{spec.Host}, SecretName: spec.App + "-tls"}}
	}
	// Bind the Ingress to the ingress-nginx controller. ingress-nginx runs with
	// --ingress-class=nginx and (by default) ignores Ingresses that carry no class, so without
	// this the app Ingress is orphaned: it never gets an external address and the reachability
	// chain stalls. "nginx" is the class `burrow ingress install` sets up (ADR-0018).
	ingressClass := defaultIngressClass
	return &networkingv1.Ingress{
		ObjectMeta: meta,
		Spec: networkingv1.IngressSpec{
			IngressClassName: &ingressClass,
			TLS:              tls,
			Rules: []networkingv1.IngressRule{{
				Host: spec.Host,
				IngressRuleValue: networkingv1.IngressRuleValue{
					HTTP: &networkingv1.HTTPIngressRuleValue{
						Paths: []networkingv1.HTTPIngressPath{{
							Path:     "/",
							PathType: &pathType,
							Backend: networkingv1.IngressBackend{
								Service: &networkingv1.IngressServiceBackend{
									Name: spec.App,
									Port: networkingv1.ServiceBackendPort{Number: 80},
								},
							},
						}},
					},
				},
			}},
		},
	}
}

func (a *Adapter) buildDeployment(spec controlplane.WorkloadSpec) *appsv1.Deployment {
	labels := map[string]string{nameLabel: spec.App, managedByLabel: managedByValue}
	selector := map[string]string{nameLabel: spec.App}

	var env []corev1.EnvVar
	for _, k := range sortedKeys(spec.Env) { // deterministic order
		env = append(env, corev1.EnvVar{Name: k, Value: spec.Env[k]})
	}

	// A positive MetricsPort annotates the pod template so the metrics add-on's scraper (a
	// vmagent with a Prometheus-style discovery rule) finds and scrapes /metrics on that port
	// (ADR-0026). Zero adds no annotations, so a deploy without it is unchanged.
	var podAnnotations map[string]string
	if spec.MetricsPort > 0 {
		podAnnotations = map[string]string{
			"prometheus.io/scrape": "true",
			"prometheus.io/port":   strconv.Itoa(int(spec.MetricsPort)),
			"prometheus.io/path":   "/metrics",
		}
	}

	// Stamp the release ID on the pod template so every new release rolls the workload, even when
	// the image reference is unchanged (a re-deploy is a new release with a new ID). Merge it with
	// any metrics annotations rather than clobbering them; an empty ReleaseID adds nothing.
	if spec.ReleaseID != "" {
		if podAnnotations == nil {
			podAnnotations = map[string]string{}
		}
		podAnnotations[controlplane.ReleaseAnnotation] = spec.ReleaseID
	}

	// Source every key in the app's per-app Secret as an env var (ADR-0028). optional: true so a
	// workload with no secrets set still applies (the Secret may not exist yet) — the values live
	// only in the Secret, never inlined here. The name is derived from the app, so a deploy,
	// rollback, or env reapply all inject the same Secret without it crossing the API.
	envFrom := []corev1.EnvFromSource{{
		SecretRef: &corev1.SecretEnvSource{
			LocalObjectReference: corev1.LocalObjectReference{Name: controlplane.AppSecretName(spec.App)},
			Optional:             boolPtr(true),
		},
	}}

	replicas := spec.Replicas
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: spec.App, Namespace: a.namespace, Labels: labels},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: selector},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels, Annotations: podAnnotations},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name:    spec.App,
						Image:   spec.Image,
						Command: spec.Command,
						Env:     env,
						EnvFrom: envFrom,
						// Readiness only, and nil when the engine resolved no probe (ADR-0076 §1,
						// §3). LivenessProbe and StartupProbe are deliberately left unset: a wrong
						// liveness probe restarts a working container in a loop and presents as the
						// crash loop it was installed to detect.
						ReadinessProbe: readinessProbe(spec.Readiness),
					}},
				},
			},
		},
	}

	// Apply the ADR-0061 extension point last, over the fully-constructed pod spec, so the hook sees
	// the containers, env, and annotations the engine built and can adjust the pod for the cluster's
	// own requirements (a toleration, a runtime class, a scheduling policy). Because both callers of
	// buildDeployment go through here — the create and the update in ApplyWorkload — the mutation is
	// re-applied on every rollout instead of being dropped by the first one (ADR-0061 §2). No mutator
	// leaves the Deployment exactly as built above (ADR-0061 §3).
	if a.podMutator != nil {
		a.podMutator(&dep.Spec.Template.Spec)
	}
	return dep
}

// readinessProbe translates the engine's resolved ReadinessCheck into the Kubernetes probe, or nil
// when there is none (ADR-0076 §3: an app whose port Burrow does not know keeps exactly the
// behaviour it had before probes existed).
//
// The two handlers here are the ONLY two Burrow ever authors, and both address the pod's own port:
// TCPSocket and HTTPGet with no Host, so the request goes to the pod's IP. There is no Exec handler
// and no way to name another host, which is ADR-0076 §2 enforced in the one place that turns intent
// into a cluster object — a readiness probe that checked the shared database would fail every
// replica of every app the moment that database blipped, converting a degraded dependency into a
// total outage.
//
// The timings come from the constants in controlplane/health.go and are chosen to fail toward
// DEPLOYED (§6): a generous timeout, and six consecutive failures — about a minute — before a
// serving pod is pulled out of its Service.
func readinessProbe(r controlplane.ReadinessCheck) *corev1.Probe {
	if !r.Enabled() {
		return nil
	}
	probe := &corev1.Probe{
		InitialDelaySeconds: controlplane.ReadinessInitialDelaySeconds,
		PeriodSeconds:       controlplane.ReadinessPeriodSeconds,
		TimeoutSeconds:      controlplane.ReadinessTimeoutSeconds,
		FailureThreshold:    controlplane.ReadinessFailureThreshold,
		SuccessThreshold:    controlplane.ReadinessSuccessThreshold,
	}
	if r.HTTP() {
		// Host is left empty on purpose: Kubernetes then addresses the pod's own IP. Setting it is
		// how a probe would reach off the pod, so it is never set.
		probe.HTTPGet = &corev1.HTTPGetAction{Path: r.Path, Port: intstr.FromInt32(r.Port)}
		return probe
	}
	probe.TCPSocket = &corev1.TCPSocketAction{Port: intstr.FromInt32(r.Port)}
	return probe
}

func boolPtr(b bool) *bool { return &b }

func deploymentAvailable(dep *appsv1.Deployment, desired int32) bool {
	for _, c := range dep.Status.Conditions {
		if c.Type == appsv1.DeploymentAvailable {
			return c.Status == corev1.ConditionTrue
		}
	}
	return desired > 0 && dep.Status.ReadyReplicas >= desired
}

// deploymentRolledOut reports whether the Deployment's newest revision is fully rolled out and
// serving, using the same completion test as `kubectl rollout status`
// (deploymentutil.DeploymentComplete). ReadyReplicas alone is insufficient: it counts ready pods
// across BOTH the old and new ReplicaSets, so during a rolling update the old pods keep it
// satisfied while the new pods are wedged. Requiring UpdatedReplicas/AvailableReplicas to reach
// desired and Replicas to equal UpdatedReplicas confirms the new revision is the only one left
// and available. When it returns false the caller inspects the pods and the progress deadline to
// tell a wedged rollout from one still legitimately in progress.
func deploymentRolledOut(dep *appsv1.Deployment, desired int32) bool {
	return desired > 0 &&
		dep.Status.ObservedGeneration >= dep.Generation &&
		dep.Status.UpdatedReplicas >= desired &&
		dep.Status.Replicas == dep.Status.UpdatedReplicas &&
		dep.Status.AvailableReplicas >= desired
}

// deploymentProgressStalled reports whether the Deployment's rollout has stalled past its progress
// deadline: the Progressing condition goes False with reason ProgressDeadlineExceeded when the
// newest revision has not made progress in time. It is the deadline-bounded backstop for a rollout
// wedged for a reason no pod reports — the last resort after the pod inspection, which names the
// fix where this only reports that time ran out. The reason string is a constant in deploymentutil
// and not exported by appsv1, so it is the vocabulary's own member that names it — one spelling for
// the condition Burrow reads and the IssueReason it reports.
func deploymentProgressStalled(dep *appsv1.Deployment) bool {
	for _, c := range dep.Status.Conditions {
		if c.Type == appsv1.DeploymentProgressing {
			return c.Status == corev1.ConditionFalse && c.Reason == controlplane.ReasonProgressDeadlineExceeded
		}
	}
	return false
}

func sortedKeys(m map[string]string) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

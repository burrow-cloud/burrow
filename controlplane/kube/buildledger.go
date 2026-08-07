// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package kube

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	batchv1client "k8s.io/client-go/kubernetes/typed/batch/v1"

	"github.com/burrow-cloud/burrow/controlplane"
)

// The adapter records what each build is for and reads it back, so a build that succeeds after its
// caller has gone is finished rather than discarded (issue #504).
var (
	_ controlplane.AttributedBuilder = (*BuildAdapter)(nil)
	_ controlplane.BuildLedger       = (*BuildAdapter)(nil)
)

// THE BUILD JOB IS THE RECORD, and there is no second one. A build's whole problem (issue #504) is
// that the durable thing — a Kubernetes Job, running on its own — knew nothing about why it had been
// started, so anything that knew died with the request. Writing the intent onto that same object
// fixes it with no new state to keep consistent: the record cannot outlive the build or be orphaned
// by it, it is reaped by exactly the same retention, and it survives a burrowd restart, a burrowd
// redeploy, and a control plane whose database was restored from a backup taken before the build.
//
// A SUCCESSFUL BUILD JOB THAT STILL EXISTS IS THE SIGNAL. The ordinary path reaps a Job the moment
// its build has been used, so the presence of a Complete build Job is not incidental — it is the
// state "this build succeeded and nobody has finished it". That is the state the control plane
// notices; it does not have to infer anything about who was connected or when.
const (
	// buildAppLabel is the app a build's image is destined for. It is a LABEL rather than an
	// annotation so a stranded build can be listed with a selector rather than by reading every Job
	// in the namespace, and its presence is what marks a Job as carrying intent at all.
	buildAppLabel = "burrow.cloud/build-app"
	// buildEnvLabel is the environment the build's deploy targets, always a concrete name.
	buildEnvLabel = "burrow.cloud/build-environment"
	// buildImageAnnotation is the reference the build's deploy pins, WITHOUT the digest — the public
	// host base the built digest is appended to (ADR-0054). It is an annotation because an image
	// reference contains characters (`/`, `:`) a label value may not.
	buildImageAnnotation = "burrow.cloud/build-image"
	// buildHeldAnnotation marks a build whose unattended deploy a guardrail did not allow, with the
	// guardrail code as its value. It stops the same decision being re-recorded on every sweep, and
	// it is cleared whenever someone asks for the build again.
	buildHeldAnnotation = "burrow.cloud/build-held"
)

// buildIntentMetadata renders a BuildIntent as the labels and annotations a build Job carries. A
// zero or incomplete intent renders nothing: a build whose caller recorded no intent is a build that
// cannot be recovered, and half-recorded intent would be worse than none — it would be a build the
// reconciler picks up and cannot finish.
// A VALUE THAT IS NOT A LEGAL LABEL VALUE DROPS THE WHOLE INTENT rather than failing the build. The
// app and the environment reach this as data the caller supplied, and a Job the apiserver rejects
// for a malformed label would turn a build that used to run into a 500 — trading the bug this fixes
// for a worse one. A build that cannot be attributed is simply a build that cannot be recovered,
// which is where every build was before this existed.
func buildIntentMetadata(intent controlplane.BuildIntent) (labels, annotations map[string]string) {
	if intent.App == "" || intent.Env == "" || intent.Image == "" {
		return nil, nil
	}
	if len(validation.IsValidLabelValue(intent.App)) > 0 || len(validation.IsValidLabelValue(intent.Env)) > 0 {
		return nil, nil
	}
	return map[string]string{
			buildAppLabel: intent.App,
			buildEnvLabel: intent.Env,
		}, map[string]string{
			buildImageAnnotation: intent.Image,
		}
}

// stampBuildIntent records intent against an EXISTING build Job — the idempotent re-run path, where
// a Job with this deterministic name is reused rather than created. It also clears any hold marker,
// because a caller asking for this build again has not given up on it.
//
// It is a merge patch rather than a read-modify-write: the intent is the only thing being changed,
// nothing else in the Job depends on its previous value, and a patch cannot lose a concurrent
// update to a field it does not mention.
func (b *BuildAdapter) stampBuildIntent(ctx context.Context, jobs batchv1client.JobInterface, name string, intent controlplane.BuildIntent) error {
	labels, annotations := buildIntentMetadata(intent)
	if len(labels) == 0 {
		return nil
	}
	// A null value removes the key under a JSON merge patch (RFC 7386), which is how the hold is
	// cleared in the same round trip that records the intent.
	patchAnnotations := make(map[string]any, len(annotations)+1)
	for k, v := range annotations {
		patchAnnotations[k] = v
	}
	patchAnnotations[buildHeldAnnotation] = nil
	patch, err := json.Marshal(map[string]any{"metadata": map[string]any{
		"labels":      labels,
		"annotations": patchAnnotations,
	}})
	if err != nil {
		return fmt.Errorf("kube: recording what build job %q is for: %w", name, err)
	}
	if _, err := jobs.Patch(ctx, name, types.MergePatchType, patch, metav1.PatchOptions{}); err != nil {
		return fmt.Errorf("kube: recording what build job %q is for: %w", name, err)
	}
	return nil
}

// StrandedBuilds lists the builds that SUCCEEDED and whose deploy never ran: build Jobs that carry
// intent, have completed successfully, are not already held, finished before completedBefore, and
// still have a readable digest.
//
// Every one of those conditions is doing work. Intent is what makes a build finishable at all.
// Success is what makes it worth finishing — a failed build is left for diagnosis and its own
// retention reaps it. completedBefore is the caller's settling margin against racing the build's
// original caller. And a build whose digest cannot be read has nothing to deploy: reporting it would
// hand the control plane a build it can only fail at, so it is not reported (the ordinary path
// treats the same condition as a build failure rather than pinning a deploy to nothing).
func (b *BuildAdapter) StrandedBuilds(ctx context.Context, completedBefore time.Time) ([]controlplane.StrandedBuild, error) {
	// The selector is the intent label's mere PRESENCE, so this list is empty on a cluster that has
	// never run a build and on the ordinary path where every finished build was reaped by its caller.
	list, err := b.client.BatchV1().Jobs(b.namespace).List(ctx, metav1.ListOptions{LabelSelector: buildAppLabel})
	if err != nil {
		return nil, fmt.Errorf("kube: listing build jobs in %s: %w", b.namespace, err)
	}
	var stranded []controlplane.StrandedBuild
	for i := range list.Items {
		job := &list.Items[i]
		if job.DeletionTimestamp != nil || job.Status.Succeeded == 0 {
			continue
		}
		if _, held := job.Annotations[buildHeldAnnotation]; held {
			continue
		}
		completed := jobCompletedAt(job)
		if completed.IsZero() || !completed.Before(completedBefore) {
			continue
		}
		digest := b.jobTerminationDigest(ctx, job.Name)
		if digest == "" {
			continue
		}
		stranded = append(stranded, controlplane.StrandedBuild{
			ID:          job.Name,
			App:         job.Labels[buildAppLabel],
			Env:         job.Labels[buildEnvLabel],
			Image:       job.Annotations[buildImageAnnotation],
			Digest:      digest,
			CompletedAt: completed,
		})
	}
	return stranded, nil
}

// ReapBuild discards a build there is nothing further to do with, exactly as the ordinary success
// path does: background propagation, so the Job owner goes immediately and its pods and owned
// credential Secret are collected asynchronously. A Job already gone is not an error — the TTL
// controller and this call are both allowed to be the one that removed it.
func (b *BuildAdapter) ReapBuild(ctx context.Context, id string) error {
	policy := metav1.DeletePropagationBackground
	if err := b.client.BatchV1().Jobs(b.namespace).Delete(ctx, id, metav1.DeleteOptions{PropagationPolicy: &policy}); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("kube: reaping build job %q: %w", id, err)
	}
	return nil
}

// HoldBuild marks a build whose unattended deploy a guardrail did not allow. The build is LEFT IN
// PLACE: the image is good and has already been paid for, so a person who re-runs the same build
// with a confirmation reuses it (the deterministic Job name finds it Succeeded and returns its
// digest) instead of paying for the same minutes again. The marker is what stops the guardrail
// decision being re-recorded on every sweep; stampBuildIntent clears it when the build is re-run.
func (b *BuildAdapter) HoldBuild(ctx context.Context, id, reason string) error {
	patch, err := json.Marshal(map[string]any{"metadata": map[string]any{
		"annotations": map[string]string{buildHeldAnnotation: reason},
	}})
	if err != nil {
		return fmt.Errorf("kube: marking build job %q held: %w", id, err)
	}
	if _, err := b.client.BatchV1().Jobs(b.namespace).Patch(ctx, id, types.MergePatchType, patch, metav1.PatchOptions{}); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("kube: marking build job %q held: %w", id, err)
	}
	return nil
}

// jobCompletedAt reads when a Job finished from the cluster's own record of it, never from a clock
// this process keeps. CompletionTime is the direct answer; the Complete condition's transition time
// is the fallback for an apiserver that recorded one without the other.
func jobCompletedAt(job *batchv1.Job) time.Time {
	if job.Status.CompletionTime != nil {
		return job.Status.CompletionTime.Time
	}
	for _, c := range job.Status.Conditions {
		if c.Type == batchv1.JobComplete && !c.LastTransitionTime.IsZero() {
			return c.LastTransitionTime.Time
		}
	}
	return time.Time{}
}

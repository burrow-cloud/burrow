// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package main

import (
	"context"
	"fmt"
	"io"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"

	"github.com/burrow-cloud/burrow/connect"
	"github.com/burrow-cloud/burrow/controlplane/kube"
)

// The two shapes the control plane's own database can be installed in (ADR-0086 §2).
//
// databaseCNPG is the default: a CloudNativePG `Cluster`, the same stack every Postgres add-on
// instance runs on, so there is one set of failure modes, one repair path, and a component that
// knows how to take a backup. databasePlain is the single Deployment Burrow shipped before, kept
// for a cluster whose platform team will not accept cluster-scoped CustomResourceDefinitions.
//
// PLAIN IS A CHOICE, NEVER A FALLBACK. Nothing in this file detects a refusal and quietly installs
// the other one: an install that cannot create the definitions stops and names the flag, so a
// database with no backups is always something a person picked on purpose (ADR-0086 §2).
const (
	databaseCNPG  = "cnpg"
	databasePlain = "plain"
)

// controlPlaneClusterName is the name of the `Cluster` the control-plane database runs as, and of
// the Service that reaches it. It is the name the plain Deployment and its Service already carry,
// which is what lets the connection URL in the `burrowd-db` Secret be identical in both shapes.
const controlPlaneClusterName = "postgres"

// clusterWait is how long the control-plane database's `Cluster` is given to come up, and how it is
// polled. Every field is explicit so a test can drive the wait with tiny durations, deterministically
// and without a real cluster — the same shape burrowd's own database wait uses.
type clusterWait struct {
	// grace is how long the operator gets to put ANY status on the object before the wait calls it a
	// stalled bootstrap rather than a slow one. The operator writes a status within a reconcile or
	// two of the object appearing, so silence past this is not a slow database — it is nothing
	// reconciling the object at all.
	grace time.Duration
	// timeout bounds the whole wait. It is longer than the three minutes the plain Deployment gets
	// because there is more to do: notice the object, provision a volume, pull the PostgreSQL operand
	// image, and run initdb before anything accepts a connection (ADR-0086 Consequences).
	timeout time.Duration
	// poll is the pause between reads.
	poll time.Duration
}

// controlPlaneClusterWait is the production timing.
var controlPlaneClusterWait = clusterWait{grace: 90 * time.Second, timeout: 10 * time.Minute, poll: 2 * time.Second}

// cnpgAPIWait bounds the wait for the API server to start serving CloudNativePG's API group after
// its definitions are applied. It is short because this is establishment, not readiness: the API
// server admits a CustomResourceDefinition and serves it a moment later, and a minute of that is
// already generous.
const cnpgAPIWait = time.Minute

// waitForCloudNativePGAPI blocks until the API server serves the `postgresql.cnpg.io` group, so the
// `Cluster` in the install manifests has something to be validated against.
func waitForCloudNativePGAPI(ctx context.Context, cs kubernetes.Interface, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		found, err := detectCloudNativePGFn(ctx, cs)
		if err == nil && found.Present {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("the API server did not start serving %s within %s after the operator was "+
				"applied, so the control plane's database cannot be created", kube.CNPGAPIGroup, timeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

// cnpgClusterGVR is where `postgresql.cnpg.io/v1 Cluster` lives. It is addressed through the
// dynamic client because Burrow does not import CloudNativePG's Go module — the same reasoning
// controlplane/kube states for the add-on's `Cluster`, and the same posture as the cert-manager
// ClusterIssuer probe in clusterregistry.go.
var cnpgClusterGVR = schema.GroupVersionResource{Group: kube.CNPGAPIGroup, Version: "v1", Resource: "clusters"}

// controlPlaneClusterFn is the seam the `Cluster` readiness wait reads through. It is a package var
// so a test can drive the wait from a fake dynamic client with no cluster behind it.
var controlPlaneClusterFn = func(kubeconfig, kubeContext, namespace string) (dynamic.ResourceInterface, error) {
	cfg, err := connect.RESTConfig(kubeconfig, kubeContext)
	if err != nil {
		return nil, err
	}
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("building dynamic client: %w", err)
	}
	return dyn.Resource(cnpgClusterGVR).Namespace(namespace), nil
}

// validateDatabase resolves the --database value, defaulting an empty one to CloudNativePG. An
// unrecognized value is refused by name rather than silently treated as one of the two: which
// database an install ends up with is the one thing about this flag nobody should have to guess.
func validateDatabase(v string) (string, error) {
	switch v {
	case "":
		return databaseCNPG, nil
	case databaseCNPG, databasePlain:
		return v, nil
	default:
		return "", fmt.Errorf("unknown --database %q: it is either %q (the default: the control plane's "+
			"database runs on CloudNativePG, with backups, failover and point-in-time recovery) or %q "+
			"(a single Deployment with none of those, for a cluster that will not accept cluster-scoped "+
			"CustomResourceDefinitions)", v, databaseCNPG, databasePlain)
	}
}

// writeInstallDatabasePlan states what this install will do about its database BEFORE anything is
// applied (ADR-0086 §5): which of the two shapes it will be, what that shape does and does not
// give, and — for the default — that it creates cluster-scoped CustomResourceDefinitions and takes
// longer. `burrow cluster postgres install` names its own cost the same way, through the same
// plan-row renderer, so the two do not drift.
func writeInstallDatabasePlan(w io.Writer, verbose bool, database string, cnpg cloudNativePGState) {
	if database == databasePlain {
		fmt.Fprintln(w, "The control plane's database will be a plain Deployment (--database plain):")
		writePlanRows(w, verbose, []planRow{{
			name:   "Database",
			detail: "one replica on a 1Gi volume: no backups, no point-in-time recovery, no failover",
		}})
		fmt.Fprintln(w)
		fmt.Fprintln(w, note(w)+"the default runs it on CloudNativePG instead, which archives it to object")
		fmt.Fprintln(w, "  storage once a provider is registered. Add-ons are unaffected either way: this cluster")
		fmt.Fprintln(w, "  can still run `burrow cluster postgres install` later and get Postgres add-ons.")
		return
	}

	fmt.Fprintln(w, "The control plane's database will be a CloudNativePG cluster:")
	writePlanRows(w, verbose, []planRow{
		cloudNativePGInstallPlanRow(cnpg),
		{name: "Database", detail: "a CloudNativePG Cluster: failover, and backups once an " +
			"object-storage provider is registered"},
	})
	fmt.Fprintln(w)
	fmt.Fprintln(w, note(w)+"CloudNativePG installs cluster-scoped CustomResourceDefinitions and cluster RBAC, so")
	fmt.Fprintln(w, "  this install needs cluster-admin on this kube context, and it takes a few minutes longer")
	fmt.Fprintln(w, "  than a plain one. A cluster that will not accept those definitions installs with")
	fmt.Fprintln(w, "  --database plain, which runs the database as a single Deployment with no backups.")
}

// cloudNativePGState is what the install knows about the operator before it starts: whether a
// controller is already running, and which release. It is the subset of the capability report the
// plan row and the install step both read, kept as its own type so the plan cannot be printed from
// one read and the install performed from another.
type cloudNativePGState struct {
	ready   bool
	version string
}

// cloudNativePGInstallPlanRow describes the operator's line in the install plan: skipped when a
// controller is already running (and the release named, since an older operator validates a
// different schema), otherwise the release this run will apply.
func cloudNativePGInstallPlanRow(c cloudNativePGState) planRow {
	switch {
	case c.ready && c.version == "":
		return planRow{name: "CloudNativePG", detail: "already running, version unknown; skipped"}
	case c.ready && c.version != kube.CNPGVersion:
		return planRow{name: "CloudNativePG", version: c.version,
			detail: "already running; skipped (Burrow targets " + kube.CNPGVersion + ")"}
	case c.ready:
		return planRow{name: "CloudNativePG", version: c.version, detail: "already running; skipped"}
	default:
		return planRow{name: "CloudNativePG", version: kube.CNPGVersion, url: kube.CNPGManifestURL(kube.CNPGVersion),
			detail: "installed first; runs and manages the database"}
	}
}

// errCloudNativePGRequired turns a failed operator install into the stop ADR-0086 §2 asks for: the
// install ends here, says that it did NOT install something else instead, and names the flag that
// exists for a cluster which will not accept the definitions.
//
// Naming the flag is the whole point. The alternative — detecting the refusal and installing the
// plain database — reads as helpful and hands somebody a working install, a success message, and no
// backups, with nothing afterwards saying which of the two they got.
func errCloudNativePGRequired(kubeContext string, err error) error {
	return fmt.Errorf("installing CloudNativePG, which the control plane's database runs on: %w. "+
		"Nothing else was installed in its place: the install stops here rather than quietly giving you a "+
		"database with no backups. Creating cluster-scoped CustomResourceDefinitions needs cluster-admin on "+
		"this kube context. If this cluster will not accept them, install the plain database deliberately: "+
		"burrow cluster install %s --database plain", err, kubeContext)
}

// waitForControlPlaneCluster blocks until the CloudNativePG `Cluster` behind the control-plane
// database has an instance serving, and otherwise says WHICH stage did not finish.
//
// Three things can go wrong here and they have three different fixes, so the wait distinguishes
// them rather than reporting one timeout:
//
//   - The object never appeared. The apply did not land, so there is nothing to reconcile.
//   - The object is there and the operator has written no status at all. Nothing is reconciling it:
//     the controller is down, wedged, or watching an API version this object is not.
//   - The operator is reconciling and PostgreSQL is not serving yet. The phase and the latest
//     condition are reported, because that is where the reason lives (no volume could be bound, the
//     operand image will not pull, initdb failed).
//
// Readiness is `status.readyInstances`, not `status.phase` — the same signal controlplane/kube
// reads for an add-on instance. The phase is a human-facing string CloudNativePG is free to change
// and passes through several healthy-but-not-serving values on the way up; the ready count is a
// number with one meaning.
func waitForControlPlaneCluster(ctx context.Context, ri dynamic.ResourceInterface, namespace string, out io.Writer, w clusterWait) error {
	fmt.Fprintf(out, "  database ...")
	start := time.Now()
	deadline := start.Add(w.timeout)
	var (
		lastErr        error
		seen           bool
		statusObserved bool
		phase          string
		condition      string
	)
	for {
		u, err := ri.Get(ctx, controlPlaneClusterName, metav1.GetOptions{})
		switch {
		case err == nil:
			seen = true
			lastErr = nil
			if p, ok := clusterStatusSummary(u); ok {
				statusObserved = true
				phase, condition = p.phase, p.condition
			}
			if clusterReady(u) {
				fmt.Fprintln(out, " "+okMark(out))
				return nil
			}
		case apierrors.IsNotFound(err):
			lastErr = nil
		default:
			lastErr = err
		}

		// A `Cluster` that is present but has no status after the grace period is not a slow
		// database — it is an object nothing is reconciling, and waiting out the full timeout would
		// only delay saying so.
		if seen && !statusObserved && time.Since(start) > w.grace {
			fmt.Fprintln(out, " "+failMark(out))
			return errClusterNotReconciling(namespace, w.grace)
		}
		if time.Now().After(deadline) {
			fmt.Fprintln(out, " "+failMark(out)+" timed out")
			return errClusterWaitTimeout(namespace, w, seen, statusObserved, phase, condition, lastErr)
		}
		select {
		case <-ctx.Done():
			fmt.Fprintln(out, " "+failMark(out))
			return ctx.Err()
		case <-time.After(w.poll):
		}
	}
}

// clusterStatus is the part of a `Cluster` status the wait reports on failure: the phase
// CloudNativePG is in and the most recent condition message, which is where the reason a bootstrap
// is stuck actually shows up.
type clusterStatus struct {
	phase     string
	condition string
}

// clusterStatusSummary reads that status, reporting false when the operator has not written one
// yet — the signal that separates "bootstrapping slowly" from "nothing is reconciling this".
func clusterStatusSummary(u *unstructured.Unstructured) (clusterStatus, bool) {
	status, found, err := unstructured.NestedMap(u.Object, "status")
	if err != nil || !found || len(status) == 0 {
		return clusterStatus{}, false
	}
	out := clusterStatus{}
	if phase, ok, _ := unstructured.NestedString(u.Object, "status", "phase"); ok {
		out.phase = phase
	}
	conditions, ok, _ := unstructured.NestedSlice(u.Object, "status", "conditions")
	if ok {
		for _, c := range conditions {
			m, ok := c.(map[string]any)
			if !ok {
				continue
			}
			if msg, _ := m["message"].(string); msg != "" {
				out.condition = msg
			}
		}
	}
	return out, true
}

// clusterReady reports whether the `Cluster` has an instance serving.
func clusterReady(u *unstructured.Unstructured) bool {
	ready, found, err := unstructured.NestedInt64(u.Object, "status", "readyInstances")
	return err == nil && found && ready > 0
}

// errClusterNotReconciling is the stalled-bootstrap stop: the object exists and the operator has
// not touched it. The fix is on the operator, so that is where the message points.
func errClusterNotReconciling(namespace string, grace time.Duration) error {
	return fmt.Errorf("the control plane's database was created but the CloudNativePG operator has not "+
		"started reconciling it: the Cluster %s/%s still has no status after %s. The operator is not "+
		"watching it — check its controller with `kubectl -n %s logs deploy/%s`",
		namespace, controlPlaneClusterName, grace, kube.CNPGNamespace, kube.CNPGControllerDeployment)
}

// errClusterWaitTimeout names which of the remaining stages ran out of time, and carries the
// operator's own phase and condition when there is one — the reason a bootstrap is stuck (an
// unbound volume, an operand image that will not pull, a failed initdb) is written there, not here.
func errClusterWaitTimeout(namespace string, w clusterWait, seen, statusObserved bool, phase, condition string, lastErr error) error {
	switch {
	case lastErr != nil:
		return fmt.Errorf("reading the control plane's database (the Cluster %s/%s) after %s: %w",
			namespace, controlPlaneClusterName, w.timeout, lastErr)
	case !seen:
		return fmt.Errorf("the control plane's database did not appear within %s: there is no Cluster "+
			"%s/%s, so the install manifests did not land. Re-run `burrow cluster install`",
			w.timeout, namespace, controlPlaneClusterName)
	case !statusObserved:
		return errClusterNotReconciling(namespace, w.grace)
	default:
		msg := fmt.Sprintf("the control plane's database did not start accepting connections within %s: "+
			"CloudNativePG is reconciling the Cluster %s/%s but no instance is serving yet",
			w.timeout, namespace, controlPlaneClusterName)
		if phase != "" {
			msg += fmt.Sprintf(", and reports phase %q", phase)
		}
		if condition != "" {
			msg += fmt.Sprintf(" (%s)", condition)
		}
		return fmt.Errorf("%s. See what it is waiting on with `kubectl -n %s describe cluster %s`",
			msg, namespace, controlPlaneClusterName)
	}
}

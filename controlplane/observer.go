// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package controlplane

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"time"
)

// DefaultObserveInterval is how often the observer sweeps what Burrow manages. A minute is short
// enough that a crash loop is recorded while it is happening and long enough that the cost is a
// handful of list calls per minute against the API server, scaling with what Burrow manages rather
// than with usage (ADR-0074's consequences). Unlike the auto-deploy poller's cadence it needs no
// jitter: the thing being called is the cluster's own API server, not a third party with a rate
// limit, and there is one burrowd per cluster rather than a fleet that could fall into lockstep.
const DefaultObserveInterval = time.Minute

// DefaultLedgerRetention is how long a RESOLVED failure and an elapsed observation window are kept.
// ADR-0074 §4 requires the bound and does not fix its size: a month is long enough to answer "has
// this happened before" across a release cycle, and short enough that the table cannot become the
// reason the control plane's own database fills up — which is an outage, not untidiness. An ACTIVE
// failure is never pruned, however old: a thing that is still broken is not history.
const DefaultLedgerRetention = 30 * 24 * time.Hour

// DefaultLedgerPruneInterval is how often retention is enforced. Pruning every sweep would spend a
// DELETE per minute to remove nothing; hourly keeps the table bounded without the cost.
const DefaultLedgerPruneInterval = time.Hour

// pendingBackupGrace is how old a `pending` backup row must be before a missing Job is read as the
// Job having disappeared rather than as one that has not been created yet (ADR-0074 §6). It is
// comfortably longer than the backup Job's own deadline, so a backup that is simply slow is never
// reported as an orphan — the failure being looked for is a row left pending by a burrowd that
// restarted mid-backup, which no amount of waiting resolves.
const pendingBackupGrace = 30 * time.Minute

// ObserverConfig configures the failure observer. The zero value is valid: every field falls back to
// a documented default, so cmd/burrowd can start it with an empty config and a test can drive it
// deterministically by supplying Interval and After.
type ObserverConfig struct {
	// Interval is the sweep cadence. Zero or negative applies DefaultObserveInterval.
	Interval time.Duration
	// Retention is how long resolved failures and elapsed windows are kept. Zero applies
	// DefaultLedgerRetention; negative disables pruning, which is a deliberate operator choice and
	// not the default (ADR-0074 §4 requires the bound).
	Retention time.Duration
	// PruneInterval is how often retention is enforced. Zero applies DefaultLedgerPruneInterval.
	PruneInterval time.Duration
	// After returns a channel that fires after d — the seam the run loop waits on between sweeps.
	// Nil applies time.After. It is injected (rather than reading the wall clock) so a test drives
	// the loop with no real sleeping and no ambient time (ADR-0010).
	After func(d time.Duration) <-chan time.Time
}

// Observer watches the workloads the registry says Burrow owns and writes what breaks to the ledger
// (ADR-0074 §3–§7). It is the first thing in Burrow that runs when nobody asked it to, and the
// reason burrowd is no longer purely request/response.
//
// WHAT IT WATCHES IS BOUNDED BY THE REGISTRY, NOT BY A NAMESPACE OR A LABEL (ADR-0074 §3). That is
// what makes §6 expressible at all: a label selector can only find things that exist, and the
// interesting failure is the thing that does not — a registered app with no Deployment, an add-on
// with no running pod, a pending backup whose Job is gone, an exposure with no Ingress. Each is an
// absence, visible only from the side that knows what was intended.
//
// IT IS READ-ONLY AGAINST THE CLUSTER AND MUST STAY THAT WAY (ADR-0074 §9). Every cluster call it
// makes is a read — ListWorkloads, AddonReady, BackupJobPresent, ExposureStatus — and it takes no
// corrective action of any kind. This is stated because the next step is obvious and wrong by
// default: once something notices a crash loop, restarting it looks like a small addition. It is
// not. It is a mutation performed with nobody present, which is the exact shape every guardrail in
// Burrow exists to gate, and the remedies for the failures that most invite it are usually wrong —
// restarting an OOM-killed pod without changing its limit reproduces the OOM. Remediation, if it is
// ever wanted, is its own record with its own guardrail dispositions.
//
// It does NOT infer causes. It records what it observed, completely, in a shape something else can
// reason over; turning twenty rows into "the node pool was tainted at 02:14" is the agent's half of
// the work, and an inference engine inside burrowd is explicitly out of scope (ADR-0074 §5).
//
// It SAMPLES on an interval rather than holding an informer. What ADR-0074 §3 requires of it is that
// the set it looks at be bounded by the registry, and a sweep over that set satisfies it through the
// same read seams every other surface uses — so a ledger row and a status read cannot disagree, and
// the whole thing is deterministic against a fake. The cost is resolution: a failure shorter than
// the interval can pass unseen. That is a real limit, and it is why the coverage record below says
// how often it looked rather than implying it was watching continuously. Moving to a watch later
// changes this file and nothing about the ledger's shape.
type Observer struct {
	engine        *Engine
	interval      time.Duration
	retention     time.Duration
	pruneInterval time.Duration
	after         func(d time.Duration) <-chan time.Time

	// window is the observation window this run of the observer is extending, 0 until the first
	// sweep opens one. It is per-PROCESS and never read back from the store: a restart begins a new
	// window, so the gap while burrowd was down stays a gap rather than being absorbed into an
	// unbroken stretch of claimed coverage.
	window int64
	// lastPrune is when retention was last enforced, zero until the first pass.
	lastPrune time.Time
}

// NewObserver builds the observer bound to this engine. It reads the engine's Kubernetes, database
// and clock seams and writes only to the database. Constructing it does not start it — call Run.
func (e *Engine) NewObserver(cfg ObserverConfig) *Observer {
	interval := cfg.Interval
	if interval <= 0 {
		interval = DefaultObserveInterval
	}
	retention := cfg.Retention
	if retention == 0 {
		retention = DefaultLedgerRetention
	}
	pruneInterval := cfg.PruneInterval
	if pruneInterval <= 0 {
		pruneInterval = DefaultLedgerPruneInterval
	}
	after := cfg.After
	if after == nil {
		after = time.After
	}
	return &Observer{engine: e, interval: interval, retention: retention, pruneInterval: pruneInterval, after: after}
}

// Run observes until ctx is cancelled. It sweeps immediately, then waits the interval before the
// next sweep, honoring cancellation promptly. A sweep never returns an error to the loop: an
// observer that stopped on the first cluster hiccup would produce exactly the silent gap the
// coverage record exists to expose.
func (o *Observer) Run(ctx context.Context) {
	slog.InfoContext(ctx, "failure observer started", "interval", o.interval, "retention", o.retention)
	for {
		o.observe(ctx)
		select {
		case <-ctx.Done():
			slog.InfoContext(ctx, "failure observer stopped", "reason", ctx.Err())
			return
		case <-o.after(o.interval):
		}
	}
}

// sweep accumulates one pass over everything Burrow manages before any of it is written, so the
// ledger sees a consistent picture: the failures found, the objects that could not be read, and
// whether the pass was complete.
type sweep struct {
	// at is the single instant every observation in this pass is stamped with, read once from the
	// injected clock. One timestamp per sweep is what makes "thirty rows in one minute" legible as
	// the one cluster-level event it is (ADR-0074 §5) rather than a smear across a sweep's duration.
	at time.Time
	// found are the failures observed active in this pass.
	found []FailureObservation
	// skipped are the objects this pass could not read. Their rows are left exactly as they are:
	// a failure Burrow could not check is not a failure that recovered.
	skipped []ObjectRef
	// degraded is the most recent reason this pass was incomplete, empty when it read everything.
	degraded string
}

// record adds one observed failure. A reason outside the closed vocabulary is dropped rather than
// stored: the ledger's consumer branches on the reason, so an unvetted one is worse than none.
func (s *sweep) record(ref ObjectRef, reason, detail string) {
	if !IsLedgerReason(reason) {
		return
	}
	s.found = append(s.found, FailureObservation{Object: ref, Reason: reason, Detail: LedgerDetail(detail), At: s.at})
}

// skip marks an object as unread this pass and records why the pass is degraded.
func (s *sweep) skip(ref ObjectRef, reason string) {
	s.skipped = append(s.skipped, ref)
	s.degraded = LedgerDetail(reason)
}

// keys returns the identity of every failure found, for the resolution pass.
func (s *sweep) keys() []FailureKey {
	out := make([]FailureKey, 0, len(s.found))
	for _, f := range s.found {
		out = append(out, FailureKey{Object: f.Object, Reason: f.Reason})
	}
	return out
}

// observe runs one full pass: enumerate what the registry says Burrow owns, compare it against the
// cluster, write what broke, close what recovered, extend the coverage record, and enforce retention.
//
// A failure to ENUMERATE aborts the pass without resolving anything and without advancing coverage.
// Every enumeration is a read of the control plane's own database, so a failure means the database
// is unavailable — and a pass that resolved rows on the strength of an intent list it could not read
// would report a cluster-wide recovery that did not happen. Not advancing coverage is the honest
// consequence: the observer is running but is not observing, and that reads as a gap, which is what
// it is.
func (o *Observer) observe(ctx context.Context) {
	sw := &sweep{at: o.engine.clock.Now()}
	for _, step := range []struct {
		what string
		fn   func(context.Context, *sweep) error
	}{
		{"apps", o.observeApps},
		{"add-ons", o.observeAddons},
		{"backups", o.observeBackups},
		{"exposures", o.observeExposures},
	} {
		if err := step.fn(ctx, sw); err != nil {
			slog.WarnContext(ctx, "observation sweep aborted: could not enumerate what Burrow manages",
				"objects", step.what, "error", err)
			return
		}
		if ctx.Err() != nil {
			return
		}
	}

	for _, obs := range sw.found {
		if err := o.engine.db.RecordFailure(ctx, obs); err != nil {
			slog.WarnContext(ctx, "recording a failure in the ledger failed",
				"object", obs.Object, "reason", obs.Reason, "error", err)
			sw.degraded = "a failure observed this sweep could not be written to the ledger"
		}
	}
	if err := o.engine.db.ResolveFailures(ctx, sw.at, sw.keys(), sw.skipped); err != nil {
		slog.WarnContext(ctx, "resolving recovered failures failed", "error", err)
		sw.degraded = "recovered failures could not be closed this sweep"
	}

	o.extendCoverage(ctx, sw, o.engine.clock.Now())
	o.prune(ctx, sw.at)
}

// observeApps compares the apps the registry records against the workloads the cluster has, one
// listing per environment rather than one read per app: the listing is what the status surface
// already uses, so a blocking condition lands in the ledger under exactly the reason a user would
// have read off `burrow app status` (ADR-0074 §2's vocabulary, unchanged).
func (o *Observer) observeApps(ctx context.Context, sw *sweep) error {
	apps, err := o.engine.db.ManagedApps(ctx)
	if err != nil {
		return err
	}
	byEnv := make(map[string][]string)
	var envs []string
	for _, ref := range apps {
		env := envName(ref.Env)
		if _, ok := byEnv[env]; !ok {
			envs = append(envs, env)
		}
		byEnv[env] = append(byEnv[env], ref.App)
	}
	sort.Strings(envs)

	for _, env := range envs {
		refs := make([]ObjectRef, 0, len(byEnv[env]))
		for _, app := range byEnv[env] {
			refs = append(refs, ObjectRef{Kind: FailureApp, Name: app, Environment: env})
		}
		ns, err := o.engine.resolveNamespace(ctx, env)
		if err != nil {
			skipAll(sw, refs, fmt.Sprintf("environment %q could not be resolved to a namespace", env))
			continue
		}
		statuses, err := o.engine.k8s.WithNamespace(ns).ListWorkloads(ctx)
		if err != nil {
			skipAll(sw, refs, fmt.Sprintf("the workloads in namespace %q could not be listed", ns))
			continue
		}
		running := make(map[string]WorkloadStatus, len(statuses))
		for _, ws := range statuses {
			running[ws.App] = ws
		}
		for _, ref := range refs {
			ws, ok := running[ref.Name]
			if !ok {
				// The §6 diagnosis kubectl structurally cannot make: the evidence is an absence, and
				// only the side that knows what was intended can see it.
				sw.record(ref, ReasonWorkloadMissing,
					"the registry records a rolled-out release for this app, but the cluster has no workload for it")
				continue
			}
			// The live surface already ranked the pod's several reasons down to the one that names
			// the cause (issues.go); the ledger records that reason rather than ranking again, so a
			// row and a status read can never disagree about one pod.
			sw.record(ref, ws.IssueReason, ws.Issue)
		}
	}
	return nil
}

// observeAddons checks each add-on the registry records as INSTALLED against its backing workload.
// A connected add-on is deliberately skipped: it is a backend the user runs, so its readiness is
// not a thing Burrow intended into existence and not a failure Burrow can honestly attribute.
func (o *Observer) observeAddons(ctx context.Context, sw *sweep) error {
	addons, err := o.engine.db.Addons(ctx)
	if err != nil {
		return err
	}
	for _, a := range addons {
		if a.Mode != "installed" {
			continue
		}
		ref := ObjectRef{Kind: FailureAddon, Name: a.Name, Environment: a.Environment}
		ready, err := o.engine.k8s.AddonReady(ctx, a.Name)
		if err != nil {
			sw.skip(ref, fmt.Sprintf("the readiness of add-on %q could not be read", a.Name))
			continue
		}
		if !ready {
			sw.record(ref, ReasonAddonNotRunning,
				"the registry records this add-on as installed, but its workload is not available")
		}
	}
	return nil
}

// observeBackups looks for the shape a backup interrupted by a burrowd restart leaves behind: a row
// still `pending` whose Job no longer exists (ADR-0074 §6). Without it, a backup that never ran is
// indistinguishable from one still running — and since a backup is the thing consulted only when
// something else has already gone wrong, that is the worst possible moment to find out.
func (o *Observer) observeBackups(ctx context.Context, sw *sweep) error {
	pending, err := o.engine.db.PendingBackups(ctx, sw.at.Add(-pendingBackupGrace))
	if err != nil {
		return err
	}
	for _, b := range pending {
		ref := ObjectRef{Kind: FailureBackup, Name: b.ID, Environment: b.Environment}
		present, err := o.engine.k8s.BackupJobPresent(ctx, b.ID)
		if err != nil {
			sw.skip(ref, fmt.Sprintf("the backup Job for %q could not be read", b.ID))
			continue
		}
		if !present {
			sw.record(ref, ReasonBackupJobMissing, fmt.Sprintf(
				"this backup of %q has been pending since %s and its Job no longer exists, so it will never complete",
				b.App, b.CreatedAt.UTC().Format(time.RFC3339)))
		}
	}
	return nil
}

// observeExposures checks each recorded exposure against the cluster: the Ingress that routes the
// host, and — when the exposure asked for TLS — whether the certificate was ever issued. Both are
// absences the cluster cannot report, because nothing is left to report them.
//
// The intent it reads is written by Expose and removed by Unexpose, so it covers exposures made
// through Burrow. An app exposed before that intent was recorded gains a row the next time it is
// exposed; the observer deliberately does not adopt an Ingress it finds into the registry, because
// an adoption racing an unexpose would invent an intent nobody expressed and then report it broken.
func (o *Observer) observeExposures(ctx context.Context, sw *sweep) error {
	exposures, err := o.engine.db.Exposures(ctx)
	if err != nil {
		return err
	}
	for _, ex := range exposures {
		ref := ObjectRef{Kind: FailureExposure, Name: ex.App, Environment: ex.Environment}
		ns, err := o.engine.resolveNamespace(ctx, ex.Environment)
		if err != nil {
			sw.skip(ref, fmt.Sprintf("environment %q could not be resolved to a namespace", ex.Environment))
			continue
		}
		status, err := o.engine.k8s.WithNamespace(ns).ExposureStatus(ctx, ex.App)
		if err != nil {
			sw.skip(ref, fmt.Sprintf("the exposure of %q could not be read", ex.App))
			continue
		}
		switch {
		case !status.Exposed:
			sw.record(ref, ReasonIngressMissing, fmt.Sprintf(
				"the registry records this app as exposed at %s, but the cluster has no Ingress routing it", ex.Host))
		case ex.TLS && !status.CertReady:
			sw.record(ref, ReasonCertificateNotIssued, fmt.Sprintf(
				"the Ingress for %s is present but its TLS certificate has not been issued", ex.Host))
		}
	}
	return nil
}

// extendCoverage records that this sweep happened, so a gap in the ledger is distinguishable from a
// period in which nothing broke. The first sweep of a process opens a window; every later one
// extends it. A window that has gone (pruned by retention, or a database restored from a backup)
// causes the next sweep to open a fresh one rather than silently stop recording coverage — the one
// failure this surface may not have.
func (o *Observer) extendCoverage(ctx context.Context, sw *sweep, until time.Time) {
	if o.window == 0 {
		id, err := o.engine.db.StartObservationWindow(ctx, sw.at)
		if err != nil {
			slog.WarnContext(ctx, "recording observation coverage failed", "error", err)
			return
		}
		o.window = id
	}
	err := o.engine.db.ExtendObservationWindow(ctx, o.window, until, sw.degraded)
	if errors.Is(err, ErrNotFound) {
		o.window = 0
		return
	}
	if err != nil {
		slog.WarnContext(ctx, "extending observation coverage failed", "error", err)
	}
}

// prune enforces the retention bound ADR-0074 §4 requires. It runs on its own cadence rather than
// every sweep, and only ever removes RESOLVED failures and elapsed windows: an active failure is
// not history, however long it has been going on.
func (o *Observer) prune(ctx context.Context, now time.Time) {
	if o.retention < 0 {
		return
	}
	if !o.lastPrune.IsZero() && now.Sub(o.lastPrune) < o.pruneInterval {
		return
	}
	o.lastPrune = now
	res, err := o.engine.db.PruneLedger(ctx, now.Add(-o.retention))
	if err != nil {
		slog.WarnContext(ctx, "pruning the failure ledger failed", "error", err)
		return
	}
	if res.Failures > 0 || res.Windows > 0 {
		slog.InfoContext(ctx, "pruned the failure ledger past its retention window",
			"retention", o.retention, "failures", res.Failures, "windows", res.Windows)
	}
}

// skipAll marks every object in refs unread this sweep for one shared reason — a namespace that
// could not be resolved or listed affects each app in it identically.
func skipAll(sw *sweep, refs []ObjectRef, reason string) {
	for _, ref := range refs {
		sw.skip(ref, reason)
	}
}

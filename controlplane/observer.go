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

// DefaultObserveInterval is how often the observer runs its PERIODIC pass. It is no longer the
// ledger's resolution: a workload failure is recorded when the cluster reports it plus its dwell
// (ADR-0079 §1–§3), and this cadence bounds only the three things a watch cannot do — notice an
// environment that has appeared, advance a standing failure's last_seen, and compare the registry
// against the cluster for the ABSENCES no controller emits an event for (ADR-0074 §6).
//
// ADR-0079 deliberately leaves that comparison a periodic pass: folding it into the watch would
// decide §6's mechanism as a side effect of deciding §3's. A minute is what it has always been, and
// at this cadence the cost is a handful of list calls per minute, scaling with what Burrow manages
// rather than with usage. Unlike the auto-deploy poller's cadence it needs no jitter: the thing being
// called is the cluster's own API server, not a third party with a rate limit, and there is one
// burrowd per cluster rather than a fleet that could fall into lockstep.
const DefaultObserveInterval = time.Minute

// DefaultLedgerRetention is how long a RESOLVED failure and an elapsed observation window are kept.
// ADR-0074 §4 requires the bound and does not fix its size: a month is long enough to answer "has
// this happened before" across a release cycle, and short enough that the table cannot become the
// reason the control plane's own database fills up — which is an outage, not untidiness. An ACTIVE
// failure is never pruned, however old: a thing that is still broken is not history.
const DefaultLedgerRetention = 30 * 24 * time.Hour

// DefaultLedgerPruneInterval is how often retention is enforced. Pruning every pass would spend a
// DELETE per minute to remove nothing; hourly keeps the table bounded without the cost.
const DefaultLedgerPruneInterval = time.Hour

// observerEventBuffer is how many watch events may be in flight before a watch is made to wait. It
// is generous because the alternative to waiting is dropping, and a dropped event leaves the latch
// believing a condition that has cleared — the one failure the latch may not have. A watch that
// stalls long enough loses its place with the API server, which surfaces as a re-list and a
// coverage gap: slower, and honest about being slower.
const observerEventBuffer = 256

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
	// Interval is the periodic pass's cadence. Zero or negative applies DefaultObserveInterval.
	Interval time.Duration
	// Retention is how long resolved failures and elapsed windows are kept. Zero applies
	// DefaultLedgerRetention; negative disables pruning, which is a deliberate operator choice and
	// not the default (ADR-0074 §4 requires the bound).
	Retention time.Duration
	// PruneInterval is how often retention is enforced. Zero applies DefaultLedgerPruneInterval.
	PruneInterval time.Duration
	// After returns a channel that fires after d — the seam the run loop waits on. Nil applies
	// time.After. It is injected (rather than reading the wall clock) so a test drives the loop with
	// no real sleeping and no ambient time (ADR-0010), which matters more now than it did for a
	// sweep: the loop's wake-up is what makes a dwell expire on time.
	After func(d time.Duration) <-chan time.Time
}

// Observer watches the workloads the registry says Burrow owns and writes what breaks to the ledger
// (ADR-0074 §3–§7, ADR-0079). It is the first thing in Burrow that runs when nobody asked it to, and
// the reason burrowd is no longer purely request/response.
//
// WHAT IT WATCHES IS BOUNDED BY THE REGISTRY, NOT BY A NAMESPACE OR A LABEL (ADR-0074 §3, unchanged
// by ADR-0079 §1). That is what makes §6 expressible at all: a label selector can only find things
// that exist, and the interesting failure is the thing that does not — a registered app with no
// Deployment, an add-on with no running pod, a pending backup whose Job is gone, an exposure with no
// Ingress. Each is an absence, visible only from the side that knows what was intended.
//
// IT IS READ-ONLY AGAINST THE CLUSTER AND MUST STAY THAT WAY (ADR-0074 §9). Every cluster call it
// makes is a read — WatchWorkloads, ListWorkloads, AddonReady, BackupJobPresent, ExposureStatus —
// and it takes no corrective action of any kind. This is stated because the next step is obvious and
// wrong by default: once something notices a crash loop, restarting it looks like a small addition.
// It is not. It is a mutation performed with nobody present, which is the exact shape every guardrail
// in Burrow exists to gate, and the remedies for the failures that most invite it are usually wrong —
// restarting an OOM-killed pod without changing its limit reproduces the OOM. Remediation, if it is
// ever wanted, is its own record with its own guardrail dispositions.
//
// It does NOT infer causes. It records what it observed, completely, in a shape something else can
// reason over; turning twenty rows into "the node pool was tainted at 02:14" is the agent's half of
// the work, and an inference engine inside burrowd is explicitly out of scope (ADR-0074 §5).
//
// IT RUNS TWO MECHANISMS, AND ADR-0079 IS WHY THEY ARE TWO. A WATCH reports what the cluster says
// about a workload as it says it, and every such report is latched — held for a dwell before it opens
// a row and for a dwell after it clears before that row is closed (§2) — because Kubernetes status is
// a stream of edges, most of which mean nothing, and an unlatched watch produces the event stream
// ADR-0074 §4 rejected on its merits. A PERIODIC PASS keeps ADR-0074 §6's registry-versus-cluster
// comparison, which is a comparison rather than an event and which ADR-0079 explicitly declines to
// re-decide, and re-affirms the rows the watch still believes open so a standing failure's last_seen
// keeps moving.
type Observer struct {
	engine        *Engine
	interval      time.Duration
	retention     time.Duration
	pruneInterval time.Duration
	after         func(d time.Duration) <-chan time.Time

	// events is the single channel every namespace watch delivers on. The observer owns it rather
	// than merging a channel per watch, so an event is handled on the observer's own goroutine, in
	// the order the cluster produced it, and a test can drive the whole path without racing a
	// scheduler.
	events chan WorkloadEvent
	// watches are the established watches, by namespace. An entry with no cancel is a namespace whose
	// watch could not be established; it is kept so the next pass retries it AND so coverage knows
	// the observer is not, in fact, watching everything.
	watches map[string]*workloadWatch
	// latch holds every transition seen but not yet recorded and every row recorded but not yet
	// resolved (ADR-0079 §2). It is bounded by the managed set: the periodic pass drops entries for
	// objects the registry no longer records, and an entry that clears before it ever opened is
	// deleted rather than kept.
	latch map[FailureKey]*latched
	// limits is the operational configuration the dwells are read from, refreshed once per periodic
	// pass rather than per event. A read per event would put a database call behind every pod
	// transition in the cluster; one pass is the same freshness ListWorkloads gives the grace it
	// resolves once per listing, and it is what makes `cluster config set` take effect without
	// restarting burrowd.
	limits OperationalConfig

	// window is the observation window this run of the observer is extending, 0 when none is open.
	// It is per-PROCESS and never read back from the store: a restart begins a new window, so the gap
	// while burrowd was down stays a gap rather than being absorbed into an unbroken stretch of
	// claimed coverage. A dropped watch closes it for the same reason (ADR-0079 §4).
	window int64
	// lastPrune is when retention was last enforced, zero until the first pass.
	lastPrune time.Time
	// nextPass is when the periodic pass is next due, zero until the first one has run.
	nextPass time.Time
}

// workloadWatch is one namespace's watch as the observer sees it.
type workloadWatch struct {
	// env is the environment this namespace holds, so an event can be turned into an ObjectRef.
	env string
	// cancel stops the watch; nil while the watch could not be established.
	cancel context.CancelFunc
	// synced reports whether the watch has a complete current picture of its namespace. It is false
	// from establishment until the first WorkloadSynced and again from every WorkloadDropped, and
	// while it is false this observer is not covering that namespace (ADR-0079 §4).
	synced bool
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
	return &Observer{
		engine:        e,
		interval:      interval,
		retention:     retention,
		pruneInterval: pruneInterval,
		after:         after,
		events:        make(chan WorkloadEvent, observerEventBuffer),
		watches:       make(map[string]*workloadWatch),
		latch:         make(map[FailureKey]*latched),
	}
}

// Run observes until ctx is cancelled: it runs the periodic pass when one is due, acts on every
// latched transition whose dwell has elapsed, and then sleeps until whichever of an event, the next
// dwell deadline, or the next pass comes first.
//
// SLEEPING UNTIL THE NEXT DWELL DEADLINE IS WHAT MAKES A DWELL A DWELL. Waking only on the pass
// would round every dwell up to the interval, which is the imprecision ADR-0079 exists to remove —
// a thirty-second grace has to open its row thirty seconds later, not at the top of the next minute.
// The deadline is arithmetic on the injected clock, not a timer somebody armed, so a test drives the
// same code path with no wall time at all (ADR-0010).
//
// Neither a pass nor an event ever returns an error to the loop: an observer that stopped on the
// first cluster hiccup would produce exactly the silent gap the coverage record exists to expose.
func (o *Observer) Run(ctx context.Context) {
	slog.InfoContext(ctx, "failure observer started", "interval", o.interval, "retention", o.retention)
	// The watches are children of this context, so returning stops every one of them rather than
	// leaving goroutines writing into a channel nobody reads.
	ctx, stop := context.WithCancel(ctx)
	defer stop()
	for {
		if now := o.engine.clock.Now(); !now.Before(o.nextPass) {
			o.observe(ctx)
		}
		o.settle(ctx)
		select {
		case <-ctx.Done():
			slog.InfoContext(ctx, "failure observer stopped", "reason", ctx.Err())
			return
		case ev := <-o.events:
			o.handle(ctx, ev)
		case <-o.after(o.wait()):
		}
	}
}

// wait is how long the run loop may sleep: until the next periodic pass, or until the earliest
// latched transition matures, whichever is sooner.
func (o *Observer) wait() time.Duration {
	next := o.nextPass
	if at, ok := o.nextDeadline(); ok && at.Before(next) {
		next = at
	}
	if d := next.Sub(o.engine.clock.Now()); d > 0 {
		return d
	}
	return 0
}

// sweep accumulates one periodic pass over everything Burrow manages before any of it is written, so
// the ledger sees a consistent picture: the failures found, the objects that could not be read, and
// whether the pass was complete.
type sweep struct {
	// at is the single instant every observation in this pass is stamped with, read once from the
	// injected clock. One timestamp per pass is what makes "thirty rows in one minute" legible as
	// the one cluster-level event it is (ADR-0074 §5) rather than a smear across a pass's duration.
	at time.Time
	// found are the failures observed active in this pass.
	found []FailureObservation
	// skipped are the objects this pass could not read. Their rows are left exactly as they are:
	// a failure Burrow could not check is not a failure that recovered.
	skipped []ObjectRef
	// degraded is the most recent reason this pass was incomplete, empty when it read everything.
	degraded string
	// namespaces are the namespaces the registry's apps resolve to, mapped to their environment.
	// Gathering it here is what keeps the WATCHED set bounded by the registry (ADR-0074 §3): the
	// watches are reconciled against exactly the set this pass just enumerated, never against a
	// label selector over the cluster.
	namespaces map[string]string
	// managed is every object the registry still records, so a latch entry for one it no longer does
	// is dropped rather than held for the life of the process (ADR-0079's bounded-state consequence).
	managed map[ObjectRef]bool
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

// observe runs one periodic pass: enumerate what the registry says Burrow owns, compare it against
// the cluster for the absences a watch cannot report, reconcile the watches against that same set,
// take in whatever the watches have already said, write what broke, close what recovered, extend the
// coverage record, and enforce retention.
//
// A failure to ENUMERATE aborts the pass without resolving anything and without advancing coverage.
// Every enumeration is a read of the control plane's own database, so a failure means the database
// is unavailable — and a pass that resolved rows on the strength of an intent list it could not read
// would report a cluster-wide recovery that did not happen. Not advancing coverage is the honest
// consequence: the observer is running but is not observing, and that reads as a gap, which is what
// it is.
func (o *Observer) observe(ctx context.Context) {
	sw := &sweep{
		at:         o.engine.clock.Now(),
		namespaces: make(map[string]string),
		managed:    make(map[ObjectRef]bool),
	}
	// The next pass is scheduled before this one runs anything, so a pass that abandons itself on an
	// unreadable database still yields the interval rather than spinning.
	o.nextPass = sw.at.Add(o.interval)
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
			slog.WarnContext(ctx, "observation pass aborted: could not enumerate what Burrow manages",
				"objects", step.what, "error", err)
			return
		}
		if ctx.Err() != nil {
			return
		}
	}

	// The dwells are the operator's, read once here and used until the next pass.
	o.limits = o.engine.operationalConfig(ctx)
	o.reconcileWatches(ctx, sw)
	// Whatever the watches have already said is folded in BEFORE coverage is decided, so a watch that
	// synced during this pass is counted as covering it rather than as one pass late.
	o.drain(ctx)
	o.settle(ctx)
	o.forgetUnmanaged(sw.managed)

	// The rows the watch still believes open are re-affirmed here and nowhere else. Without it a
	// standing failure that emits no further events — a pod the scheduler has refused for an hour
	// reports its condition once — would keep the last_seen of its first edge, and "is it still
	// happening" would read as no. Their keys join this pass's own, because the resolution below
	// decides by absence and a latched row is not absent.
	standing := o.reaffirm(sw)
	keep := append(sw.keys(), standing...)
	for _, obs := range sw.found {
		if err := o.engine.db.RecordFailure(ctx, obs); err != nil {
			slog.WarnContext(ctx, "recording a failure in the ledger failed",
				"object", obs.Object, "reason", obs.Reason, "error", err)
			sw.degraded = "a failure observed this pass could not be written to the ledger"
		}
	}
	if err := o.engine.db.ResolveFailures(ctx, sw.at, keep, sw.skipped); err != nil {
		slog.WarnContext(ctx, "resolving recovered failures failed", "error", err)
		sw.degraded = "recovered failures could not be closed this pass"
	}

	o.extendCoverage(ctx, sw, o.engine.clock.Now())
	o.prune(ctx, sw.at)
}

// observeApps compares the apps the registry records against the workloads the cluster has, one
// listing per environment. It looks for ADR-0074 §6's ABSENCE only. The blocking conditions a pod
// reports are the watch's, latched (ADR-0079 §2), and recording them here as well would open a row
// for a flap the latch had deliberately swallowed — two mechanisms writing the same rows on
// different terms, which is worse than either.
//
// The listing is also what resolves each environment to its namespace, and that map is what the
// watches are reconciled against, so the watched set stays bounded by the registry.
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
			ref := ObjectRef{Kind: FailureApp, Name: app, Environment: env}
			refs = append(refs, ref)
			sw.managed[ref] = true
		}
		ns, err := o.engine.resolveNamespace(ctx, env)
		if err != nil {
			skipAll(sw, refs, fmt.Sprintf("environment %q could not be resolved to a namespace", env))
			continue
		}
		sw.namespaces[ns] = env
		statuses, err := o.engine.k8s.WithNamespace(ns).ListWorkloads(ctx)
		if err != nil {
			skipAll(sw, refs, fmt.Sprintf("the workloads in namespace %q could not be listed", ns))
			continue
		}
		running := make(map[string]bool, len(statuses))
		for _, ws := range statuses {
			running[ws.App] = true
		}
		for _, ref := range refs {
			if !running[ref.Name] {
				// The §6 diagnosis kubectl structurally cannot make: the evidence is an absence, and
				// only the side that knows what was intended can see it.
				sw.record(ref, ReasonWorkloadMissing,
					"the registry records a rolled-out release for this app, but the cluster has no workload for it")
			}
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
		sw.managed[ref] = true
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
		sw.managed[ref] = true
		// WHICH OBJECT WOULD STILL BE THERE DEPENDS ON THE MECHANISM. A logical dump leaves a Job; a
		// physical backup leaves a `Backup` custom resource (ADR-0066 §2). Asking the wrong one would
		// report every pending physical backup as abandoned once a minute, on a cluster where nothing
		// is wrong — the exact opposite of what a coverage record is for.
		present, err := o.backupObjectPresent(ctx, b)
		if err != nil {
			sw.skip(ref, fmt.Sprintf("the in-cluster object for backup %q could not be read", b.ID))
			continue
		}
		if !present {
			sw.record(ref, ReasonBackupJobMissing, backupAbandonedDetail(b))
		}
	}
	return nil
}

// backupObjectPresent asks the cluster whether the object that would be finishing this pending
// backup still exists, choosing the question by the row's kind.
func (o *Observer) backupObjectPresent(ctx context.Context, b Backup) (bool, error) {
	if b.Kind == BackupKindPhysical {
		return o.engine.k8s.PhysicalBackupPresent(ctx, b.ID)
	}
	return o.engine.k8s.BackupJobPresent(ctx, b.ID)
}

// backupAbandonedDetail is the line a stranded pending row is recorded with. It names the app for a
// logical dump and the environment's instance for a physical one, because those are the things the
// two back up and "this backup of \"\"" is not a sentence.
func backupAbandonedDetail(b Backup) string {
	since := b.CreatedAt.UTC().Format(time.RFC3339)
	if b.Kind == BackupKindPhysical {
		return fmt.Sprintf("this physical backup of environment %s's instance has been pending since %s and its Backup object no longer exists, so it will never complete",
			envName(b.Environment), since)
	}
	return fmt.Sprintf("this backup of %q has been pending since %s and its Job no longer exists, so it will never complete", b.App, since)
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
		sw.managed[ref] = true
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

// reconcileWatches brings the established watches into line with the namespaces this pass
// enumerated: a namespace the registry no longer reaches loses its watch and its latch, and one it
// has gained gets a watch. A watch that could not be established is recorded as an unsynced entry
// rather than skipped, so the next pass retries it and coverage does not claim a namespace nobody is
// watching (ADR-0079 §4).
func (o *Observer) reconcileWatches(ctx context.Context, sw *sweep) {
	for ns, w := range o.watches {
		if _, wanted := sw.namespaces[ns]; wanted {
			continue
		}
		if w.cancel != nil {
			w.cancel()
		}
		delete(o.watches, ns)
	}
	for ns, env := range sw.namespaces {
		if w, ok := o.watches[ns]; ok && w.cancel != nil {
			continue
		}
		wctx, cancel := context.WithCancel(ctx)
		if err := o.engine.k8s.WithNamespace(ns).WatchWorkloads(wctx, o.events); err != nil {
			cancel()
			slog.WarnContext(ctx, "establishing a workload watch failed; the ledger is not covering this namespace",
				"namespace", ns, "environment", env, "error", err)
			o.watches[ns] = &workloadWatch{env: env}
			sw.degraded = LedgerDetail(fmt.Sprintf("the workloads in namespace %q could not be watched", ns))
			continue
		}
		o.watches[ns] = &workloadWatch{env: env, cancel: cancel}
	}
}

// watching reports whether every namespace the registry reaches is being watched with a complete
// current picture. It is the coverage question, and it is answered across ALL namespaces rather than
// per namespace because the coverage record is one timeline: a window that claimed coverage while one
// environment's watch was down would be claiming more than it saw. Erring toward reporting a gap is
// the right direction for the one surface whose job is to admit what it does not know.
//
// No watches at all is covered, not uncovered: an installation with nothing deployed has nothing to
// watch, and a gap there would be an artefact rather than a fact.
func (o *Observer) watching() bool {
	for _, w := range o.watches {
		if !w.synced {
			return false
		}
	}
	return true
}

// extendCoverage records that this pass happened, so a gap in the ledger is distinguishable from a
// period in which nothing broke. The first pass with everything watched opens a window; every later
// one extends it. A window that has gone (pruned by retention, or a database restored from a backup)
// causes the next pass to open a fresh one rather than silently stop recording coverage — the one
// failure this surface may not have.
//
// A pass during which anything is unwatched extends nothing and opens nothing. That is ADR-0079 §4:
// between a dropped watch and its completed re-list the observer saw nothing, and claiming coverage
// over it would be the observer pretending continuity across its own blind spot.
func (o *Observer) extendCoverage(ctx context.Context, sw *sweep, until time.Time) {
	if !o.watching() {
		return
	}
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

// endCoverage closes the open window at the instant a watch dropped, so the gap the reader sees
// begins when the observer actually stopped seeing rather than at the last periodic pass. The next
// pass with everything watched opens a new window, and the space between the two is the drop
// (ADR-0079 §4).
func (o *Observer) endCoverage(ctx context.Context, at time.Time) {
	if o.window == 0 {
		return
	}
	if err := o.engine.db.ExtendObservationWindow(ctx, o.window, at, ""); err != nil && !errors.Is(err, ErrNotFound) {
		slog.WarnContext(ctx, "closing observation coverage at a dropped watch failed", "error", err)
	}
	o.window = 0
}

// prune enforces the retention bound ADR-0074 §4 requires. It runs on its own cadence rather than
// every pass, and only ever removes RESOLVED failures and elapsed windows: an active failure is
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

// skipAll marks every object in refs unread this pass for one shared reason — a namespace that
// could not be resolved or listed affects each app in it identically.
func skipAll(sw *sweep, refs []ObjectRef, reason string) {
	for _, ref := range refs {
		sw.skip(ref, reason)
	}
}

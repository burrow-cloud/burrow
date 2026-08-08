// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package controlplane

import (
	"context"
	"log/slog"
	"time"
)

// The LATCH: the half of ADR-0079 that makes a watch usable as a ledger.
//
// Kubernetes status is not a sequence of meaningful transitions, it is a stream of edges. A pod goes
// NotReady and Ready again in two seconds during an ordinary rolling update; a scheduler takes four
// seconds to place a pod that was briefly unschedulable because a node was still joining; a container
// that will start fine reports ContainerCreating while its image layers unpack. An observer that
// recorded every edge would produce precisely what ADR-0074 §4 rejected in choosing a ledger over an
// event stream — thousands of rows describing nothing, and a first_seen that answers "when did the
// last flap begin".
//
// So a transition is HELD before it is recorded. It must persist for a dwell before it opens a row,
// and it must be clear for a dwell before that row is closed (ADR-0079 §2). Both edges, because
// latching only the opening one lets a flapper open and close one row repeatedly, which is the same
// noise arriving through a different door and makes §4's occurrence count count flaps.
//
// The dwell is PER REASON (ADR-0079 §3), on one principle: a reason that is already the outcome of
// waiting gets no further wait. Adding one would double-count the patience Kubernetes has already
// spent and delay a row that is certainly real.

// latched is one condition's transition state. Exactly one of the two instants is meaningful at a
// time: before the row is open, `since` is when the condition started; after, `cleared` is when it
// stopped, or zero while it is still being reported.
type latched struct {
	// since is when this condition was first reported continuously — the instant a row opened for it
	// is stamped with, because that is when it started, not when Burrow finished being sure.
	since time.Time
	// cleared is when the condition was last observed absent, zero while it is present. An open row
	// with a non-zero cleared is one dwell away from being resolved.
	cleared time.Time
	// detail is the most recent line the evidence carried, kept so the row opens with what was
	// observed rather than with whatever the first edge happened to say.
	detail string
	// open reports whether a ledger row has been written for this condition.
	open bool
	// recorded is when a sighting of this condition was last written to the ledger. It is what stops
	// the periodic pass re-affirming a row the latch opened moments earlier in the same pass, which
	// would count one failure twice.
	recorded time.Time
	// retryAt is when a write that failed may be attempted again, zero when none has. Without it a
	// transition whose dwell has elapsed and whose write keeps failing would be retried as fast as the
	// loop can turn: the deadline stays in the past, so the loop would never sleep. The transition is
	// not abandoned — an unavailable database must not lose a failure — it is merely paced.
	retryAt time.Time
}

// writeRetry is how long a ledger write that failed waits before the latch tries it again. It is
// short enough that a brief database hiccup costs the row seconds and long enough that an outage
// does not become a tight loop against the thing that is already down.
const writeRetry = 5 * time.Second

// dwell is how long reason must persist before it opens a ledger row, and how long it must be gone
// before that row is resolved (ADR-0079 §3).
//
// The table is the record's, and so is the shape of the argument for each entry:
//
//   - OOMKilled has none. It already happened; the kernel killed the process and there is nothing to
//     wait to see.
//   - CrashLoopBackOff has none. The backoff IS the dwell — Kubernetes only reports it after repeated
//     failures.
//   - ProgressDeadlineExceeded and DeadlineExceeded have none. A deadline is itself a dwell that has
//     already elapsed.
//   - Unschedulable carries status.unschedulable_grace. A scheduler may simply be slow, or a node may
//     be joining. VolumeUnavailable is the same fact read off the same PodScheduled condition — a pod
//     waiting on a claim that is not bound yet — and it shares the value because the limit's own
//     obligation is that whatever applies has to apply everywhere the same fact is judged.
//   - ImagePullBackOff and ErrImagePull carry status.image_pull_grace, which is short. A registry
//     hiccup that resolves itself is not worth a row.
//
// Everything else has none. That is not an oversight: the remaining reasons are all a report that
// something already failed rather than a report that something has not yet succeeded —
// CreateContainerConfigError names a key that is missing now, StartError a container that already
// refused to start — and a dwell on one of those would delay a row that will not change.
//
// §6's absence reasons never reach here at all: they are the periodic pass's, written on a comparison
// rather than an edge, and a comparison has nothing to flap.
func (o *Observer) dwell(reason string) time.Duration {
	switch reason {
	case ReasonUnschedulable, ReasonVolumeUnavailable:
		d, _ := o.limits.Duration("", LimitUnschedulableGrace)
		return d
	case ReasonImagePullBackOff, ReasonErrImagePull:
		d, _ := o.limits.Duration("", LimitImagePullGrace)
		return d
	default:
		return 0
	}
}

// handle folds one watch event into the observer's state.
func (o *Observer) handle(ctx context.Context, ev WorkloadEvent) {
	w := o.watches[ev.Namespace]
	if w == nil {
		// An event from a watch this observer has already torn down, still in flight when the
		// namespace left the registry. Its latch entries went with it; acting on it would resurrect
		// them.
		return
	}
	now := o.engine.clock.Now()
	switch ev.Kind {
	case WorkloadSynced:
		if !w.synced {
			slog.InfoContext(ctx, "workload watch synced", "namespace", ev.Namespace, "environment", w.env)
		}
		w.synced = true
	case WorkloadDropped:
		w.synced = false
		slog.WarnContext(ctx, "workload watch dropped; observation coverage ends here until it re-lists",
			"namespace", ev.Namespace, "environment", w.env, "detail", ev.Detail)
		// The rows already open are left exactly as they are. A failure Burrow could not check is not
		// a failure that recovered, and the coverage record is where the reader learns it could not
		// be checked (ADR-0079 §4).
		o.endCoverage(ctx, now)
	case WorkloadChanged:
		o.observed(ObjectRef{Kind: FailureApp, Name: ev.Status.App, Environment: w.env}, ev.Status.IssueReason, ev.Status.Issue, now)
	case WorkloadGone:
		// A workload that is not there reports no conditions. Its rows clear on the normal edge and
		// are resolved a dwell later; whether its ABSENCE is itself a failure is ADR-0074 §6's
		// question, answered against the registry by the periodic pass.
		o.observed(ObjectRef{Kind: FailureApp, Name: ev.Status.App, Environment: w.env}, "", "", now)
	}
}

// drain takes in every event already delivered without waiting for more, so a caller that needs the
// observer's state to reflect what the cluster has already said — the periodic pass, deciding
// coverage — does not have to wait a loop iteration per event.
func (o *Observer) drain(ctx context.Context) {
	for {
		select {
		case ev := <-o.events:
			o.handle(ctx, ev)
		default:
			return
		}
	}
}

// observed folds one sighting of one object into the latch: `reason` is what the cluster is
// reporting for it right now, empty for none.
//
// One sighting says something about EVERY reason latched against that object, not only about the one
// being reported. A WorkloadStatus carries the single reason the live surface ranked highest
// (issues.go), so a reason that was latched and is not in this sighting has cleared — and it is that
// inference, not an event, that starts the closing dwell.
func (o *Observer) observed(ref ObjectRef, reason, detail string, now time.Time) {
	if reason != "" && IsLedgerReason(reason) {
		key := FailureKey{Object: ref, Reason: reason}
		p := o.latch[key]
		if p == nil {
			p = &latched{since: now}
			o.latch[key] = p
		}
		p.detail = detail
		// Present again. A row one moment from being resolved goes back to being open rather than
		// closing and reopening, which is what stops a flapper counting as two episodes (ADR-0079 §2).
		p.cleared = time.Time{}
	}
	for key, p := range o.latch {
		if key.Object != ref || key.Reason == reason {
			continue
		}
		if !p.open {
			// A condition that never lasted its dwell leaves nothing behind at all. This is the flap
			// the latch exists to swallow, and swallowing it is why the ledger has no row for a pod
			// that went unready for ten seconds during a rolling update.
			delete(o.latch, key)
			continue
		}
		if p.cleared.IsZero() {
			p.cleared = now
		}
	}
}

// settle acts on every latched transition whose dwell has elapsed: a condition that has persisted
// opens its row, and one that has been clear closes it. It is called after every event and at the
// top of every loop iteration, and it is pure arithmetic against the injected clock — there is no
// timer anywhere in the latch, which is what keeps it testable the way ADR-0010 requires.
func (o *Observer) settle(ctx context.Context) {
	now := o.engine.clock.Now()
	for key, p := range o.latch {
		if now.Before(p.retryAt) {
			continue
		}
		switch d := o.dwell(key.Reason); {
		case !p.open && !now.Before(p.since.Add(d)):
			o.openRow(ctx, key, p, now)
		case p.open && !p.cleared.IsZero() && !now.Before(p.cleared.Add(d)):
			o.closeRow(ctx, key, p, now)
		}
	}
}

// openRow writes the row for a transition that has now persisted for its dwell.
//
// IT IS STAMPED WITH THE ONSET, NOT WITH THE MOMENT THE DWELL EXPIRED. `first_seen` answers "when did
// this start", and for a condition that went on to last, the honest answer is when the cluster began
// reporting it — the dwell is how long Burrow waited to be sure, not part of what happened. What the
// latch removes is the transition that never lasted, which never gets a row at all.
//
// A write that fails leaves the entry pending, so the next settle retries it rather than losing the
// transition to one unavailable database.
func (o *Observer) openRow(ctx context.Context, key FailureKey, p *latched, now time.Time) {
	obs := FailureObservation{Object: key.Object, Reason: key.Reason, Detail: LedgerDetail(p.detail), At: p.since}
	if err := o.engine.db.RecordFailure(ctx, obs); err != nil {
		slog.WarnContext(ctx, "recording a failure in the ledger failed",
			"object", key.Object, "reason", key.Reason, "error", err)
		p.retryAt = now.Add(writeRetry)
		return
	}
	p.open, p.recorded, p.retryAt = true, now, time.Time{}
}

// closeRow resolves the row for a condition that has now been clear for its dwell, at the instant it
// actually cleared rather than at the moment the dwell confirmed it — the same choice openRow makes,
// from the other side, so a row's lifetime describes the failure rather than Burrow's caution.
func (o *Observer) closeRow(ctx context.Context, key FailureKey, p *latched, now time.Time) {
	if err := o.engine.db.ResolveFailure(ctx, p.cleared, key); err != nil {
		slog.WarnContext(ctx, "resolving a recovered failure failed",
			"object", key.Object, "reason", key.Reason, "error", err)
		p.retryAt = now.Add(writeRetry)
		return
	}
	delete(o.latch, key)
}

// nextDeadline is the earliest instant at which some latched transition becomes actionable, and
// whether there is one. It is what the run loop sizes its sleep from, so a dwell expires on time
// instead of at the next periodic pass.
func (o *Observer) nextDeadline() (time.Time, bool) {
	var earliest time.Time
	for key, p := range o.latch {
		var at time.Time
		switch {
		case !p.open:
			at = p.since.Add(o.dwell(key.Reason))
		case !p.cleared.IsZero():
			at = p.cleared.Add(o.dwell(key.Reason))
		default:
			continue
		}
		if at.Before(p.retryAt) {
			at = p.retryAt
		}
		if earliest.IsZero() || at.Before(earliest) {
			earliest = at
		}
	}
	return earliest, !earliest.IsZero()
}

// reaffirm records one more sighting of every row the watch still believes open, stamped with the
// pass's instant, and returns their keys so the resolution pass keeps them.
//
// A row in a namespace whose watch is NOT synced is kept but not re-affirmed. Its last_seen freezes,
// which is the truth — nobody is looking — and the coverage record is what says so. Advancing it
// would be the observer asserting a failure is ongoing on the strength of a watch that has stopped
// reporting. Nor is a row the latch opened moments ago in THIS pass re-affirmed: it has already been
// written once at the instant it started, and writing it again would make one failure two sightings.
func (o *Observer) reaffirm(sw *sweep) []FailureKey {
	keys := make([]FailureKey, 0, len(o.latch))
	for key, p := range o.latch {
		if !p.open {
			continue
		}
		keys = append(keys, key)
		if !o.covered(key.Object.Environment) || !p.recorded.Before(sw.at) {
			continue
		}
		p.recorded = sw.at
		sw.found = append(sw.found, FailureObservation{
			Object: key.Object, Reason: key.Reason, Detail: LedgerDetail(p.detail), At: sw.at,
		})
	}
	return keys
}

// covered reports whether the watch over env's namespace is currently delivering.
func (o *Observer) covered(env string) bool {
	for _, w := range o.watches {
		if w.env == env {
			return w.synced
		}
	}
	return false
}

// forgetUnmanaged drops the latch entries for objects the registry no longer records, which is what
// bounds the latch by the managed set rather than by how much has ever gone wrong (ADR-0079's
// bounded-state consequence). Their rows are not resolved here: they fall out of the resolution
// pass's keep list on this same pass and are closed by it, which is the path an object that has left
// the registry has always taken.
func (o *Observer) forgetUnmanaged(managed map[ObjectRef]bool) {
	for key := range o.latch {
		if !managed[key.Object] {
			delete(o.latch, key)
		}
	}
}

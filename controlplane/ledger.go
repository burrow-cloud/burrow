// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package controlplane

import (
	"fmt"
	"strings"
	"time"
)

// The failure ledger is the durable record of what the cluster DID, kept because nothing else can
// reconstruct it afterwards (ADR-0074 §1/§4). Current state stays a live read — `burrow status` is
// never served from here — and this file holds only history: one row per (object, reason), with a
// first-seen, a last-seen, a resolution and a count.
//
// IT IS NOT THE AUDIT LOG AND MUST NOT SHARE ITS TABLE (ADR-0074 §7). The audit log records what
// Burrow was ASKED to do and what it DECIDED; it is append-only and its value depends on being
// complete. This records what happened afterwards, mostly unrequested by anyone, and §4 requires it
// to be pruned. Merging them would subject a security-relevant append-only record to that pruning
// and make "the audit log is complete" false, so they are separate tables read together rather than
// one table read twice.
//
// NO SECRET VALUE EVER ENTERS A LEDGER ROW (ADR-0074 §9). A row carries an object name, a reason
// from a closed set, and one bounded Burrow-authored line — see LedgerDetail, which is the single
// gate every detail passes through.

// FailureKind names the class of managed object a ledger row is about. It is part of the row's
// identity, not decoration: an app and an add-on may share a name, and a reason on one says nothing
// about the other.
type FailureKind string

const (
	// FailureApp is a deployed application — the workload behind a release the registry records.
	FailureApp FailureKind = "app"
	// FailureAddon is an installed building-block add-on (ADR-0025).
	FailureAddon FailureKind = "addon"
	// FailureBackup is a recorded backup (ADR-0032), identified by its backup id.
	FailureBackup FailureKind = "backup"
	// FailureExposure is an app's recorded exposure — the Ingress and certificate behind a host
	// (ADR-0018).
	FailureExposure FailureKind = "exposure"
)

// Valid reports whether k is a known FailureKind.
func (k FailureKind) Valid() bool {
	switch k {
	case FailureApp, FailureAddon, FailureBackup, FailureExposure:
		return true
	default:
		return false
	}
}

// ObjectRef identifies one object Burrow manages, stably enough that two observations a week apart
// land on the same ledger row. The environment is part of the identity for the same reason it is
// part of a release's: the same app name in staging and in production is two objects, and a failure
// on one is not a failure on the other (ADR-0067 §1).
type ObjectRef struct {
	Kind        FailureKind `json:"kind"`
	Name        string      `json:"name"`
	Environment string      `json:"environment,omitempty"`
}

// String renders the reference for a log line or an error. It is not the storage key — see
// ledgerKey — so it can stay readable.
func (r ObjectRef) String() string {
	if r.Environment == "" {
		return fmt.Sprintf("%s %s", r.Kind, r.Name)
	}
	return fmt.Sprintf("%s %s (%s)", r.Kind, r.Name, r.Environment)
}

// FailureKey is one ledger row's identity: the object AND the reason (ADR-0074 §5). Keying on both
// is what lets one pod be OOM-killed and unschedulable at once without either being lost, and what
// makes a pod alternating between two reasons legible as the one bug it is. A single status field
// per object would silently drop the second, which is the failure this record exists to stop.
type FailureKey struct {
	Object ObjectRef
	Reason string
}

// FailureObservation is one sighting of one failure, handed to the store to open or extend a row.
// It carries no lifetime of its own: whether this is a new failure or the four-hundredth sighting of
// a standing one is the store's decision, made against what is already there.
type FailureObservation struct {
	// Object is what failed.
	Object ObjectRef
	// Reason is a member of the closed ledger vocabulary (LedgerReasons).
	Reason string
	// Detail is one bounded, Burrow-authored line — already through LedgerDetail.
	Detail string
	// At is the observation time, from the injected clock (ADR-0010).
	At time.Time
}

// Failure is one ledger row: a failure with a lifetime rather than an event (ADR-0074 §4). An event
// stream has higher fidelity and is unreadable at the moment it matters — an incident produces
// thousands of rows describing one problem — so the shape here answers "when did it start" and "is
// it still happening" in a single row.
type Failure struct {
	// ID is the store-assigned row identity (0 before it is persisted).
	ID int64 `json:"id,omitempty"`
	// Object is what failed.
	Object ObjectRef `json:"object"`
	// Reason is the member of the closed ledger vocabulary this row is about.
	Reason string `json:"reason"`
	// Detail is the most recent bounded, Burrow-authored line for this failure. It is overwritten
	// on each sighting rather than accumulated: the row is a transition, not a log.
	Detail string `json:"detail,omitempty"`
	// FirstSeen is when this failure began — the answer to "when did it start", and the reason the
	// ledger exists at all.
	FirstSeen time.Time `json:"first_seen"`
	// LastSeen is when it was last observed — the answer to "is it still happening".
	LastSeen time.Time `json:"last_seen"`
	// ResolvedAt is when the failure stopped being observed, nil while it is still active — the
	// answer to "did it recover on its own". It is a pointer so an active row serialises without a
	// zero timestamp that reads as a date in the year 1.
	ResolvedAt *time.Time `json:"resolved_at,omitempty"`
	// Occurrences is how many OBSERVATIONS found this failure present, counting the one that opened
	// the row. It is deliberately a count of sightings rather than of restarts: a restart count
	// exists for a crash loop and for nothing else in the vocabulary, and a field that meant
	// different things for different reasons could not be compared across rows. Read with the
	// observation cadence, it is what says whether a failure is a blip or a standing condition.
	Occurrences int64 `json:"occurrences"`
}

// Active reports whether this failure was still happening when it was last observed.
func (f Failure) Active() bool { return f.ResolvedAt == nil }

// FailureFilter narrows a ledger query. A zero value matches every ACTIVE row (subject to Limit) —
// the default a reader wants during an incident, which is "what is broken", not "what ever broke".
type FailureFilter struct {
	// Kind restricts to one class of object; empty matches any.
	Kind FailureKind
	// Name restricts to one object name; empty matches any.
	Name string
	// Environment restricts to one environment; empty matches any.
	Environment string
	// Reason restricts to one reason; empty matches any. It is the filter that answers "what else
	// hit this in the same window", which is §5's grouping seen from the query side.
	Reason string
	// Since restricts to rows last seen at or after this instant; the zero value applies no bound.
	Since time.Time
	// IncludeResolved widens the result from active failures to the history as well.
	IncludeResolved bool
	// Limit caps the number of rows returned. Zero or negative applies the store's default cap.
	Limit int
}

// The §6 reasons: what Burrow INTENDED to exist, compared with what the cluster has.
//
// These are additions to ADR-0074 §2's IssueReason vocabulary, not a second vocabulary beside it.
// They are separate constants because they are a different KIND of fact: every IssueReason is a
// condition the cluster reported about an object that exists, and each of these is an ABSENCE, which
// no Kubernetes reason string names because no controller is there to emit one. That is exactly why
// §6 calls it the diagnosis kubectl structurally cannot make, and why the ledger has to be the thing
// that makes it: only the side that knows what was intended can see it.
//
// They are deliberately NOT members of IssueReasons(): that set gates the live status surface, whose
// criterion is a blocking condition observed ON a pod (ADR-0074 §2). Widening it here would put a
// registry-versus-cluster verdict into a field documented as the cluster's own reason.
const (
	// ReasonWorkloadMissing is a registered app — one whose release history says a Deployment was
	// created — with no Deployment in the cluster: deleted by hand, evicted and never rescheduled,
	// or never created because a write failed.
	ReasonWorkloadMissing = "WorkloadMissing"
	// ReasonAddonNotRunning is an add-on the registry records as installed whose backing workload is
	// not available.
	ReasonAddonNotRunning = "AddonNotRunning"
	// ReasonBackupJobMissing is a backup row still `pending` whose Job no longer exists — the shape
	// a backup interrupted by a burrowd restart leaves behind, and one that is otherwise
	// indistinguishable from a backup still running.
	ReasonBackupJobMissing = "BackupJobMissing"
	// ReasonIngressMissing is a recorded exposure with no Ingress: the app is registered as reachable
	// at a host and nothing routes it.
	ReasonIngressMissing = "IngressMissing"
	// ReasonCertificateNotIssued is a recorded TLS exposure whose Ingress exists but whose
	// certificate has not been issued. It is separated from ReasonIngressMissing because the fix is
	// a different one — cert-manager, an issuer, or a DNS/HTTP-01 path — not a missing route.
	ReasonCertificateNotIssued = "CertificateNotIssued"
)

// DiscrepancyReasons returns every §6 intent-versus-cluster reason.
func DiscrepancyReasons() []string {
	return []string{
		ReasonWorkloadMissing,
		ReasonAddonNotRunning,
		ReasonBackupJobMissing,
		ReasonIngressMissing,
		ReasonCertificateNotIssued,
	}
}

// LedgerReasons returns the closed vocabulary a ledger row may carry: ADR-0074 §2's IssueReason set
// as merged for the live status surface, plus §6's intent-versus-cluster reasons. It is enumerated
// rather than left implicit in a switch for the same reason IssueReasons() is — the ledger's most
// important consumer is an agent branching on the reason, not a person reading prose (ADR-0074 §5).
func LedgerReasons() []string {
	issues := IssueReasons()
	out := make([]string, 0, len(issues)+len(DiscrepancyReasons()))
	out = append(out, issues...)
	return append(out, DiscrepancyReasons()...)
}

// IsLedgerReason reports whether reason is a member of the closed ledger vocabulary. The observer
// checks it before writing, so a reason nobody decided to record cannot reach a row by accident.
func IsLedgerReason(reason string) bool {
	for _, r := range LedgerReasons() {
		if r == reason {
			return true
		}
	}
	return false
}

// LedgerDetailBytes bounds a ledger row's detail. A detail is one line of context beside a reason,
// not a report: the reason is what an agent branches on, and an unbounded detail would make the
// ledger a place log output accumulates.
const LedgerDetailBytes = 400

// LedgerDetail is the single gate every ledger detail passes through, and the enforcement point for
// ADR-0074 §9's rule that no secret value ever enters a ledger row.
//
// It takes the FIRST LINE only. That is not cosmetic. The live status surface's crash-loop Issue
// carries a bounded tail of the application's PREVIOUS log after a newline (issues.go), and
// application output may contain anything at all — a connection string it printed on startup, a
// token in a stack frame. Bounded and labelled, that is an acceptable trade for a value read live
// and discarded; it is a different trade for a row that persists for the retention window in the
// control plane's own database. So the ledger keeps the part Burrow authored — the reason, the exit
// code, the limit, the missing key — and leaves the application's own output to `burrow app logs`
// and the live status surface, which still carry it.
func LedgerDetail(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return boundText(strings.TrimSpace(strings.Join(strings.Fields(line), " ")), LedgerDetailBytes)
}

// ObservationWindow is one continuous run of the observer: when it started, how far its completed
// sweeps have carried coverage, and how many of those sweeps were incomplete.
//
// It exists because ADR-0074's consequences name the failure mode a reliability surface must not
// have: if the observer was down from 02:00 to 03:00, an empty ledger for that hour reads as
// "nothing broke". A GAP BETWEEN WINDOWS IS THEREFORE LITERAL — it is time no observer was
// recording, and a reader can tell it apart from a quiet hour. A restart always begins a NEW window
// rather than extending the last one, so burrowd bouncing can never be papered over into an
// unbroken stretch of coverage.
type ObservationWindow struct {
	// ID is the store-assigned window identity (0 before it is persisted).
	ID int64 `json:"id,omitempty"`
	// StartedAt is when this run of the observer began.
	StartedAt time.Time `json:"started_at"`
	// Until is the end of the most recent sweep that COMPLETED its enumeration of what Burrow
	// manages. A sweep that could not enumerate does not advance it, so an observer that is running
	// but wedged stops claiming coverage instead of claiming it falsely.
	Until time.Time `json:"until"`
	// Sweeps is how many sweeps have advanced this window.
	Sweeps int64 `json:"sweeps"`
	// DegradedSweeps is how many of those could not read every object they set out to. Coverage in
	// such a sweep is partial, and a window that reports some is saying its ledger rows may be
	// incomplete for that stretch.
	DegradedSweeps int64 `json:"degraded_sweeps,omitempty"`
	// Detail is the most recent degradation note, bounded like a failure detail. Empty on a window
	// whose sweeps all completed.
	Detail string `json:"detail,omitempty"`
}

// Complete reports whether every sweep in this window read everything it set out to.
func (w ObservationWindow) Complete() bool { return w.DegradedSweeps == 0 }

// LedgerPruneResult is what a retention pass removed. It is returned rather than logged inside the
// store so the caller decides whether a prune is worth a line, and a test can assert the bound is
// actually enforced rather than merely configured.
type LedgerPruneResult struct {
	// Failures is how many resolved failure rows were removed.
	Failures int64 `json:"failures"`
	// Windows is how many observation windows were removed.
	Windows int64 `json:"windows"`
}

// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package controlplane

import (
	"context"
	"fmt"
	"sort"
	"time"
)

// This file is ADR-0063 §7's status surface — "the destination's reachability, the age of the last
// successful backup, and the last failure belong on a status surface a human or an agent can ask
// for" — and it is the surface ADR-0066 §5 requires the backup-age signal to live on.
//
// It answers from what BURROW OBSERVED: its own `Backup` rows, plus a reachability probe run at the
// moment of the call. That sourcing is the decision, not an implementation detail. CloudNativePG's
// own backup status fields and metrics were deprecated in 1.26.1 and, under plugin-based backups,
// report STALE values rather than absent ones — so a health view built on them would report a
// healthy backup age for a backup pipeline that stopped working, and would keep reporting it. A view
// built on Burrow's rows cannot inherit that failure, and it does not change shape when ADR-0066
// replaces the mechanism underneath: the rows are the same rows whether a dump was taken by a Job
// Burrow ran or by an operator Burrow asked.
//
// TWO AGES, because there are two different questions and only one of them is ADR-0063 §7's:
//
//   - the age of the last COMPLETED backup, wherever it went; and
//   - the age of the last DURABLE one — completed, at an object-store destination (Backup.Durable),
//     which is the only kind whose bytes are known to have left the cluster.
//
// Reporting only the first would let a wall of in-cluster dumps read as a backup strategy, which is
// the thing ADR-0063 exists to end. Reporting only the second would hide the dumps an install with
// no registered provider legitimately still takes. So both are reported and the state says which
// applies.
//
// NO THRESHOLD IS ASSERTED HERE. ADR-0063 §7 phrases the signal as "no successful backup in N
// hours", but nothing in Burrow schedules a backup yet — every backup is one somebody asked for — so
// any N this surface picked would be a number Burrow invented and then alerted on. The surface
// reports the ages; the threshold is configuration, and belongs with the scheduling that gives it a
// meaning (ADR-0068).

// BackupHealthState is the shape of what Burrow has observed about a scope's backups. It is
// deliberately a statement of FACT rather than a verdict against a threshold: "durable" does not
// mean "recent enough", it means a backup is known to exist outside the cluster, and the age
// alongside it is what says how long ago.
type BackupHealthState string

const (
	// BackupHealthNever means no backup has ever completed in this scope. It is distinct from a
	// stale one: nothing has been lost yet, and nothing has ever been preserved either.
	BackupHealthNever BackupHealthState = "never"
	// BackupHealthClusterOnly means backups have completed but none of them left the cluster, so
	// every copy shares a failure domain with the database it came from. It is reported as its own
	// state rather than folded into "never" because the dumps genuinely exist and genuinely restore
	// an app whose data is wrong — they just do not survive losing the cluster.
	BackupHealthClusterOnly BackupHealthState = "cluster-only"
	// BackupHealthDurable means at least one backup completed at an object-store destination, which
	// ADR-0063 §7 only permits once the object was written and read back.
	BackupHealthDurable BackupHealthState = "durable"
)

// BackupObservation is one `Backup` row as the health surface reports it: what it was, when, and how
// long ago. It carries the app, environment, destination, provider and size — all names and
// numbers, never a credential — and on a failure the closed reason and the Burrow-authored detail.
type BackupObservation struct {
	// ID is the backup identifier.
	ID string `json:"id"`
	// App is the application whose database was dumped.
	App string `json:"app"`
	// Environment is the environment whose instance it was taken from.
	Environment string `json:"environment,omitempty"`
	// At is when the backup was recorded.
	At time.Time `json:"at"`
	// AgeSeconds is how long ago that was, measured against the injected clock at the moment of the
	// call. It is reported in seconds rather than as a rendered duration so a caller — an agent, an
	// alert rule — compares a number instead of parsing prose.
	AgeSeconds int64 `json:"age_seconds"`
	// Status is the row's lifecycle state.
	Status BackupStatus `json:"status"`
	// Destination is where the bytes ended up: the cluster volume, or the object store.
	Destination BackupDestinationKind `json:"destination,omitempty"`
	// Provider names the object-storage provider, for an object-store destination. A registry name,
	// never a credential or an endpoint secret.
	Provider string `json:"provider,omitempty"`
	// SizeBytes is the dump's size, or 0 when unknown.
	SizeBytes int64 `json:"size_bytes,omitempty"`
	// Reason is why a failed backup failed — a member of the closed BackupFailureReason set, or of
	// ADR-0074 §2's IssueReason set when the Job never started. Empty on a completed row.
	Reason string `json:"reason,omitempty"`
	// Detail is one Burrow-authored line elaborating the reason. Never a vendor response body and
	// never a credential.
	Detail string `json:"detail,omitempty"`
}

// BackupDestinationHealth is one registered object-storage destination, probed AT THE MOMENT OF THE
// CALL. It is not persisted and not cached: a stored reachability verdict goes stale while
// continuing to read as current, which is the failure this surface exists to avoid rather than
// reproduce (the same reasoning that keeps ProviderVerification unpersisted).
type BackupDestinationHealth struct {
	// Provider is the registry name of the object-storage provider.
	Provider string `json:"provider"`
	// Endpoint is the S3-compatible API endpoint that answers for it. Non-secret configuration.
	Endpoint string `json:"endpoint,omitempty"`
	// Bucket is the bucket Burrow writes backups to.
	Bucket string `json:"bucket,omitempty"`
	// Reachable reports whether the bucket answered for this credential just now.
	Reachable bool `json:"reachable"`
	// Detail says what was observed, in one Burrow-authored line. It never carries a vendor response
	// body and never carries a credential: an unreachable destination is described by what Burrow
	// could not do, not by quoting what the vendor said.
	Detail string `json:"detail,omitempty"`
}

// BackupHealth is the answer to "are my backups working?" for one add-on, optionally narrowed to one
// app or one environment. It carries names, times, sizes and reasons — never a credential.
type BackupHealth struct {
	// Addon is the add-on type the health is reported for. Only postgres has backups today.
	Addon AddonType `json:"addon"`
	// App is the app the report was narrowed to, empty when it spans every app.
	App string `json:"app,omitempty"`
	// Environment is the environment the report was narrowed to, empty when it spans every one.
	Environment string `json:"environment,omitempty"`
	// ObservedAt is when this report was assembled, from the injected clock. Every age below is
	// measured against it, so the numbers in one report are consistent with each other.
	ObservedAt time.Time `json:"observed_at"`
	// State says what kind of backup coverage exists.
	State BackupHealthState `json:"state"`
	// LastSuccess is the newest COMPLETED backup, wherever its bytes went. Nil when none exists.
	LastSuccess *BackupObservation `json:"last_success,omitempty"`
	// LastDurableSuccess is the newest backup that completed at an object-store destination — the
	// one ADR-0063 §7's age is about. Nil when no backup has left the cluster. It is the same row as
	// LastSuccess whenever the newest success was durable.
	LastDurableSuccess *BackupObservation `json:"last_durable_success,omitempty"`
	// LastFailure is the newest FAILED backup, with the closed reason it recorded. Nil when none
	// exists. It is reported alongside the successes rather than instead of them: a recent failure
	// after an older success is a different situation from never having succeeded, and an operator
	// deciding whether to act needs both.
	LastFailure *BackupObservation `json:"last_failure,omitempty"`
	// Pending counts rows still recorded as pending in this scope. A pending row is NEVER read as a
	// success — the ages above count completed rows only — so a burrowd that died mid-Job leaves the
	// age growing rather than resetting it. The count is surfaced because a growing pile of pending
	// rows is itself a symptom.
	Pending int `json:"pending,omitempty"`
	// Destinations are the registered object-storage destinations and whether each answered just
	// now, sorted by provider name. Empty when none is registered, which is not an error: an install
	// with no object storage still takes in-cluster dumps, and the state says so.
	Destinations []BackupDestinationHealth `json:"destinations,omitempty"`
	// Summary is one line a human can read or an agent can relay, stating the fact rather than a
	// verdict against a threshold Burrow has not been given.
	Summary string `json:"summary"`
}

// BackupHealth reports what Burrow has observed about an add-on's backups: the age of the last
// successful backup, the age of the last one that left the cluster, the last failure, and whether
// each registered object-storage destination answers right now (ADR-0063 §7, ADR-0066 §5).
//
// An empty app spans every app and an empty env every environment, exactly as ListBackups does — the
// same latitude, for the same reason: this answers "what coverage do I have", and each observation
// says which app and environment it came from. Unlike backup and restore, which act on one instance
// and take the environment as a required target, a health question that had to name an environment
// would be unable to answer the question most worth asking.
//
// It is READ-ONLY and moves no secret: it reads its own rows, and the reachability probe reads the
// provider credential into memory to sign one request and returns names, not values. Nothing here is
// audited, because nothing here changes anything.
//
// A destination that cannot be probed is reported as unreachable WITH the reason, never omitted: a
// destination silently missing from the listing is indistinguishable from no destination at all, and
// "there is nowhere to back up to" is exactly the wrong conclusion to draw from "the probe failed".
func (e *Engine) BackupHealth(ctx context.Context, t AddonType, app, env string) (BackupHealth, error) {
	if t != AddonPostgres {
		return BackupHealth{}, fmt.Errorf("backup health %s: only the postgres add-on has backups: %w", t, ErrInvalid)
	}
	if app != "" {
		if err := (App{Name: app}).Validate(); err != nil {
			return BackupHealth{}, fmt.Errorf("backup health: %w: %w", ErrInvalid, err)
		}
	}
	backups, err := e.db.ListBackups(ctx, app, env)
	if err != nil {
		return BackupHealth{}, fmt.Errorf("backup health: reading recorded backups: %w", err)
	}

	health := BackupHealth{
		Addon:       t,
		App:         app,
		Environment: env,
		ObservedAt:  e.clock.Now(),
		State:       BackupHealthNever,
	}
	// Rows arrive newest first, so the first match in each category is the newest one. Every category
	// is filled in one pass rather than three, so all three observations are read from one listing
	// and cannot describe different moments.
	for i := range backups {
		b := backups[i]
		switch {
		case b.Status == BackupPending:
			health.Pending++
		case b.Status == BackupCompleted:
			if health.LastSuccess == nil {
				health.LastSuccess = e.observe(b, health.ObservedAt)
			}
			if b.Durable() && health.LastDurableSuccess == nil {
				health.LastDurableSuccess = e.observe(b, health.ObservedAt)
			}
		case b.Status == BackupFailed:
			if health.LastFailure == nil {
				health.LastFailure = e.observe(b, health.ObservedAt)
			}
		}
	}
	switch {
	case health.LastDurableSuccess != nil:
		health.State = BackupHealthDurable
	case health.LastSuccess != nil:
		health.State = BackupHealthClusterOnly
	}

	health.Destinations = e.probeBackupDestinations(ctx)
	health.Summary = backupHealthSummary(health)
	return health, nil
}

// observe turns a recorded row into an observation, with its age measured against the one moment the
// whole report is assembled at.
func (e *Engine) observe(b Backup, now time.Time) *BackupObservation {
	age := now.Sub(b.CreatedAt)
	if age < 0 {
		// A row minted in the same instant, or a clock that moved. An age is a duration since
		// something happened, and a negative one is a nonsense a caller would have to special-case.
		age = 0
	}
	return &BackupObservation{
		ID:          b.ID,
		App:         b.App,
		Environment: envName(b.Environment),
		At:          b.CreatedAt,
		AgeSeconds:  int64(age / time.Second),
		Status:      b.Status,
		Destination: b.Destination,
		Provider:    b.Provider,
		SizeBytes:   b.SizeBytes,
		Reason:      b.FailureReason,
		Detail:      b.FailureDetail,
	}
}

// probeBackupDestinations asks each registered object-storage provider whether its bucket answers
// for its credential, right now (ADR-0063 §7). It is best-effort per destination and never fails the
// report: an unreadable provider registry, an unwired object-store seam, or a store that will not
// answer each yield a row saying so, because the caller asked whether their backups are working and
// "the check itself broke" is a usable answer where an error page is not.
//
// The credential is read at call time and used to sign one request. It is never returned, never
// logged, and never placed in a Detail.
func (e *Engine) probeBackupDestinations(ctx context.Context) []BackupDestinationHealth {
	all, err := e.db.Providers(ctx)
	if err != nil {
		return nil
	}
	var out []BackupDestinationHealth
	for _, p := range all {
		if !p.Serves(CapabilityObjectStorage) || p.ObjectStore == nil {
			continue
		}
		out = append(out, e.probeBackupDestination(ctx, p))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Provider < out[j].Provider })
	return out
}

// probeBackupDestination probes one destination. Each failure is described by what Burrow could not
// do, in Burrow's own words — a vendor's response body can carry request identifiers, bucket
// policies and occasionally the credential that was rejected, and none of that belongs on a status
// surface an agent may relay (ADR-0063 §1).
func (e *Engine) probeBackupDestination(ctx context.Context, p Provider) BackupDestinationHealth {
	h := BackupDestinationHealth{
		Provider: p.Name,
		Endpoint: p.ObjectStore.Endpoint,
		Bucket:   p.ObjectStore.Bucket,
	}
	if e.objectStore == nil {
		h.Detail = "this build has no object-storage adapter wired, so the destination cannot be probed"
		return h
	}
	cred, err := e.ObjectStoreCredentialFor(ctx, p)
	if err != nil {
		h.Detail = "the destination's credential could not be read from burrow-credentials"
		return h
	}
	store, err := e.objectStore.ObjectStore(p.ObjectStore.Endpoint, p.ObjectStore.Region, cred)
	if err != nil {
		h.Detail = "the destination's endpoint could not be addressed"
		return h
	}
	exists, err := store.BucketExists(ctx, p.ObjectStore.Bucket)
	switch {
	case err != nil:
		h.Detail = "the endpoint did not answer for this bucket"
	case !exists:
		// BucketExists reports a bucket that belongs to someone else as absent, so these two cases
		// genuinely cannot be told apart from here, and saying so is more useful than picking one.
		h.Detail = "the bucket is absent, or this credential is not permitted to see it"
	default:
		h.Reachable = true
		h.Detail = "the bucket answered"
	}
	return h
}

// backupHealthSummary renders the one line. It states what was observed and, where the observation
// is that nothing durable exists, says what to do about it — the same shape the backup listing uses
// when it explains that an in-cluster dump shares a failure domain with its database.
func backupHealthSummary(h BackupHealth) string {
	scope := "backups"
	switch {
	case h.App != "" && h.Environment != "":
		scope = fmt.Sprintf("backups of %s in environment %s", h.App, h.Environment)
	case h.App != "":
		scope = fmt.Sprintf("backups of %s", h.App)
	case h.Environment != "":
		scope = fmt.Sprintf("backups in environment %s", h.Environment)
	}

	switch h.State {
	case BackupHealthDurable:
		d := h.LastDurableSuccess
		line := fmt.Sprintf("%s: the last backup to leave the cluster was %s ago, to the %q object store",
			scope, humanAge(d.AgeSeconds), d.Provider)
		if h.LastFailure != nil && h.LastFailure.At.After(d.At) {
			line += fmt.Sprintf("; a backup has failed since then (%s)", h.LastFailure.Reason)
		}
		return line
	case BackupHealthClusterOnly:
		return fmt.Sprintf("%s: the last successful backup was %s ago, but no backup has ever left this cluster — "+
			"register an object-storage provider with `burrow config provider add --type s3` so a backup survives losing it",
			scope, humanAge(h.LastSuccess.AgeSeconds))
	default:
		if h.LastFailure != nil {
			return fmt.Sprintf("%s: no backup has ever completed; the most recent attempt failed %s ago (%s)",
				scope, humanAge(h.LastFailure.AgeSeconds), h.LastFailure.Reason)
		}
		return fmt.Sprintf("%s: none recorded — take one with `burrow addon backup postgres <app>`", scope)
	}
}

// humanAge renders an age in seconds as the coarsest unit that still says something useful. It is
// for the summary line only; every machine-readable age on this surface stays in seconds.
func humanAge(seconds int64) string {
	switch {
	case seconds < 60:
		return fmt.Sprintf("%ds", seconds)
	case seconds < 3600:
		return fmt.Sprintf("%dm", seconds/60)
	case seconds < 86400:
		return fmt.Sprintf("%dh", seconds/3600)
	default:
		return fmt.Sprintf("%dd", seconds/86400)
	}
}

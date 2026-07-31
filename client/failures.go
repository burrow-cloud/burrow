// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package client

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// The client side of the failure ledger's read surface (ADR-0074 §8). These DTOs mirror the API's
// JSON contract; like the rest of this package they deliberately do not import the control-plane
// types, so the two CLIs stay decoupled clients across the module boundary.
//
// The json tags must match the engine's exactly. A mismatch here does not fail: it silently drops a
// field, and the field most likely to be dropped quietly is the one that says the ledger has a hole
// in it — which is the one failure ADR-0074 says this surface may not have.

// FailureKind names the class of managed object a ledger row is about.
type FailureKind = string

// The classes of object the ledger records against (controlplane.FailureKind).
const (
	FailureApp      FailureKind = "app"
	FailureAddon    FailureKind = "addon"
	FailureBackup   FailureKind = "backup"
	FailureExposure FailureKind = "exposure"
)

// FailureKinds returns every kind the --kind filter accepts, in the order they are documented.
func FailureKinds() []FailureKind {
	return []FailureKind{FailureApp, FailureAddon, FailureBackup, FailureExposure}
}

// ObjectRef identifies one object Burrow manages. The environment is part of the identity: the same
// app name in staging and in production is two objects.
type ObjectRef struct {
	Kind        FailureKind `json:"kind"`
	Name        string      `json:"name"`
	Environment string      `json:"environment,omitempty"`
}

// Failure is one ledger row: a failure with a lifetime rather than an event (ADR-0074 §4).
type Failure struct {
	ID     int64     `json:"id,omitempty"`
	Object ObjectRef `json:"object"`
	// Reason is a member of the ledger's closed vocabulary — the field to branch on, in preference
	// to parsing Detail.
	Reason string `json:"reason"`
	// Detail is one bounded, Burrow-authored line of context. It never carries a secret value.
	Detail string `json:"detail,omitempty"`
	// FirstSeen is when the failure began: the answer to "when did it start".
	FirstSeen time.Time `json:"first_seen"`
	// LastSeen is when it was last observed: the answer to "is it still happening".
	LastSeen time.Time `json:"last_seen"`
	// ResolvedAt is when it stopped being observed, nil while it is still active.
	ResolvedAt *time.Time `json:"resolved_at,omitempty"`
	// Occurrences is how many observations found it present, counting the one that opened the row.
	Occurrences int64 `json:"occurrences"`
}

// Active reports whether this failure was still happening when it was last observed.
func (f Failure) Active() bool { return f.ResolvedAt == nil }

// ObservationWindow is one continuous run of the observer.
type ObservationWindow struct {
	ID             int64     `json:"id,omitempty"`
	StartedAt      time.Time `json:"started_at"`
	Until          time.Time `json:"until"`
	Sweeps         int64     `json:"sweeps"`
	DegradedSweeps int64     `json:"degraded_sweeps,omitempty"`
	Detail         string    `json:"detail,omitempty"`
}

// CoverageGap is a stretch of time no observer was recording — a literal gap between two runs of the
// observer, not an inference. Failures that began and ended inside it were never seen.
type CoverageGap struct {
	From time.Time `json:"from"`
	To   time.Time `json:"to"`
}

// Duration is how long the gap lasted.
func (g CoverageGap) Duration() time.Duration { return g.To.Sub(g.From) }

// Coverage is what the observer was doing over the period an answer describes. It travels with every
// answer rather than being available on request, because an empty failure list means "nothing broke"
// only when something was watching (ADR-0074's consequences).
type Coverage struct {
	Since          time.Time           `json:"since"`
	Until          time.Time           `json:"until"`
	Windows        []ObservationWindow `json:"windows"`
	Gaps           []CoverageGap       `json:"gaps"`
	DegradedSweeps int64               `json:"degraded_sweeps,omitempty"`
	Detail         string              `json:"detail,omitempty"`
}

// Complete reports whether the ledger can be read at face value for this period.
func (c Coverage) Complete() bool { return len(c.Gaps) == 0 && c.DegradedSweeps == 0 }

// Observed reports whether anything was watching at all over this period.
func (c Coverage) Observed() bool { return len(c.Windows) > 0 }

// FailureReport is one answer from the ledger: the rows, and the coverage behind them. It is rows
// and NOT groups — grouping by shared reason is a presentation heuristic the human listing applies,
// and ADR-0074 §5 keeps it out of the wire format so a caller correlates on its own terms.
type FailureReport struct {
	Failures []Failure `json:"failures"`
	Coverage Coverage  `json:"coverage"`
}

// FailureQuery narrows the failure listing. A zero value lists every ACTIVE failure across
// everything Burrow manages — the question a reader has during an incident, which is "what is
// broken", not "what ever broke".
type FailureQuery struct {
	// Kind restricts to one class of object (app, addon, backup, exposure); empty matches any.
	Kind FailureKind
	// Name restricts to one object name; empty matches any.
	Name string
	// Env restricts to one environment; empty matches any.
	Env string
	// Reason restricts to one reason from the ledger's closed vocabulary; empty matches any.
	Reason string
	// Since bounds the answer to failures last seen within this much of now and widens it to
	// history. It is sent as a duration and resolved against the CONTROL PLANE's clock, which is the
	// clock that wrote the rows.
	Since time.Duration
	// All widens the answer to resolved failures with no time bound beyond the ledger's retention.
	All bool
	// Limit caps the rows returned. Zero applies the server's default cap.
	Limit int
}

// Failures lists what is broken across everything Burrow manages, with the observation coverage
// behind the answer (ADR-0074 §8). It is read-only, it reads the ledger rather than the cluster, and
// it never claims a cause: two rows sharing a reason and a minute are a correlation the caller may
// act on, not a diagnosis Burrow asserts.
func (c *Client) Failures(ctx context.Context, f FailureQuery) (FailureReport, error) {
	q := url.Values{}
	if f.Kind != "" {
		q.Set("kind", f.Kind)
	}
	if f.Name != "" {
		q.Set("name", f.Name)
	}
	if f.Env != "" {
		q.Set("env", f.Env)
	}
	if f.Reason != "" {
		q.Set("reason", f.Reason)
	}
	if f.Since > 0 {
		q.Set("since", f.Since.String())
	}
	if f.All {
		q.Set("all", "true")
	}
	if f.Limit > 0 {
		q.Set("limit", strconv.Itoa(f.Limit))
	}
	path := "/v1/failures"
	if enc := q.Encode(); enc != "" {
		path += "?" + enc
	}
	var out FailureReport
	err := c.do(ctx, http.MethodGet, path, nil, &out)
	return out, err
}

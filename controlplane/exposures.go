// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package controlplane

import (
	"context"
	"log/slog"
	"time"
)

// Exposure is the registry's record that an app was made reachable at a host: what Burrow was told
// to route, kept alongside the releases and add-ons it already records.
//
// It exists because ADR-0074 §6 asks a question that cannot be answered from the cluster: an
// exposure whose Ingress is gone looks, from the cluster side, like an app that was never exposed.
// The Ingress itself carried the whole of that intent until now, so deleting it deleted the evidence
// that it was ever supposed to be there. Whether the exposure is currently WORKING stays a live read
// (Reachability, ExposureStatus) — this row records only what was asked for, in keeping with the
// split ADR-0074 §1 draws between intent and observed state.
type Exposure struct {
	// App is the exposed application.
	App string `json:"app"`
	// Environment is the canonical environment the exposure was made in ("prod" for the default
	// one), because the same app in two environments is exposed at two different hosts.
	Environment string `json:"environment,omitempty"`
	// Host is the external hostname routed to the app.
	Host string `json:"host"`
	// Port is the app's container port the Service forwards to.
	Port int32 `json:"port"`
	// TLS records whether a certificate was requested for Host, which is what makes "the Ingress is
	// there but the certificate never issued" a failure Burrow can name rather than guess at.
	TLS bool `json:"tls,omitempty"`
	// CreatedAt is when the exposure was recorded, read from the injected clock.
	CreatedAt time.Time `json:"created_at,omitempty"`
}

// recordExposure stores the intent behind a successful expose, best-effort. A failure is logged and
// swallowed for the same reason a failed audit append is (ADR-0027): the cluster has already been
// changed, and failing the call afterwards would report a working exposure as broken. The cost of
// losing the row is that §6's missing-Ingress check does not cover this app until it is exposed
// again — a gap in observation, not an inconsistency in the cluster.
func (e *Engine) recordExposure(ctx context.Context, ex Exposure) {
	ex.CreatedAt = e.clock.Now()
	if err := e.db.RecordExposure(ctx, ex); err != nil {
		slog.WarnContext(ctx, "recording the exposure in the registry failed",
			"app", ex.App, "env", ex.Environment, "host", ex.Host, "error", err)
	}
}

// forgetExposure removes the recorded intent behind an app's exposure, best-effort — the counterpart
// of recordExposure, called when the routing is deliberately torn down. Leaving the row behind would
// be worse than losing it: the observer would report a missing Ingress for an exposure the operator
// removed on purpose, which is the false positive most likely to teach a reader to ignore the
// surface.
func (e *Engine) forgetExposure(ctx context.Context, app, env string) {
	if err := e.db.DeleteExposure(ctx, app, envName(env)); err != nil {
		slog.WarnContext(ctx, "removing the recorded exposure from the registry failed",
			"app", app, "env", envName(env), "error", err)
	}
}

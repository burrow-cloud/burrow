// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package controlplane

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"
)

// AuditOutcome is what happened to a guarded mutating operation once the guardrail had its
// say (ADR-0027). It is the categorical fact a reviewer reads off the trail.
type AuditOutcome string

const (
	// AuditAllowed: the guardrail allowed the operation (allow disposition, or a confirm
	// disposition the caller confirmed). It is recorded before the operation executes; the
	// execution outcome is a second row.
	AuditAllowed AuditOutcome = "allowed"
	// AuditHeld: a confirm-disposition guardrail held the operation for confirmation and the
	// caller did not confirm. The operation did NOT execute. A later confirmed run is a
	// separate row.
	AuditHeld AuditOutcome = "held"
	// AuditDenied: a deny-disposition guardrail refused the operation outright. It did NOT
	// execute.
	AuditDenied AuditOutcome = "denied"
	// AuditExecuted: the operation was allowed-or-confirmed and ran to success.
	AuditExecuted AuditOutcome = "executed"
	// AuditFailed: the operation was allowed-or-confirmed and ran, but its execution errored.
	// The error is summarized in AuditEntry.Result; a distinct outcome keeps "it ran and
	// broke" readable apart from "it ran and worked" (ADR-0027).
	AuditFailed AuditOutcome = "failed"
)

// AuditEntry is one append-only record of a guarded mutating operation and the guardrail
// decision that applied (ADR-0027). It is written at the control-plane boundary — the single
// choke point that holds both the credentials and the decision.
//
// Args is redacted by construction: it carries only safe metadata (app/host/addon name, image
// reference, replica count, env/secret key NAMES) and never an env value or any secret value.
// Callers build it through the per-operation allowlist, never by serializing a raw request.
type AuditEntry struct {
	// ID is the store-assigned row identity (0 before it is persisted). Newest rows have the
	// largest IDs.
	ID int64 `json:"id,omitempty"`
	// Timestamp comes from the injected clock, never ambient time (ADR-0010).
	Timestamp time.Time `json:"timestamp"`
	// Operation is the logical operation: deploy, scale, rollback, app_delete, expose,
	// dns_write, dns_delete, addon_install, addon_remove.
	Operation string `json:"operation"`
	// Target is what the operation acted on: an app, a host, or an add-on name. It may be
	// empty for an operation that names nothing.
	Target string `json:"target,omitempty"`
	// Args is the redacted, per-operation allowlist of salient parameters. Never secret values.
	Args map[string]string `json:"args,omitempty"`
	// GuardrailCode is the guardrail that applied (e.g. app.expose_public). Empty on an execution
	// row, which is the second half of a trail whose decision row already named the guardrail.
	GuardrailCode string `json:"guardrail_code,omitempty"`
	// Disposition is the guardrail's effective disposition (allow/confirm/deny) at decision time.
	Disposition string `json:"disposition,omitempty"`
	// Outcome is the categorical result (see AuditOutcome).
	Outcome AuditOutcome `json:"outcome"`
	// Result is the execution result: empty on success, or a short error summary on a failed
	// outcome. It never carries a secret value.
	Result string `json:"result,omitempty"`
	// Caller is the authenticated caller. Identity is coarse until an auth model exists
	// (ADR-0027): today it is a constant naming the control-plane boundary.
	Caller string `json:"caller,omitempty"`
	// Principal is the acting identity — the actor behind the operation, distinct from Caller
	// (the control-plane boundary that authenticated the request). Today every agent shares one
	// ServiceAccount, so it is a constant ("shared-agent"); the principal seam (ADR-0038) is
	// where a future TokenReview→SSO identity resolution fills in a real per-user value, so
	// attribution becomes additive rather than a migration of past rows' meaning.
	Principal string `json:"principal,omitempty"`
	// ClientVersion is the release version of the client (CLI or MCP server) that drove the
	// operation, read from the X-Burrow-Client-Version handshake header (ADR-0039). It records which
	// client acted, next to the principal (who) and the server's own version — so version skew is
	// legible after the fact. Empty for a pre-handshake client that sent no version. The json tag must
	// match the client-side AuditEntry tag exactly (the struct crosses the API), or the field would
	// silently drop.
	ClientVersion string `json:"client_version,omitempty"`
}

// AuditFilter narrows an audit query. A zero value matches everything (subject to Limit).
type AuditFilter struct {
	// App restricts to rows whose target is this app/host/addon name; empty matches any.
	App string
	// Operation restricts to one operation name; empty matches any.
	Operation string
	// Outcome restricts to one outcome; empty matches any.
	Outcome AuditOutcome
	// Limit caps the number of rows returned, newest first. Zero or negative applies the
	// store's default cap.
	Limit int
}

// auditCaller is the coarse caller identity recorded until an authentication model exists
// (ADR-0027). The control plane authenticates with a single API token today, so every guarded
// operation is attributed to the control-plane boundary; the schema reserves room to enrich
// this without a migration of meaning.
const auditCaller = "control-plane"

// auditPrincipal is the acting identity recorded until per-user identities exist (ADR-0038).
// All agents share one `burrow-agent` ServiceAccount today, so every guarded operation is
// attributed to this shared-agent constant; the principal seam (principalFromContext) is where
// a later TokenReview→SSO mapping fills in a real per-user value.
const auditPrincipal = "shared-agent"

// principalFromContext is the caller-identity seam (ADR-0038): it resolves the acting principal
// (the actor) from the request context. Today it returns the shared-agent constant, because all
// agents share one ServiceAccount and there is no auth model to key an identity on. This is the
// single point where a future TokenReview→SSO identity resolution plugs in: middleware in the
// API layer would put the resolved SSO identity on the request context (see the note in
// controlplane/api/api.go), and this seam would read and return it. Kept a package var so a test
// or the managed product can substitute a resolver without touching call sites.
var principalFromContext = func(ctx context.Context) string {
	return auditPrincipal
}

// clientVersionContextKey keys the acting client's version on a request context. It is an unexported
// type so no other package can collide with it.
type clientVersionContextKey struct{}

// ContextWithClientVersion returns a context carrying the acting client's release version for the
// audit record (ADR-0039). The API layer sets it from the X-Burrow-Client-Version header; the engine
// reads it back via clientVersionFromContext when it records a guarded operation. An empty version
// leaves the context unchanged, so a pre-handshake client records no version.
func ContextWithClientVersion(ctx context.Context, version string) context.Context {
	if version == "" {
		return ctx
	}
	return context.WithValue(ctx, clientVersionContextKey{}, version)
}

// clientVersionFromContext returns the acting client's version placed on ctx by
// ContextWithClientVersion, or empty when none was set (a pre-handshake client, or a code path that
// does not carry one, such as an internal reconcile). Unlike principalFromContext this is a real
// resolver, not a stub — the client version is available today via the handshake header.
func clientVersionFromContext(ctx context.Context) string {
	v, _ := ctx.Value(clientVersionContextKey{}).(string)
	return v
}

// The audit operation names, referenced symbolically rather than as scattered string literals.
const (
	auditOpDeploy       = "deploy"
	auditOpScale        = "scale"
	auditOpAutoscale    = "autoscale"
	auditOpRollback     = "rollback"
	auditOpAppDelete    = "app_delete"
	auditOpExpose       = "expose"
	auditOpDNSWrite     = "dns_write"
	auditOpDNSDelete    = "dns_delete"
	auditOpAddonInstall = "addon_install"
	auditOpAddonRemove  = "addon_remove"
	auditOpAddonAttach  = "addon_attach"
	auditOpAddonDetach  = "addon_detach"
	auditOpAddonBackup  = "addon_backup"
	auditOpAddonRestore = "addon_restore"
)

// Audit returns audit rows matching filter, newest first (ADR-0027). It is a read-only,
// guarded control-plane operation: the agent and the operator may review the trail, but no
// API path writes to or alters it. The rows never carry a secret value — they were redacted at
// write time.
func (e *Engine) Audit(ctx context.Context, filter AuditFilter) ([]AuditEntry, error) {
	entries, err := e.db.Audit(ctx, filter)
	if err != nil {
		return nil, fmt.Errorf("audit: %w", err)
	}
	return entries, nil
}

// recordAudit appends one audit row, best-effort. A failed append is logged and swallowed: the
// record is best-effort relative to the action, so losing a row is a degradation, not a reason
// to fail the underlying operation (ADR-0027). The timestamp and caller are filled here so every
// call site stays uniform.
func (e *Engine) recordAudit(ctx context.Context, entry AuditEntry) {
	entry.Timestamp = e.clock.Now()
	if entry.Caller == "" {
		entry.Caller = auditCaller
	}
	if entry.Principal == "" {
		entry.Principal = principalFromContext(ctx)
	}
	if entry.ClientVersion == "" {
		entry.ClientVersion = clientVersionFromContext(ctx)
	}
	if err := e.db.AppendAudit(ctx, entry); err != nil {
		slog.WarnContext(ctx, "audit append failed",
			"operation", entry.Operation, "target", entry.Target, "outcome", entry.Outcome, "error", err)
	}
}

// auditKeys renders a map's keys as a sorted, comma-joined string for an audit row. It records
// the KEY NAMES only — the redaction boundary (ADR-0027): an env or secret map's values never
// reach the audit log. An empty map yields "".
func auditKeys(m map[string]string) string {
	if len(m) == 0 {
		return ""
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return strings.Join(keys, ",")
}

// recordDecision records the guardrail decision for a guarded operation from the error its
// evaluation returned, then returns that error unchanged so a call site reads
// `if err := e.recordDecision(...); err != nil { return ... }`. A nil error records allowed; a
// confirmation hold records held; a refusal records denied. The guardrail code and disposition
// come from the decision, falling back to code when the operation proceeded (a nil error carries
// no GuardrailError to read the effective code from).
func (e *Engine) recordDecision(ctx context.Context, op, target string, args map[string]string, code GuardrailCode, guardErr error) error {
	entry := AuditEntry{Operation: op, Target: target, Args: args, GuardrailCode: string(code)}
	if g, ok := AsGuardrail(guardErr); ok {
		entry.GuardrailCode = string(g.Code)
		if g.NeedsConfirmation {
			entry.Outcome, entry.Disposition = AuditHeld, string(DispositionConfirm)
		} else {
			entry.Outcome, entry.Disposition = AuditDenied, string(DispositionDeny)
		}
	} else {
		entry.Outcome, entry.Disposition = AuditAllowed, string(DispositionAllow)
	}
	e.recordAudit(ctx, entry)
	return guardErr
}

// recordExecution records the execution outcome of an operation the guardrail allowed: a nil
// execErr records executed, a non-nil one records failed with a short error summary. The
// guardrail decision was already recorded by recordDecision, so this row carries no guardrail
// code — it is the second half of the trail ("requested" → "executed").
func (e *Engine) recordExecution(ctx context.Context, op, target string, args map[string]string, execErr error) {
	entry := AuditEntry{Operation: op, Target: target, Args: args, Outcome: AuditExecuted}
	if execErr != nil {
		entry.Outcome = AuditFailed
		entry.Result = execErr.Error()
	}
	e.recordAudit(ctx, entry)
}

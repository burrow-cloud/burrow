// SPDX-License-Identifier: FSL-1.1-ALv2
// Copyright 2026 Nicholas Phillips

package controlplane

import (
	"fmt"
	"regexp"
	"time"
)

// App is a deployable application: the logical unit an agent operates. A single App
// has a history of Releases; the most recent successful one is what is running.
type App struct {
	// Name identifies the app within the control plane and names its Kubernetes
	// workload. It must be a DNS-1123 label (lowercase alphanumerics and '-').
	Name string
}

// dns1123Label matches a Kubernetes DNS-1123 label: lowercase alphanumerics and '-',
// not starting or ending with '-', at most 63 characters.
var dns1123Label = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

const maxNameLen = 63

// Validate reports whether the app name is a usable Kubernetes resource name.
func (a App) Validate() error {
	switch {
	case a.Name == "":
		return fmt.Errorf("app name is empty")
	case len(a.Name) > maxNameLen:
		return fmt.Errorf("app name %q is longer than %d characters", a.Name, maxNameLen)
	case !dns1123Label.MatchString(a.Name):
		return fmt.Errorf("app name %q is not a valid DNS-1123 label", a.Name)
	}
	return nil
}

// RestartedAtAnnotation is the pod-template annotation Burrow bumps to a fresh timestamp to
// force a rolling update when something the pod reads only at start changes — notably a secret
// value under an existing key, which does not otherwise mutate the template (ADR-0028).
const RestartedAtAnnotation = "burrow.cloud/restarted-at"

// AppSecretName is the per-app Kubernetes Secret that holds an app's secret env (ADR-0028): one
// object per app in the app namespace, keys = env-var names, values = secret values. The values
// live only here — never in Postgres, the Deployment spec, or over MCP/the API (ADR-0004). The
// name is derived from the app (a DNS-1123 label, so the result is always a valid Secret name),
// so every layer computes it the same way rather than passing it around.
func AppSecretName(app string) string {
	return "burrow-app-" + app + "-secrets"
}

// envKey matches a conventional environment variable name: a letter or underscore followed
// by letters, digits, or underscores. It rejects names a shell or container runtime would
// reject, keeping the non-secret config store (ADR-0028) to well-formed keys.
var envKey = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// validateEnvKey reports whether key is a usable environment variable name.
func validateEnvKey(key string) error {
	if key == "" {
		return fmt.Errorf("env key is empty")
	}
	if !envKey.MatchString(key) {
		return fmt.Errorf("env key %q is not a valid environment variable name", key)
	}
	return nil
}

// ReleaseStatus is the lifecycle state of a Release.
type ReleaseStatus string

const (
	// ReleasePending is a release that has been recorded but not yet rolled out.
	ReleasePending ReleaseStatus = "pending"
	// ReleaseDeployed is a release that rolled out successfully and is (or was) running.
	ReleaseDeployed ReleaseStatus = "deployed"
	// ReleaseFailed is a release whose rollout did not succeed.
	ReleaseFailed ReleaseStatus = "failed"
	// ReleaseSuperseded is a release replaced by a newer one (deploy or rollback).
	ReleaseSuperseded ReleaseStatus = "superseded"
)

// Valid reports whether s is a known ReleaseStatus.
func (s ReleaseStatus) Valid() bool {
	switch s {
	case ReleasePending, ReleaseDeployed, ReleaseFailed, ReleaseSuperseded:
		return true
	default:
		return false
	}
}

// Release is one immutable deploy of an App: the unit recorded in the deploy history
// and the handle rollback redeploys. It captures exactly what was asked for — the
// pullable image, the resolved digest, and the small metadata that travels over MCP
// (env, command, replica count) — never any code (ADR-0004).
type Release struct {
	// ID is the control-plane-assigned identifier for this release. It is minted by
	// the deploy engine, not here, so the domain type stays free of ambient identity.
	ID string `json:"id"`
	// App is the name of the App this release belongs to.
	App string `json:"app"`
	// Image is the pullable container image reference the deploy named (ADR-0007).
	Image string `json:"image"`
	// Digest is the content digest the registry resolved Image to, when known
	// (e.g. "sha256:..."). It is what makes a rollback deterministic.
	Digest string `json:"digest,omitempty"`
	// Env is the environment passed to the workload.
	Env map[string]string `json:"env,omitempty"`
	// Command overrides the image's default command, when set.
	Command []string `json:"command,omitempty"`
	// MetricsPort, when positive, is the container port the app serves Prometheus metrics on.
	// The deploy annotates the pod (prometheus.io/scrape, /port, /path) so the metrics add-on's
	// scraper discovers and scrapes /metrics on it. Zero means no metrics annotations (ADR-0026).
	MetricsPort int32 `json:"metrics_port,omitempty"`
	// Replicas is the desired replica count.
	Replicas int32 `json:"replicas"`
	// Status is the lifecycle state of this release.
	Status ReleaseStatus `json:"status"`
	// Supersedes is the ID of the release this one replaced, if any — the chain that
	// lets rollback walk back to a prior known-good release.
	Supersedes string `json:"supersedes,omitempty"`
	// CreatedAt is when the control plane recorded this release, read from the
	// injected clock (never from ambient time).
	CreatedAt time.Time `json:"created_at"`
}

// Validate reports whether the release is well-formed enough to act on. It checks the
// fields a caller supplies; the engine-assigned ID is not required here so the same
// validation can run before an ID is minted.
func (r Release) Validate() error {
	if err := (App{Name: r.App}).Validate(); err != nil {
		return fmt.Errorf("release app: %w", err)
	}
	if r.Image == "" {
		return fmt.Errorf("release image reference is empty")
	}
	if r.Replicas < 0 {
		return fmt.Errorf("release replicas %d is negative", r.Replicas)
	}
	if r.Status != "" && !r.Status.Valid() {
		return fmt.Errorf("release status %q is not valid", r.Status)
	}
	return nil
}

// Policy is the guardrail configuration the control plane evaluates dangerous
// operations against (ADR-0006). This type carries the limits; the evaluation that
// gates, constrains, or refuses an operation against them is the deploy engine's job.
type Policy struct {
	// Dispositions configures how each guardrail is enforced — allow, confirm, or deny
	// (ADR-0020), keyed by GuardrailCode. A guardrail with no entry here defaults to deny:
	// the safe default.
	Dispositions map[GuardrailCode]Disposition
	// MaxReplicas is the largest replica count permitted before the replica_ceiling
	// guardrail's disposition applies. Must be positive.
	MaxReplicas int32
}

// DefaultPolicy returns the conservative starting guardrail policy (ADR-0020): a modest
// replica ceiling that denies oversized scale-ups, and scale-to-zero held for confirmation
// — recoverable with an explicit confirm rather than silently allowed or hard-denied. The
// operator can relax or tighten any of these with `guard set`.
func DefaultPolicy() Policy {
	return Policy{
		Dispositions: map[GuardrailCode]Disposition{
			GuardrailReplicaCeiling: DispositionDeny,
			GuardrailScaleToZero:    DispositionConfirm,
			GuardrailExposePublic:   DispositionConfirm,
			GuardrailDNSWrite:       DispositionConfirm,
			GuardrailDNSDelete:      DispositionConfirm,
			GuardrailAddonInstall:   DispositionConfirm,
			GuardrailAddonRemove:    DispositionConfirm,
			GuardrailAppDelete:      DispositionConfirm,
			// Rollback is a recovery action, so it is allowed by default — an agent should be
			// able to restore a broken app without friction. An operator who wants sign-off can
			// raise it to confirm or deny with `guard set rollback ...`.
			GuardrailRollback: DispositionAllow,
		},
		MaxReplicas: 50,
	}
}

// With returns a copy of the policy with code's disposition set, leaving the receiver
// unchanged — the basis for `guard set` and for composing policies.
func (p Policy) With(code GuardrailCode, d Disposition) Policy {
	next := make(map[GuardrailCode]Disposition, len(p.Dispositions)+1)
	for k, v := range p.Dispositions {
		next[k] = v
	}
	next[code] = d
	return Policy{Dispositions: next, MaxReplicas: p.MaxReplicas}
}

// Validate reports whether the policy is internally coherent.
func (p Policy) Validate() error {
	if p.MaxReplicas <= 0 {
		return fmt.Errorf("policy MaxReplicas %d must be positive", p.MaxReplicas)
	}
	for code, d := range p.Dispositions {
		if !d.Valid() {
			return fmt.Errorf("policy disposition %q for guardrail %q is not valid", d, code)
		}
	}
	return nil
}

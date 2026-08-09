// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package controlplane

import (
	"fmt"
	"strings"
	"time"
)

// The types below are the structured inputs and outputs of the engine's operations.
// Every operation returns a result an agent can reason over — what changed and, where
// relevant, the handle to undo it (ADR-0006) — rather than prose.

// DeployRequest is the small, code-free description of a deploy: a pullable image plus
// metadata (ADR-0004). No code travels here. Env is deliberately absent: an app's
// non-secret config is an independently-managed, app-global store, sourced at apply time
// rather than passed per deploy (ADR-0028) — set it with SetConfig before deploying.
type DeployRequest struct {
	App string `json:"app"`
	// Env is the environment to deploy into (ADR-0035 phase 2b): empty or "prod" targets the default
	// environment's namespace — the one install created (ADR-0067 §2) — and a name added later targets
	// that environment's namespace. An unregistered name is an error.
	Env     string   `json:"env,omitempty"`
	Image   string   `json:"image"`
	Command []string `json:"command,omitempty"`
	// MetricsPort, when positive, annotates the deployed pod so the metrics add-on scrapes
	// /metrics on this container port (ADR-0026). Zero adds no annotations.
	MetricsPort int32 `json:"metrics_port,omitempty"`
	Replicas    int32 `json:"replicas"`
	// Confirm acknowledges a guardrail whose disposition is confirm, letting the operation
	// proceed past it (ADR-0020). It has no effect on a guardrail set to deny.
	Confirm bool `json:"confirm,omitempty"`
	// NoWait asks the deploy to answer at submission rather than at outcome: the Deployment is
	// written and the call returns without observing whether the new pods became ready (ADR-0092 §3).
	// The result then carries no Rollout, which is what says the outcome is unknown.
	//
	// THE DEFAULT IS TO WAIT, and the zero value is the default deliberately. A caller that predates
	// this field — an older CLI, a script posting the request's old wire shape — sends nothing and
	// gets the wait, because the report that misled is the one that answered early (issue #546).
	//
	// IT DOES NOT SKIP A WAIT SOMETHING ELSE NEEDS. An app with a `post-deploy` hook or a derived
	// dependency to check still settles, because those features are defined in terms of the rollout's
	// outcome (ADR-0072 §4, ADR-0076 §4); NoWait only declines to make the observation FOR THE REPORT.
	NoWait bool `json:"no_wait,omitempty"`
	// Progress receives the deploy's stage transitions as they happen (issue #480), so a caller can
	// say what the deploy is doing across the ten to twenty seconds it takes rather than going quiet.
	// Nil asks for nothing and is the default: the API's JSON decode leaves it nil, so the request's
	// wire shape is unchanged and no caller that did not ask pays anything.
	//
	// It is called SYNCHRONOUSLY on the deploying goroutine, in stage order, so a slow reporter slows
	// the deploy. A reporter writes and returns.
	Progress func(DeployEvent) `json:"-"`
}

// DeployResult reports the outcome of a successful deploy.
type DeployResult struct {
	// Release is the new release that is now running.
	Release Release `json:"release"`
	// SupersededReleaseID is the release this deploy replaced, or "" if it was the
	// first deploy of the app. It is the handle a rollback would return to.
	SupersededReleaseID string `json:"superseded_release_id,omitempty"`
	// Hints are non-blocking notes about the deploy the agent can reason over (ADR-0052 §8): today,
	// a nudge toward semver when the deployed tag cannot be classified for auto-update. They never
	// gate or fail the deploy — any reference deploys (ADR-0007). Empty when there is nothing to note.
	Hints []string `json:"hints,omitempty"`
	// Dependencies is what the deploy-time dependency check found (ADR-0076 §4): for each thing
	// Burrow provisioned for this app — a database it attached, a port it published — whether the app
	// could actually reach it from inside its own container after the deploy.
	//
	// IT IS A REPORT AND NEVER A VERDICT ON THE DEPLOY. A failed entry here sits on a DeployResult
	// that succeeded, because it does: the release is deployed, the rollout happened, and Burrow does
	// not roll back by itself (ADR-0072 §6). Empty when Burrow provisioned nothing it can check, or
	// when the check is turned off for this app.
	Dependencies []DependencyResult `json:"dependencies,omitempty"`
	// Rollout is what the deploy observed of its own rollout (ADR-0092): whether the new replicas
	// became ready, and if they did not, why not. It is the field that makes the rest of this result
	// mean what a reader takes it to mean — Release.Status and SupersededReleaseID describe what
	// Burrow RECORDED, and until this said so nothing described what the cluster then did with it
	// (issue #546).
	//
	// NIL MEANS UNOBSERVED, never "fine". A deploy asked not to wait (DeployRequest.NoWait) makes no
	// observation, so there is nothing truthful to put here; a caller renders that as an unknown
	// outcome and not as a success.
	Rollout *RolloutReport `json:"rollout,omitempty"`
}

// RolloutReport is the deploy's own account of its rollout — the answer to "is the image I just
// deployed actually serving?" (ADR-0092 §2).
//
// IT IS NOT RolloutOutcome, and the difference is the audience. RolloutOutcome is what a
// `post-deploy` hook is told (ADR-0072 §4): it is copied into a Job's environment, where it would
// persist in a Kubernetes object, so it deliberately carries no application output. This travels
// back over the API to the caller that is waiting on the answer, is rendered once, and is discarded
// — the same contract `burrow app status` already reads under, which is why it can carry Issue.
type RolloutReport struct {
	// Settled reports that the newest revision finished rolling out and is serving — the completion
	// test `kubectl rollout status` uses. False is every other verdict and always carries a Reason.
	Settled bool `json:"settled"`
	// Reason names why the rollout did not settle, from the closed vocabulary LedgerReasons()
	// enumerates (ADR-0074 §2), and is empty exactly when Settled is true. ReasonDeadlineExceeded is
	// its declared backstop and means something different from the rest: the bound ran out with the
	// rollout still progressing and no pod reporting anything blocking, which is "not yet" rather
	// than "not going to".
	Reason string `json:"reason,omitempty"`
	// Detail is one Burrow-authored line describing what the wait observed — replica counts, a pod's
	// phase, a container's waiting reason.
	Detail string `json:"detail,omitempty"`
	// Issue is the live status surface's actionable explanation of the same wedged workload
	// (ADR-0074 §2) — the pod's own reason, in the words `burrow app status` would use for it, so a
	// failed deploy explains itself without a second call. Empty when the surface found nothing more
	// specific than the wait already reported.
	Issue string `json:"issue,omitempty"`
}

// Failed reports that the deploy observed a rollout and the verdict was negative. A nil report is
// not a failure: it is an unobserved rollout, which is a different thing and reads differently.
func (r *RolloutReport) Failed() bool { return r != nil && !r.Settled }

// BuildRequest is the code-free description of an in-cluster build-then-deploy (ADR-0053): the app,
// the git source to clone and build inside the cluster, and the target image reference the built
// image is pushed to. No code travels here (ADR-0004) — only the git reference and the target
// reference; the builder clones the source from git inside the cluster. On success the built image
// rejoins the existing guarded deploy path (ADR-0053 §4), so a build is a front-end that ends where
// deploy begins.
type BuildRequest struct {
	App string `json:"app"`
	// Env is the environment to deploy the built image into (ADR-0035): empty or "prod" targets the
	// default environment, a name added later targets that environment. An unregistered name is an
	// error, surfaced by the deploy the build hands off to.
	Env string `json:"env,omitempty"`
	// Source is the git reference the builder clones and checks out inside the cluster.
	Source SourceRef `json:"source"`
	// TargetImage is the pullable repo:tag reference the built image is pushed to; the resulting deploy
	// pins the digest the builder returns. In this phase it is supplied explicitly; the optional
	// in-cluster registry as the zero-config default push target (ADR-0053 §5) is a later phase.
	TargetImage string `json:"target_image"`
	// Confirm acknowledges the app.deploy guardrail whose disposition is confirm, letting the deploy
	// the build hands off to proceed past it (ADR-0020). It has no effect on a guardrail set to deny.
	Confirm bool `json:"confirm,omitempty"`
	// Progress receives the build's stage transitions as they happen (issue #503) — its own stages
	// (BuildStages) and then the deploy stages it hands off to, as one continuous sequence. Nil asks
	// for nothing and is the default: the API's JSON decode leaves it nil, so the request's wire shape
	// is unchanged and no caller that did not ask pays anything.
	//
	// It is called SYNCHRONOUSLY on the building goroutine, in stage order, so a slow reporter slows
	// the build. A reporter writes and returns.
	Progress func(DeployEvent) `json:"-"`
}

// BuildResult reports the outcome of a successful build-then-deploy (ADR-0053 §4): the digest of the
// image the builder produced and the deploy that shipped it. Because the build ends where deploy
// begins, Deploy carries the same new release, rollback handle, and hints an explicit deploy returns —
// downstream cannot tell a built image apart from an externally-built one except in provenance.
type BuildResult struct {
	// Digest is the content digest of the built image (e.g. "sha256:..."), the immutable identity the
	// deployed release pins.
	Digest string `json:"digest"`
	// Deploy is the guarded deploy the built image flowed into (ADR-0053 §4).
	Deploy DeployResult `json:"deploy"`
}

// RunRequest is a one-off command to run in an app's own current image and environment (ADR-0048).
// It carries only the command (and its arguments) — the image, config, and secrets come from the
// app, resolved server-side. No code travels here (ADR-0004): the command names an entrypoint that
// already exists in the app's image.
type RunRequest struct {
	App string `json:"app"`
	// Env is the environment whose namespace the command runs in (ADR-0035): empty or "prod" targets
	// the default environment, a name added later targets that environment.
	Env string `json:"env,omitempty"`
	// Command is the command and its arguments to run, as an argv. It must be non-empty.
	Command []string `json:"command"`
	// TTLSeconds overrides how long the finished Job lingers before Kubernetes garbage-collects it
	// (ttlSecondsAfterFinished; ADR-0048 §7). Nil applies the default (one hour); a value — including
	// 0, delete as soon as the output is captured — overrides it. A negative value is rejected.
	TTLSeconds *int32 `json:"ttl_seconds,omitempty"`
	// Confirm acknowledges the app.run guardrail whose disposition is confirm, letting the command
	// proceed past it (ADR-0020). It has no effect on a guardrail set to deny.
	Confirm bool `json:"confirm,omitempty"`
}

// RunResult reports the outcome of a one-off command (ADR-0048 §3). A non-zero ExitCode is a normal
// structured outcome the agent reasons over, not a transport failure. Stdout carries the command's
// captured output — Kubernetes returns a pod's stdout and stderr as one interleaved stream, so the
// output lands in Stdout; Stderr is reserved for a future separation.
type RunResult struct {
	App      string `json:"app"`
	ExitCode int    `json:"exit_code"`
	Stdout   string `json:"stdout,omitempty"`
	Stderr   string `json:"stderr,omitempty"`
	// TimedOut reports the command did not finish within the run window (10 minutes).
	TimedOut bool `json:"timed_out,omitempty"`
}

// StatusResult is the combined control-plane and cluster view of an app.
type StatusResult struct {
	App string `json:"app"`
	// HasRelease reports whether the control plane has any release recorded for the
	// app; Release holds the most recent one when true.
	HasRelease bool    `json:"has_release"`
	Release    Release `json:"release,omitempty"`
	// Running reports whether a workload currently exists in the cluster; Workload
	// holds its observed state when true.
	Running  bool           `json:"running"`
	Workload WorkloadStatus `json:"workload,omitempty"`
	// Failures is this app's recent failure history from the ledger (ADR-0074 §8): the failures
	// observed for it over the last StatusFailureLookback, oldest first, resolved ones included.
	// Workload above is the live present tense; this is the part nothing else can reconstruct —
	// whether the app crash-looped at 02:00 and recovered, and when it started.
	Failures []Failure `json:"failures,omitempty"`
	// Coverage is what the observer was doing over that same window. It is never omitted, because
	// an empty Failures list means "nothing broke" only if something was watching.
	Coverage Coverage `json:"coverage"`
}

// ScaleResult reports the outcome of a scale.
// ExposeRequest describes making an app reachable at a hostname (ADR-0018).
type ExposeRequest struct {
	App string `json:"app"`
	// Env is the environment whose namespace the app lives in (ADR-0035 phase 2b): empty or
	// "prod" targets the default environment, a name added later targets that environment.
	Env  string `json:"env,omitempty"`
	Host string `json:"host"`
	Port int32  `json:"port"`
	// TLS requests an HTTPS certificate for Host via cert-manager; Issuer names the
	// ClusterIssuer to use.
	TLS    bool   `json:"tls,omitempty"`
	Issuer string `json:"issuer,omitempty"`
	// Confirm acknowledges the app.expose_public guardrail so the operation proceeds past it.
	Confirm bool `json:"confirm,omitempty"`
}

// ExposeResult reports the outcome of exposing an app.
type ExposeResult struct {
	App  string `json:"app"`
	Host string `json:"host"`
	Port int32  `json:"port"`
	URL  string `json:"url"`
}

// ReachabilityResult reports whether an app is reachable at its hostname, link by link, for
// the reachability surface (ADR-0018, ADR-0022). Summary is a one-line, plain-English verdict
// for a non-expert; the fields are the full chain for advanced users and the agent.
type ReachabilityResult struct {
	App                string   `json:"app"`
	Deployed           bool     `json:"deployed"`
	Ready              bool     `json:"ready"`
	Exposed            bool     `json:"exposed"`
	Host               string   `json:"host,omitempty"`
	Address            string   `json:"address,omitempty"` // controller-assigned external address
	TLS                bool     `json:"tls"`               // the Ingress requests an HTTPS certificate
	CertReady          bool     `json:"cert_ready"`        // the requested TLS certificate has been issued
	DNSPointsAtCluster bool     `json:"dns_points_at_cluster"`
	DNSAddresses       []string `json:"dns_addresses,omitempty"`
	// Reachable is the converged verdict: every link in the chain is green and the app is live.
	Reachable bool `json:"reachable"`
	// URL is where the app is live when Reachable (https when TLS was requested, else http); it
	// is empty until Reachable.
	URL string `json:"url,omitempty"`
	// BlockedOn names the first unready link when not Reachable (e.g. "ingress controller",
	// "tls certificate", "dns"); it is empty when Reachable. It is the one link to fix next.
	BlockedOn string `json:"blocked_on,omitempty"`
	Summary   string `json:"summary"`
}

type ScaleResult struct {
	App              string `json:"app"`
	PreviousReplicas int32  `json:"previous_replicas"`
	Replicas         int32  `json:"replicas"`
}

// AutoscaleSpec is the desired autoscaling shape for an app's Deployment (ADR-0006): the replica
// band the HorizontalPodAutoscaler moves within and the resource-utilization targets it scales on.
// It is code-free metadata that travels over the API. CPUPercent is required; MemoryPercent is
// optional (0 means no memory metric).
type AutoscaleSpec struct {
	// MinReplicas is the floor the autoscaler will not scale below. Must be at least 1.
	MinReplicas int32 `json:"min_replicas"`
	// MaxReplicas is the ceiling the autoscaler will not scale above. Must be >= MinReplicas and is
	// itself bounded by the replica ceiling, an operational limit an operator sets (ADR-0068 §6).
	MaxReplicas int32 `json:"max_replicas"`
	// CPUPercent is the target average CPU utilization (1..100) the autoscaler holds the app at.
	CPUPercent int32 `json:"cpu_percent"`
	// MemoryPercent is the target average memory utilization (1..100), or 0 to add no memory metric.
	MemoryPercent int32 `json:"memory_percent,omitempty"`
}

// validate reports whether the autoscale spec is well-formed: a floor of at least one replica, a
// ceiling no lower than the floor, a CPU target in 1..100 (so CPU is always set), and a memory
// target that is either unset (0) or in 1..100.
func (s AutoscaleSpec) validate() error {
	if s.MinReplicas < 1 {
		return fmt.Errorf("min replicas %d must be at least 1", s.MinReplicas)
	}
	if s.MaxReplicas < s.MinReplicas {
		return fmt.Errorf("max replicas %d must be at least min replicas %d", s.MaxReplicas, s.MinReplicas)
	}
	if s.CPUPercent < 1 || s.CPUPercent > 100 {
		return fmt.Errorf("cpu target %d must be between 1 and 100", s.CPUPercent)
	}
	if s.MemoryPercent < 0 || s.MemoryPercent > 100 {
		return fmt.Errorf("memory target %d must be between 0 and 100 (0 leaves it unset)", s.MemoryPercent)
	}
	return nil
}

// AutoscaleResult reports the outcome of configuring autoscaling: the effective spec that was
// applied, the app and environment it acted in, and whether metrics-server is present. When it is
// absent, MetricsAvailable is false and Warning explains that the autoscaler is set but will not
// scale until metrics-server is installed — the HPA is applied regardless (its creation does not
// need metrics-server; only its scaling does).
type AutoscaleResult struct {
	App              string `json:"app"`
	Env              string `json:"env,omitempty"`
	MinReplicas      int32  `json:"min_replicas"`
	MaxReplicas      int32  `json:"max_replicas"`
	CPUPercent       int32  `json:"cpu_percent"`
	MemoryPercent    int32  `json:"memory_percent,omitempty"`
	MetricsAvailable bool   `json:"metrics_available"`
	Warning          string `json:"warning,omitempty"`
}

// RollbackOptions carries what the CALLER decided about a rollback, as distinct from what the app's
// state decides. It is a struct rather than two booleans because the two mean opposite things — one
// satisfies a guardrail, the other steps around a safety step — and a call site reading
// `Rollback(ctx, app, env, true, false)` would say neither.
type RollbackOptions struct {
	// Confirm satisfies a guardrail whose disposition holds the rollback for confirmation
	// (ADR-0020). It says nothing about hooks.
	Confirm bool
	// SkipHooks rolls back WITHOUT running the app's `pre-rollback` hook (ADR-0080 §2). It exists for
	// one situation: the hook cannot run — the image will not pull, the Job is unschedulable, the
	// command has a typo — so the abort it produces is unrelated to the schema it guards, and a
	// rollback is the one operation where waiting to fix that costs an outage.
	//
	// It is NON-DESTRUCTIVE and OPERATOR-ONLY. The hook stays configured, so the next rollback has its
	// protection intact; and `burrow-agent` compiles no flag that sets this, because deciding a safety
	// step does not apply is a judgement about the situation rather than a capability (ADR-0080 §3).
	SkipHooks bool
	// NoWait returns the rollback at submission instead of waiting for its rollout, and reports the
	// outcome as UNKNOWN rather than as good (ADR-0093 §2, ADR-0092 §3).
	//
	// IT IS A NEGATIVE FIELD ON PURPOSE. The zero value is the wait, so a caller that has never heard
	// of this — an older CLI, a script calling the route without the parameter — gets the wait, which
	// is the behaviour that stops the report misleading. Like `--wait=false` on a deploy it is absent
	// from `burrow-agent`: being told an operation worked when it did not is the agent's whole problem
	// here, and a flag that restores that is not one it should reach for.
	//
	// It does not skip a wait something else needs: an app with a `post-deploy` hook still settles,
	// because that phase is defined in terms of the rollout's outcome (ADR-0072 §4).
	NoWait bool
}

// RollbackResult reports the outcome of a rollback. A rollback is itself a forward
// deploy of a prior reference (ADR-0007), so it produces a new Release.
type RollbackResult struct {
	// Release is the new release created by the rollback (carrying the prior
	// reference) and now running.
	Release Release `json:"release"`
	// RolledBackToReleaseID is the prior release whose reference was restored.
	RolledBackToReleaseID string `json:"rolled_back_to_release_id"`
	// SupersededReleaseID is the release that was running before the rollback — the one being rolled
	// back AWAY FROM, and therefore the one still serving when the rollback's own rollout does not
	// become ready (ADR-0093 §2).
	SupersededReleaseID string `json:"superseded_release_id"`
	// Rollout is what the rollback observed of its own rollout (ADR-0093 §1): whether the restored
	// image's replicas became ready, and if they did not, why not.
	//
	// NIL MEANS UNOBSERVED, never "fine" (RollbackOptions.NoWait, or a control plane older than the
	// field). A caller renders that as an unknown outcome and not as a recovery that worked.
	Rollout *RolloutReport `json:"rollout,omitempty"`
	// Hints are non-blocking notes about the rollback, in the same shape DeployResult carries them:
	// that a `pre-rollback` hook was skipped and which command did not run (ADR-0080 §4), what the
	// rollback's settle-wait observed, and what a `post-deploy` hook made of it (ADR-0072 §4). They
	// never gate or fail the rollback — it has already happened by the time any of them exists. Empty
	// when there is nothing to note.
	Hints []string `json:"hints,omitempty"`
}

// AddProviderRequest registers a vendor credential (ADR-0023, ADR-0030). The token VALUE travels
// in this request over burrowd's authenticated, TLS-protected control-plane API; burrowd validates
// it and then writes it into the burrow-credentials Secret. The value is never logged, never stored
// in Postgres, never returned in a response, and still never carried over the agent control channel
// — provider add is a human/CLI operation, not an agent one.
type AddProviderRequest struct {
	// Name identifies the provider; empty defaults to the type.
	Name string `json:"name,omitempty"`
	// Type is the vendor this provider talks to.
	Type ProviderType `json:"type"`
	// SecretKey is the key in burrow-credentials the token is written under; empty defaults to Name.
	SecretKey string `json:"secret_key,omitempty"`
	// Token is the vendor API token VALUE. It is written to burrow-credentials after validation and
	// is never logged, stored in Postgres, or echoed back (ADR-0030).
	Token string `json:"token,omitempty"`

	// The fields below configure an OBJECT-STORAGE provider (ADR-0063), whose credential is a PAIR
	// rather than one opaque token and whose configuration names a destination. They are ignored
	// for every other provider type.

	// Endpoint is the S3-compatible API endpoint. Object storage is addressed by endpoint rather
	// than by vendor (ADR-0063 §1): the vendor is whoever answers it.
	Endpoint string `json:"endpoint,omitempty"`
	// Region is the region the S3 request is signed for.
	Region string `json:"region,omitempty"`
	// Bucket names an EXISTING bucket to use. Mutually exclusive with CreateBucket: Burrow either
	// creates the bucket it will own or is pointed at one, and never infers a bucket by name
	// (ADR-0063 §4).
	Bucket string `json:"bucket,omitempty"`
	// CreateBucket asks Burrow to create its own bucket, with a readable prefix and a random
	// component, and record the name it created (ADR-0063 §4). It trips the bucket.create guardrail.
	CreateBucket bool `json:"create_bucket,omitempty"`
	// RetentionDays is how long backups written to this destination must stay restorable — the
	// window the bucket's lifecycle rules are reconciled against (ADR-0063 §3). Zero declares no
	// window, under which any age-expiring lifecycle rule conflicts, because nothing prunes
	// Burrow's backups today.
	RetentionDays int `json:"retention_days,omitempty"`
	// AccessKeyID is one half of the object-storage credential VALUE. Like Token it travels only in
	// this request body, is written to burrow-credentials after the destination is verified, and is
	// never logged, stored in Postgres, or echoed back.
	AccessKeyID string `json:"access_key_id,omitempty"`
	// SecretAccessKey is the other half of the credential VALUE, held to exactly the same rules.
	SecretAccessKey string `json:"secret_access_key,omitempty"`
	// Confirm acknowledges the bucket.create guardrail so a bucket creation proceeds past it.
	Confirm bool `json:"confirm,omitempty"`
}

// AddDomainRequest points a host at an address through a configured DNS provider (ADR-0018).
// The address is the cluster's external entry point — the ingress controller's IP or hostname.
// Supply it explicitly with Address, or name an exposed App and the control plane reads the
// controller-assigned address from that app's Ingress (the value `burrow app reachability` reports).
type AddDomainRequest struct {
	Host     string `json:"host"`
	Provider string `json:"provider"`
	// Address is the external IP or hostname to point Host at. Optional when App is set.
	Address string `json:"address,omitempty"`
	// App is an exposed application whose ingress external address Host should point at, used
	// when Address is empty so the agent need not look the address up itself.
	App string `json:"app,omitempty"`
	// Confirm acknowledges the dns.write guardrail so the operation proceeds past it.
	Confirm bool `json:"confirm,omitempty"`
}

// RemoveDomainRequest removes the DNS record a provider holds for a host (ADR-0018).
type RemoveDomainRequest struct {
	Host     string `json:"host"`
	Provider string `json:"provider"`
	// Confirm acknowledges the dns.delete guardrail so the operation proceeds past it.
	Confirm bool `json:"confirm,omitempty"`
}

// DomainResult reports the DNS record a domain operation created, updated, or removed.
type DomainResult struct {
	Host     string `json:"host"`
	Provider string `json:"provider"`
	Type     string `json:"type,omitempty"`
	Address  string `json:"address,omitempty"`
}

// BackupStatus is the lifecycle state of a recorded Postgres backup (ADR-0032). A backup is
// recorded pending when its Job is started and moves to completed on the Job's success or failed
// on its failure.
//
// Completed means THE BYTES REACHED THE BACKUP'S DESTINATION, and it means nothing weaker
// (ADR-0063 §7). Where the destination is an object store, the Job has written the object and read
// it back before this row is allowed to say completed: a row claiming success for bytes that never
// left the cluster is worse than no row at all, because it converts a missing backup into a false
// assurance, discovered at restore time by somebody who cannot restore a database.
type BackupStatus string

const (
	// BackupPending is a backup whose Job has been started but has not yet completed. It covers the
	// whole of the dump AND the retried write to the destination: a retry in flight is a backup
	// still in progress, not a failure and certainly not a success. Nothing reads pending as
	// successful — the age of the last SUCCESSFUL backup counts completed rows only — so a row left
	// pending by a burrowd that died mid-Job keeps the backup age growing, which is the safe
	// direction for it to fail in.
	BackupPending BackupStatus = "pending"
	// BackupCompleted is a backup that reached its destination: the dump is on the PVC and, when an
	// object-storage destination is configured, in the store and verified readable there.
	BackupCompleted BackupStatus = "completed"
	// BackupFailed is a backup that did not reach its destination. FailureReason says why in
	// machine-readable terms.
	BackupFailed BackupStatus = "failed"
)

// Valid reports whether s is a known BackupStatus.
func (s BackupStatus) Valid() bool {
	switch s {
	case BackupPending, BackupCompleted, BackupFailed:
		return true
	default:
		return false
	}
}

// BackupDestinationKind names where a backup's bytes ended up, so a listing can distinguish a dump
// that survives losing the cluster from one that does not (ADR-0063).
//
// The two are a TIER, not alternatives. The PVC is where pg_dump writes and where pg_restore reads:
// a same-cluster staging copy that keeps the single-app logical restore ADR-0066 §4 deliberately
// retains both cheap and available when the vendor is not. The object store is the durable
// destination that takes the backup out of the database's failure domain, which is the entire point
// of ADR-0063. Recording which one a row reached is what stops "we have backups" from meaning two
// different things on the same listing.
type BackupDestinationKind string

const (
	// BackupDestinationCluster is a backup that only ever reached the in-cluster PVC, because no
	// object-storage provider is registered. It is recorded rather than left blank: a backup sharing
	// a failure domain with the database it came from is a fact an operator should be able to read
	// off the row, not one they have to infer from an absence.
	BackupDestinationCluster BackupDestinationKind = "cluster"
	// BackupDestinationObjectStore is a backup that reached the registered object store. On a
	// completed row it means the object was written AND read back at ObjectKey.
	BackupDestinationObjectStore BackupDestinationKind = "object-store"
)

// The closed BackupFailureReason set: why a backup did not reach its destination, in terms a caller
// can BRANCH on rather than parse (ADR-0074 §5). It is deliberately a separate vocabulary from
// ADR-0074 §2's IssueReason set, which describes why a POD is blocked; these describe what happened
// once the pod was running. Both sets travel on the same row — a backup whose Job never started
// records the IssueReason the waiter returned (issue #352), and one whose Job ran records one of
// these — so a reader gets the closed reason either way, and IsBackupFailureReason is how a caller
// tells which vocabulary it is holding.
//
// NO SECRET VALUE ENTERS A REASON OR ITS DETAIL. The detail is Burrow-authored — an HTTP status, an
// attempt count, a byte count — and never the vendor's response body, which is the one place an
// access key id is known to be echoed back. The body goes to the Job's pod log, which is the
// operator's to read and is no wider an exposure than the credential the pod already mounts.
const (
	// BackupReasonDumpFailed is the backup command itself failing, with nothing offered to the store.
	// For a logical dump that is pg_dump: the database refused the connection, the PVC ran out of
	// disk, the command errored. For a physical one it is the `Backup` object reaching `failed` —
	// pgBackRest would not take the base backup, or CloudNativePG rejected the request. A `Backup`
	// OBJECT failing and a backup failing to LEAVE THE CLUSTER are different facts (ADR-0063 §7), and
	// this is the first of them; the store reasons below are the second.
	BackupReasonDumpFailed = "DumpFailed"
	// BackupReasonStoreUnreachable is the destination not answering — DNS, TLS, connection refused,
	// or a 5xx — after every retry. This is the one ADR-0063 §7 says to retry, because a transient
	// network failure is the common case; reaching this reason means the retries were used up. On the
	// physical path it is also what `walArchivingFailing` means: the instance is producing
	// write-ahead log the repository is not accepting, so the archive the base backup depends on is
	// not arriving.
	BackupReasonStoreUnreachable = "StoreUnreachable"
	// BackupReasonStoreRejected is the destination answering, and saying no: a credential that is
	// wrong or has been revoked, a bucket that is gone, a write the policy forbids. It is NOT
	// retried — a 403 does not become a 200 by being asked again — so it fails on the first answer
	// and says so, rather than spending the retry budget proving it.
	BackupReasonStoreRejected = "StoreRejected"
	// BackupReasonObjectNotReadable is the destination accepting the write and then failing to serve
	// the object back, or serving it back at the wrong length. It is the reason that exists because
	// a 200 on a PUT is not the same fact as a durable, readable object, and a Backup row is only
	// allowed to claim the second one.
	BackupReasonObjectNotReadable = "ObjectNotReadable"
	// BackupReasonNotRecorded is the backup that reached the store but whose completion could not be
	// written to the registry. The bytes are safe and the row is not, which is the harmless
	// direction: an under-reported backup age is a false alarm, and a false assurance is not.
	BackupReasonNotRecorded = "NotRecorded"
)

// BackupFailureReasons returns every member of the closed set, so a caller (or a generated agent
// schema) can enumerate the vocabulary instead of hard-coding it.
func BackupFailureReasons() []string {
	return []string{
		BackupReasonDumpFailed,
		BackupReasonStoreUnreachable,
		BackupReasonStoreRejected,
		BackupReasonObjectNotReadable,
		BackupReasonNotRecorded,
	}
}

// IsBackupFailureReason reports whether reason is a member of the closed backup set — which is also
// how a caller tells a backup reason from an ADR-0074 §2 IssueReason arriving on the same field.
func IsBackupFailureReason(reason string) bool {
	for _, r := range BackupFailureReasons() {
		if r == reason {
			return true
		}
	}
	return false
}

// Backup is one recorded per-app database backup (ADR-0032): the control-plane index row for a
// logical dump written to the backup PVC and, when one is registered, on to an object store
// (ADR-0063). It names the app, the on-PVC path, the destination, the byte size (0 when unknown),
// and the lifecycle status — never a credential or a connection string. burrowd is not mounted to
// the backup PVC, so this row, not the volume, is what `addon backups` lists.
type Backup struct {
	// ID is the backup identifier, minted from the IDs seam — also the dump filename stem.
	ID string `json:"id"`
	// Kind says which mechanism produced this row: a per-app logical dump, or a physical base backup
	// of the whole instance (ADR-0066 §4). Empty on a row written before the two coexisted, which is
	// logical by construction — nothing else existed to write it.
	Kind BackupKind `json:"kind,omitempty"`
	// App is the application whose database was dumped. EMPTY on a physical row: that backup covers
	// every database on the instance, and naming one of them would make a listing claim a per-app
	// backup exists where none does.
	App string `json:"app"`
	// Environment is the environment whose instance the dump was taken from ("prod" for the default
	// one). Each environment has its own Postgres instance (ADR-0067 §1), so a dump
	// is only meaningful against the one it came from: restore requires the two to agree rather than
	// letting one environment's contents be written over another's.
	Environment string `json:"environment,omitempty"`
	// CreatedAt is when the backup was recorded, read from the injected clock.
	CreatedAt time.Time `json:"created_at"`
	// Path is the location of the dump WITHIN its claim (e.g. /backups/<app>/<id>.dump). It is a
	// path on a volume burrowd does not mount — never a credential. It does not identify a dump on
	// its own: the same path exists on every environment's claim, and Volume says which one.
	Path string `json:"path,omitempty"`
	// Volume is the PersistentVolumeClaim the dump was written to — this environment's backup claim
	// (BackupVolumeName). It is recorded rather than derived on read because the derivation changed:
	// backups written before backups were per-environment are all on the one claim that existed then
	// (PostgresBackupVolume), and a row that only said which environment it came from could not tell
	// the two eras apart. Recording it is what lets a restore refuse a dump that is not on the claim
	// this environment mounts, instead of running a Job that finds no such file.
	//
	// Empty only on a row written by an older burrowd against a newer schema; readers treat that as
	// the pre-migration claim, which is what it can only be.
	Volume string `json:"volume,omitempty"`
	// SizeBytes is the dump's size in bytes, or 0 when unknown.
	SizeBytes int64 `json:"size_bytes,omitempty"`
	// Status is the lifecycle state of this backup.
	Status BackupStatus `json:"status"`
	// Destination is where this backup's bytes ended up. It is written when the row is created, from
	// what was registered AT THAT MOMENT, so a listing says what each backup actually did rather
	// than what the current configuration would do — registering a destination today does not make
	// last month's in-cluster dumps durable, and the rows must not imply that it did.
	Destination BackupDestinationKind `json:"destination,omitempty"`
	// Provider is the name of the object-storage provider this backup was written to, empty for an
	// in-cluster one. It is a registry name, never a credential or an endpoint secret.
	Provider string `json:"provider,omitempty"`
	// ObjectKey is the key the dump occupies in the provider's bucket, empty for an in-cluster
	// backup. It is the address a restore reads from and the thing an operator checks at the vendor;
	// the bucket and endpoint it hangs off live on the provider row, not repeated here.
	ObjectKey string `json:"object_key,omitempty"`
	// FailureReason is why a failed backup failed — a member of the closed BackupFailureReason set,
	// or of ADR-0074 §2's IssueReason set when the Job never started. Empty on a pending or
	// completed row.
	FailureReason string `json:"failure_reason,omitempty"`
	// FailureDetail is one Burrow-authored line elaborating the reason, safe to print: an HTTP
	// status, an attempt count, a length mismatch. Never a vendor response body and never a
	// credential.
	FailureDetail string `json:"failure_detail,omitempty"`
}

// Durable reports whether this row is a backup that SURVIVES LOSING THE CLUSTER: it completed, and
// it completed at an object-store destination.
//
// Both halves are required and neither is redundant. A row only says `completed` for an object-store
// destination once the object was written AND read back (ADR-0063 §7), so the pair is the strongest
// fact Burrow holds about a backup — while a `cluster` destination says `completed` on a Job exiting
// zero, and its bytes are on a volume in the same failure domain as the database they came from.
// Checking the destination as well as the status also covers the row written before a provider was
// deregistered, which is `completed` for a dump that never left the cluster.
//
// It is a method rather than a comparison written out at each call site because two decisions stand
// on it and they must not drift: ADR-0064 §5 refuses to destroy a data volume unless the final
// backup is durable, and the backup-age signal of ADR-0063 §7 / ADR-0066 §5 reports the age of the
// last durable success. If those two ever disagreed, `--delete-data` would be refusing on one
// definition of "safe" while the status surface reported another.
func (b Backup) Durable() bool {
	return b.Status == BackupCompleted && b.Destination == BackupDestinationObjectStore
}

// BackupResult reports the outcome of an on-demand backup (ADR-0032): the recorded backup row. It
// carries the backup id, the app, the path, the destination, the size, and the status — never a
// secret value.
type BackupResult struct {
	Backup Backup `json:"backup"`
}

// BackupPath is the on-PVC path of app's dump for id, recorded in the backup row and used by the
// kube Job builder so both sides agree on the layout without the engine importing the kube package
// (ADR-0032). It is a path on a volume burrowd does not mount — never a credential.
func BackupPath(app, id string) string {
	return backupMountPath + "/" + app + "/" + id + ".dump"
}

// backupMountPath is where the backup PVC is mounted inside the backup/restore Job container; it
// prefixes every recorded backup path so the engine and the kube Job builder agree on the layout.
const backupMountPath = "/backups"

// BackupObjectKey is the object-store key of app's dump for id in environment env, recorded on the
// backup row and handed to the Job that writes it (ADR-0063). It is derived here, next to
// BackupPath, so the engine and the Job builder cannot disagree about where a backup lives.
//
// The layout is <prefix>/<env>/<app>/<id>.dump: the environment leads because it is the coarsest
// thing an operator or a lifecycle rule ever scopes to, and the id is unique on its own, so the
// path is legible at the vendor rather than being a flat bucket of opaque names. The prefix keeps
// Burrow's objects together in a bucket the operator may have pointed at rather than let Burrow
// create (ADR-0063 §4).
func BackupObjectKey(app, env, id string) string {
	if env == "" {
		env = DefaultEnvironment
	}
	return backupObjectPrefix + "/" + env + "/" + app + "/" + id + ".dump"
}

// backupObjectPrefix is where every backup object Burrow writes lives in the bucket.
const backupObjectPrefix = "burrow/backups"

// BackupKind says which of the two backup mechanisms produced a row (ADR-0066 §4). They are not
// interchangeable and the row has to say which it is, because the two answer different questions and
// a reader under pressure reaches for whichever one the listing makes look applicable.
//
//	logical  — one app's database, as it was at the moment of the dump. `pg_dump -Fc` through a Job
//	           Burrow runs, restorable into one app without touching any other (ADR-0032).
//	physical — the whole instance, and every database on it, as a base backup CloudNativePG asked
//	           pgBackRest for. It is the only kind with a write-ahead-log window behind it, and it
//	           is the only kind that cannot be restored per app.
type BackupKind string

const (
	// BackupKindLogical is a per-app `pg_dump`. It is the default a row carries when nothing says
	// otherwise, which is what every backup recorded before physical backups existed is.
	BackupKindLogical BackupKind = "logical"
	// BackupKindPhysical is a CloudNativePG `Backup` object over the whole instance. Its App is
	// empty — an instance-wide backup belongs to no single app, and attributing it to one would make
	// a listing claim a coverage that app does not have.
	BackupKindPhysical BackupKind = "physical"
)

// Valid reports whether k is a known BackupKind.
func (k BackupKind) Valid() bool {
	switch k {
	case BackupKindLogical, BackupKindPhysical:
		return true
	default:
		return false
	}
}

// PgBackRestRepoPath is the pgBackRest repository path holding environment env's physical backups
// and archived write-ahead log (ADR-0066 §3). It is derived here, beside BackupObjectKey, so the
// engine and the `Stanza` the adapter writes cannot disagree about where an environment's repository
// is.
//
// THE ENVIRONMENT IS IN THE PATH, and that is the isolation. Each environment has its own instance
// (ADR-0067 §1) and therefore its own pgBackRest stanza; two stanzas sharing a repository path would
// have each one's `create-stanza` looking at the other's backups, and a restore could reach an
// instance it was never taken from. One path per environment makes that unreachable rather than
// merely unlikely.
//
// It is returned with the leading slash pgBackRest's `repo-path` expects. The object KEYS underneath
// it carry no leading slash, which is what PgBackRestManifestKey accounts for.
func PgBackRestRepoPath(env string) string {
	if env == "" {
		env = DefaultEnvironment
	}
	return "/" + pgBackRestObjectPrefix + "/" + env
}

// PgBackRestManifestKey is the object key of the backup manifest pgBackRest writes for the backup
// labelled label, in stanza, in the repository at repoPath.
//
// It exists so a completed physical backup can be READ BACK before its row is allowed to say
// completed (ADR-0063 §7). CloudNativePG reporting a `Backup` as completed is the operator's word
// that pgBackRest returned zero; that the object store will serve the result back, at the key the
// repository says it is at, is a separate fact, and it is the one a restore depends on. The manifest
// is the object pgBackRest itself reads first when it restores that backup, so its absence is not a
// cosmetic discrepancy.
//
// It takes the repository PATH rather than the environment, and that is load-bearing: the path is
// read off the instance's OWN `Stanza`, so the key is where that instance actually writes rather than
// where Burrow would have configured it to. An instance whose stanza says something else is a
// mismatch to refuse, not a backup to go looking for in the wrong place.
func PgBackRestManifestKey(repoPath, stanza, label string) string {
	return strings.TrimPrefix(repoPath, "/") + "/backup/" + stanza + "/" + label + "/backup.manifest"
}

// PgBackRestLabelFromManifestKey reads pgBackRest's backup LABEL back out of a manifest key
// PgBackRestManifestKey composed, and returns "" for anything that is not one.
//
// It exists because a physical restore has to NAME the base backup it recovers, and the name the
// repository knows it by is pgBackRest's label rather than Burrow's backup id. The label is already on
// the row — inside the object key, which is recorded before the row is allowed to say completed — so
// this reads it back rather than adding a column that would have to be backfilled for every physical
// backup taken before restore existed. The key is one Burrow composed from the label itself, so the
// derivation is exact rather than a guess at somebody else's layout.
//
// It is the INVERSE of PgBackRestManifestKey and lives beside it so the two cannot drift: a change to
// the layout that did not change this would leave a restore naming a backup the repository has never
// heard of, which fails at the one moment there is nothing to fall back on.
func PgBackRestLabelFromManifestKey(key string) string {
	const suffix = "/backup.manifest"
	if !strings.HasSuffix(key, suffix) {
		return ""
	}
	trimmed := strings.TrimSuffix(key, suffix)
	i := strings.LastIndex(trimmed, "/")
	if i < 0 {
		return ""
	}
	return trimmed[i+1:]
}

// pgBackRestObjectPrefix is where every pgBackRest repository Burrow configures lives in the bucket.
// It is a sibling of backupObjectPrefix rather than the same prefix: the logical dumps are Burrow's
// own object layout and the repository underneath this one is pgBackRest's, and letting a lifecycle
// rule or an operator scope to one without the other is worth the extra path segment.
const pgBackRestObjectPrefix = "burrow/pgbackrest"

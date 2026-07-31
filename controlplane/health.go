// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package controlplane

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
	"unicode"
)

// Readiness probes for user applications (ADR-0076 §1-§3, §5-§6).
//
// Burrow set no probe on a user application at all, which meant Kubernetes marked a pod ready the
// moment its container started: an app that boots, binds nothing, and returns 500 to every request
// was a successful deploy, and a new pod took traffic before it had connected to anything. This file
// is the whole decision about what Burrow probes, and every rule in it errs the same way.
//
// THREE RULES, AND THEY ARE THE POINT:
//
//  1. READINESS ONLY; NEVER A LIVENESS PROBE (§1). Readiness removes a pod from its Service — a
//     reversible state that recovers by itself. Liveness RESTARTS THE CONTAINER, so a probe that is
//     wrong (a tight timeout, a dependency hiccup, a slow start under load) kills a working process
//     repeatedly and presents as CrashLoopBackOff: it manufactures the exact failure it was
//     installed to detect, under the load that made it briefly slow. Nothing here authors one, and
//     there is a test that says so.
//
//  2. A READINESS PROBE NEVER CHECKS AN EXTERNAL DEPENDENCY (§2). This is the rule a future
//     contributor will break while trying to help, so it is enforced by the SHAPE of ReadinessCheck
//     rather than by a comment: the type carries a port and a path and has nowhere to put a host, a
//     command, or a URL, and ValidateHealthPath refuses anything that looks like one. If every app's
//     readiness tested the shared database, one blip would fail every replica of every app at the
//     same moment and Kubernetes would pull them all from their Services — a degraded dependency
//     turned into a total outage, and worse here than most because the database is SHARED, so the
//     correlation is total. "Can I reach my database" is a real and valuable check; it belongs at
//     deploy time (§4), not in a loop.
//
//  3. THE DEFAULT IS CONSERVATIVE, AND ABSENT WHERE IT WOULD BE A GUESS (§3). Port known (the app is
//     published, so an exposure recorded one) → a TCP check on that port, which proves the process
//     bound the socket. Port unknown → NO PROBE, exactly as before. Burrow does not scan for a
//     listening port, does not assume 8080, and does not guess /healthz. A probe Burrow invented is
//     worse than no probe: it fails a working deploy and the user cannot tell whether their app is
//     broken or the guess is.
//
// §6 is the posture behind all of it: a probe that wrongly reports healthy costs a bad release the
// user can roll back; one that wrongly reports unhealthy costs them the ability to deploy at all,
// during an incident, with no obvious cause — and they will turn health checking off entirely rather
// than debug it, which loses the feature permanently. Every timing constant below is chosen on that
// asymmetry.
//
// HOW A FAILING PROBE SURFACES. It reuses the vocabulary that already exists rather than adding to
// it. A pod that starts but never passes its readiness probe reports no blocking pod condition — it
// is running, it is simply not ready — so the rollout stalls and the Deployment's Progressing
// condition goes False with ProgressDeadlineExceeded, which is already a member of ADR-0074 §2's
// closed IssueReason set (ReasonProgressDeadlineExceeded). An agent branches on it today; a parallel
// reason coined here would be a second name for a state the surface can already describe.
//
// And because the rollout stalls rather than the app going down — the old ReplicaSet keeps serving
// while the new pod fails to become ready — a wrong probe costs a release, not an outage. That is
// §6's asymmetry showing up in the actual mechanics rather than only in the defaults.

// The readiness probe's timing, chosen to fail toward DEPLOYED (ADR-0076 §6) rather than toward a
// tidy signal. These are deliberately not operator-tunable: ADR-0076 asks for no such knob, and
// ADR-0068 §2's test for what belongs on the limits mechanism is whether a human has a real reason
// to pick a different number — a probe cadence is an implementation detail of the probe, not a
// bound anyone is trying to enforce.
const (
	// ReadinessInitialDelaySeconds is zero on purpose. A readiness probe that has not passed yet
	// simply keeps the pod out of its Service, which costs nothing, so delaying the first attempt
	// would only delay traffic to a pod that was ready sooner. Slow starts are absorbed by the
	// Deployment's progress deadline, not by a guessed delay.
	ReadinessInitialDelaySeconds int32 = 0
	// ReadinessPeriodSeconds is how often the check runs.
	ReadinessPeriodSeconds int32 = 10
	// ReadinessTimeoutSeconds is generous rather than tight: a loaded pod can be slow to accept a
	// connection or serve a handler, and a timeout that fires on load pulls the pod out of service
	// exactly when the remaining replicas can least absorb it.
	ReadinessTimeoutSeconds int32 = 5
	// ReadinessFailureThreshold is how many consecutive failures remove a SERVING pod from its
	// Service — six, so roughly a minute of continuous failure is required. A brief stall is not
	// a reason to stop serving.
	ReadinessFailureThreshold int32 = 6
	// ReadinessSuccessThreshold is one: a single success restores the pod. Kubernetes requires this
	// to be 1 for a readiness probe in any case.
	ReadinessSuccessThreshold int32 = 1
)

// maxHealthPathLen bounds a declared health path. A path is a path, not a payload.
const maxHealthPathLen = 200

// HealthEndpoint is the health endpoint a user or their agent DECLARED for an app in one
// environment (ADR-0076 §5) — the opt-in that turns Burrow's conservative TCP default into an HTTP
// check against a real endpoint. It is intent, recorded by `burrow app health set`, and the zero
// value means nothing was declared, which is the state every app starts in.
//
// It is keyed per (app, environment) like an exposure and an auto-deploy level are: the same app in
// two environments can serve on two different ports, and an operator may want the production probe
// to differ from the development one.
type HealthEndpoint struct {
	// App is the application the endpoint belongs to.
	App string `json:"app"`
	// Environment is the canonical environment name ("prod" for the default one).
	Environment string `json:"environment,omitempty"`
	// Path is the HTTP path the app serves its readiness answer on, e.g. "/healthz". Empty means no
	// endpoint was declared and §3's default applies.
	Path string `json:"path,omitempty"`
	// Port is the container port the path is served on. Zero means "the port the app is published
	// on", resolved at apply time from the recorded exposure — so an app that moves its exposure
	// does not need its health endpoint re-declared.
	Port int32 `json:"port,omitempty"`
	// UpdatedAt is when the endpoint was recorded, read from the injected clock.
	UpdatedAt time.Time `json:"updated_at,omitempty"`
}

// Declared reports whether an endpoint was actually declared.
func (h HealthEndpoint) Declared() bool { return h.Path != "" }

// ReadinessCheck is the readiness probe Burrow authors on a user application's container — the
// RESOLVED answer, after ResolveReadiness has combined what the user declared with what Burrow
// knows. The zero value means NO PROBE, which is the honest answer whenever Burrow does not know a
// port (ADR-0076 §3).
//
// Its shape is the enforcement of §2. There is no Host field, no Exec command, and no URL: the
// probe can only ever address the pod's OWN port, so "check the database from the readiness probe"
// is not a thing this type can express. That is deliberate — the rule with the largest blast radius
// is the one most likely to be broken by someone trying to be helpful, and a comment would not have
// stopped them.
type ReadinessCheck struct {
	// Port is the container port to check. Zero means no probe at all.
	Port int32 `json:"port,omitempty"`
	// Path, when set, makes this an HTTP GET against the pod's own port. Empty makes it a TCP
	// connect, which proves only that the process bound the socket — strictly more than "the
	// container did not exit", and all Burrow can prove without the app's cooperation.
	Path string `json:"path,omitempty"`
}

// Enabled reports whether a probe is authored at all.
func (r ReadinessCheck) Enabled() bool { return r.Port > 0 }

// HTTP reports whether this is an HTTP GET check rather than a TCP connect.
func (r ReadinessCheck) HTTP() bool { return r.Enabled() && r.Path != "" }

// Kind names the probe in one word for a human or an agent reading the health surface: "http",
// "tcp", or "none".
func (r ReadinessCheck) Kind() string {
	switch {
	case r.HTTP():
		return "http"
	case r.Enabled():
		return "tcp"
	default:
		return "none"
	}
}

// ResolveReadiness decides the probe for one app from its declared endpoint and the container port
// its recorded exposure routes to (zero when it is not published). It is the whole of ADR-0076 §3
// and §5, in one pure function, so the decision is testable without a cluster and cannot drift
// between the deploy path, the rollback path, and the config-reapply path that all call it.
//
//   - A declared path wins, on its own port when one was given and on the exposure's otherwise.
//   - No declared path, but a published port → a TCP connect on it.
//   - Neither → no probe. Burrow does not scan, assume 8080, or guess /healthz.
//
// A declared path with no port anywhere is the one degenerate case: the endpoint was declared before
// the app was published, so there is nothing to reach it on yet. That returns NO PROBE rather than a
// guessed port, and the health surface says so; the probe starts on the first apply after the app is
// published.
func ResolveReadiness(ep HealthEndpoint, exposedPort int32) ReadinessCheck {
	if ep.Declared() {
		port := ep.Port
		if port == 0 {
			port = exposedPort
		}
		if port <= 0 {
			return ReadinessCheck{}
		}
		return ReadinessCheck{Port: port, Path: ep.Path}
	}
	if exposedPort > 0 {
		return ReadinessCheck{Port: exposedPort}
	}
	return ReadinessCheck{}
}

// ValidateHealthPath checks a declared health path at the boundary, before it is stored.
//
// It is the second half of §2's enforcement. ReadinessCheck cannot HOLD a host, and this refuses to
// accept one: "http://db:5432/health" does not begin with "/", and "//db/health" is a
// protocol-relative URL, so both are rejected here rather than silently turned into a probe that
// leaves the pod. The remaining rules keep a path a path — one line, bounded, no whitespace or
// control characters that would land in a Kubernetes object.
func ValidateHealthPath(path string) error {
	if path == "" {
		return fmt.Errorf("health path is empty")
	}
	if !strings.HasPrefix(path, "/") {
		return fmt.Errorf("health path %q must begin with %q: a readiness probe checks THIS pod's own endpoint, so it takes a path and never a URL or a host (ADR-0076 §2)", path, "/")
	}
	if strings.HasPrefix(path, "//") {
		return fmt.Errorf("health path %q begins with %q, which is a protocol-relative URL and would leave the pod: a readiness probe checks THIS pod's own endpoint (ADR-0076 §2)", path, "//")
	}
	if strings.Contains(path, "://") {
		return fmt.Errorf("health path %q looks like a URL: a readiness probe checks THIS pod's own endpoint, so it takes a path like %q (ADR-0076 §2)", path, "/healthz")
	}
	for _, r := range path {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			return fmt.Errorf("health path %q contains whitespace or a control character", path)
		}
	}
	if len(path) > maxHealthPathLen {
		return fmt.Errorf("health path is %d bytes, over the %d-byte limit", len(path), maxHealthPathLen)
	}
	return nil
}

// ValidateHealthPort checks a declared health port. Zero is allowed and means "the port the app is
// published on", resolved at apply time.
func ValidateHealthPort(port int32) error {
	if port < 0 || port > 65535 {
		return fmt.Errorf("health port %d is not a TCP port (1-65535, or 0 to use the published port)", port)
	}
	return nil
}

// HealthReport is what the health surface answers on a read or a write: what the user declared, what
// Burrow actually authors on the container as a result, and where that came from.
//
// The resolved half is there because the declared half is not the question an agent is asking. "I
// set a path" and "my app is being probed" are different facts — an endpoint declared before the app
// was published resolves to no probe — and a surface that reported only the intent would let that
// gap sit unnoticed.
type HealthReport struct {
	App         string `json:"app"`
	Environment string `json:"environment,omitempty"`
	// Path and Port are the DECLARED endpoint, empty and zero when none was declared.
	Path string `json:"path,omitempty"`
	Port int32  `json:"port,omitempty"`
	// Probe is what Burrow authors: "http", "tcp", or "none".
	Probe string `json:"probe"`
	// ProbePort is the port the probe checks; zero when Probe is "none".
	ProbePort int32 `json:"probe_port,omitempty"`
	// ProbePath is the path an HTTP probe requests; empty for a TCP or absent probe.
	ProbePath string `json:"probe_path,omitempty"`
	// Source is where the probe came from: "endpoint" (declared), "exposure" (§3's TCP default off
	// the published port), or "none".
	Source string `json:"source"`
	// Liveness is always false and is reported anyway, because "does Burrow restart my container
	// when this fails?" is the first question a reader has and the answer is load-bearing (§1).
	Liveness bool `json:"liveness"`
	// Hint is the §5 guidance, present whenever no endpoint has been declared. It is what turns this
	// from a status read into the place an agent learns that adding one is worth doing and what a
	// good one checks.
	Hint string `json:"hint,omitempty"`
	// AppliesOn says when the reported probe reaches the running pods, when it is not already there.
	AppliesOn string `json:"applies_on,omitempty"`
}

const (
	// HealthSourceEndpoint is a probe that came from a declared endpoint.
	HealthSourceEndpoint = "endpoint"
	// HealthSourceExposure is §3's conservative default: a TCP check on the published port.
	HealthSourceExposure = "exposure"
	// HealthSourceNone is no probe at all — the app is not published and declared nothing.
	HealthSourceNone = "none"
)

// NoHealthEndpointHint is ADR-0076 §5's guidance, stated where an agent will meet it: on the health
// surface and on the result of a deploy that has no declared endpoint.
//
// The last sentence is the one that earns its place. An agent asked to add /healthz will reach for
// the internet's most common example, which checks the database — the single worst thing to put in a
// readiness probe on this product, because the database is shared and one blip would then take every
// app out of service at once (§2). Saying so explicitly does not stop it happening every time, but
// leaving it unsaid guarantees it.
const NoHealthEndpointHint = "this app declares no health endpoint, so a release that boots but cannot serve still reports as a successful deploy. Adding one is usually a few lines: serve a path (e.g. /healthz) that returns 200 when the app itself is ready to handle a request, then run `burrow app health set <app> --path /healthz` so Burrow probes it. It must check only THIS app's own readiness to serve — never its database, cache, or any other dependency: a shared dependency blipping would then fail every replica of every app at once and take them all out of service (ADR-0076 §2)."

// NewHealthReport builds the answer for one app from its declared endpoint and its published port.
// applied says whether the running workload already carries the resolved probe; when it does not,
// the report names what makes it land, because a user who just set a path and saw nothing change
// would otherwise assume the setting did not take.
func NewHealthReport(app, env string, ep HealthEndpoint, exposedPort int32, applied bool) HealthReport {
	probe := ResolveReadiness(ep, exposedPort)
	rep := HealthReport{
		App:         app,
		Environment: env,
		Path:        ep.Path,
		Port:        ep.Port,
		Probe:       probe.Kind(),
		ProbePort:   probe.Port,
		ProbePath:   probe.Path,
		Source:      HealthSourceNone,
		Liveness:    false,
	}
	switch {
	case probe.HTTP():
		rep.Source = HealthSourceEndpoint
	case probe.Enabled():
		rep.Source = HealthSourceExposure
	}
	if !ep.Declared() {
		rep.Hint = NoHealthEndpointHint
	} else if !probe.Enabled() {
		// Declared, but there is no port to reach it on: the endpoint was set before the app was
		// published. Say so rather than reporting "none" with no explanation.
		rep.Hint = "this app declares a health path but no port, and it is not published, so Burrow has no port to check it on and sets no probe. Publish the app, or re-run `burrow app health set` with --port."
	}
	if !applied {
		rep.AppliesOn = "the app's next release; it is not on the running pods yet"
	}
	return rep
}

// resolveHealth reads what the app declared and what Burrow knows about its published port, and
// returns the probe those two resolve to. It is the single read behind every path that authors a
// workload — deploy, rollback, config reapply, health set/unset, expose/unexpose — so the probe on a
// rolled-back app is the same probe a redeploy would have given it.
//
// An app that is not published is the ordinary case, not a failure: ErrNotFound from the exposure
// read becomes a zero port, which resolves to no probe (§3).
func (e *Engine) resolveHealth(ctx context.Context, app, env string) (ReadinessCheck, HealthEndpoint, int32, error) {
	ep, err := e.db.HealthEndpoint(ctx, app, env)
	if err != nil {
		return ReadinessCheck{}, HealthEndpoint{}, 0, fmt.Errorf("reading the declared health endpoint: %w", err)
	}
	var port int32
	ex, err := e.db.Exposure(ctx, app, env)
	switch {
	case err == nil:
		port = ex.Port
	case errors.Is(err, ErrNotFound):
		// Not published: Burrow does not know a port, so §3 authors nothing.
	default:
		return ReadinessCheck{}, HealthEndpoint{}, 0, fmt.Errorf("reading the recorded exposure: %w", err)
	}
	return ResolveReadiness(ep, port), ep, port, nil
}

// readinessFor is resolveHealth for the callers that only need the probe.
func (e *Engine) readinessFor(ctx context.Context, app, env string) (ReadinessCheck, error) {
	r, _, _, err := e.resolveHealth(ctx, app, env)
	return r, err
}

// rollForReadiness re-applies the running workload when, and only when, something that just changed
// moved the resolved readiness probe away from `before`. It is how publishing or unpublishing an app
// reaches the probe on its pods rather than waiting for the next release.
//
// It is BEST-EFFORT and never fails its caller. The expose or unexpose it follows has already
// changed the cluster and been reported as done; failing it afterwards would report a working
// exposure as broken. Losing the reapply costs the probe until the app's next release, which is a
// delay in a check, not an inconsistency in the routing.
//
// The equality test is what keeps this from restarting apps for nothing: re-publishing an app at the
// same port resolves to the same probe and rolls nothing.
func (e *Engine) rollForReadiness(ctx context.Context, k Kubernetes, op, app, env string, before ReadinessCheck) {
	after, err := e.readinessFor(ctx, app, env)
	if err != nil {
		slog.WarnContext(ctx, "resolving the readiness probe after "+op+" failed",
			"app", app, "env", env, "error", err)
		return
	}
	if after == before {
		return
	}
	if _, err := e.reapplyWorkload(ctx, k, op, app, env); err != nil {
		slog.WarnContext(ctx, "re-applying the workload to update its readiness probe failed",
			"app", app, "env", env, "op", op, "error", err)
	}
}

// AppHealth reports the health endpoint declared for an app and the probe Burrow authors as a result
// (ADR-0076 §3, §5). It is a read: nothing is changed and nothing is rolled.
func (e *Engine) AppHealth(ctx context.Context, app, env string) (HealthReport, error) {
	if err := (App{Name: app}).Validate(); err != nil {
		return HealthReport{}, fmt.Errorf("app health: %w: %w", ErrInvalid, err)
	}
	if _, err := e.resolveNamespace(ctx, env); err != nil {
		return HealthReport{}, fmt.Errorf("app health %s: %w", app, err)
	}
	_, ep, port, err := e.resolveHealth(ctx, app, envName(env))
	if err != nil {
		return HealthReport{}, fmt.Errorf("app health %s: %w", app, err)
	}
	running, err := e.hasRunningRelease(ctx, app, envName(env))
	if err != nil {
		return HealthReport{}, fmt.Errorf("app health %s: %w", app, err)
	}
	return NewHealthReport(app, envName(env), ep, port, running), nil
}

// SetAppHealth records the health endpoint an app serves and re-applies the running workload so the
// probe reaches its pods (ADR-0076 §5). port may be zero, meaning "the port the app is published
// on", resolved on every apply.
//
// There is no guardrail on it, for the reason `config set` has none: it changes what Burrow checks,
// not what the app can reach, and the change is reversible with `health unset`. Its blast radius is
// one app's pods being gated on one more condition — and §6's whole posture is that the condition
// fails toward deployed.
func (e *Engine) SetAppHealth(ctx context.Context, app, env, path string, port int32) (HealthReport, error) {
	if err := (App{Name: app}).Validate(); err != nil {
		return HealthReport{}, fmt.Errorf("set health: %w: %w", ErrInvalid, err)
	}
	if err := ValidateHealthPath(path); err != nil {
		return HealthReport{}, fmt.Errorf("set health %s: %w: %w", app, ErrInvalid, err)
	}
	if err := ValidateHealthPort(port); err != nil {
		return HealthReport{}, fmt.Errorf("set health %s: %w: %w", app, ErrInvalid, err)
	}
	ns, err := e.resolveMutatingNamespace(ctx, env)
	if err != nil {
		return HealthReport{}, fmt.Errorf("set health %s: %w", app, err)
	}
	ep := HealthEndpoint{App: app, Environment: envName(env), Path: path, Port: port, UpdatedAt: e.clock.Now()}
	if err := e.db.SetHealthEndpoint(ctx, ep); err != nil {
		return HealthReport{}, fmt.Errorf("set health %s: recording the endpoint: %w", app, err)
	}
	return e.applyHealth(ctx, ns, "set health", app, env, ep)
}

// UnsetAppHealth removes an app's declared endpoint, returning it to ADR-0076 §3's default — a TCP
// check on the published port, or no probe at all when the app is not published — and re-applies the
// running workload so the change reaches its pods. Unsetting an endpoint that was never declared is
// not an error: it is the state the app is already in.
func (e *Engine) UnsetAppHealth(ctx context.Context, app, env string) (HealthReport, error) {
	if err := (App{Name: app}).Validate(); err != nil {
		return HealthReport{}, fmt.Errorf("unset health: %w: %w", ErrInvalid, err)
	}
	ns, err := e.resolveMutatingNamespace(ctx, env)
	if err != nil {
		return HealthReport{}, fmt.Errorf("unset health %s: %w", app, err)
	}
	if err := e.db.UnsetHealthEndpoint(ctx, app, envName(env)); err != nil {
		return HealthReport{}, fmt.Errorf("unset health %s: %w", app, err)
	}
	return e.applyHealth(ctx, ns, "unset health", app, env, HealthEndpoint{})
}

// applyHealth rolls the running workload after a health-endpoint write and builds the report. Unlike
// rollForReadiness, a failure here IS returned: the user asked for this directly, so "the setting is
// stored but did not reach your pods" is the answer they need, not a log line.
func (e *Engine) applyHealth(ctx context.Context, ns, op, app, env string, ep HealthEndpoint) (HealthReport, error) {
	applied, err := e.reapplyWorkload(ctx, e.k8s.WithNamespace(ns), op, app, envName(env))
	if err != nil {
		return HealthReport{}, err
	}
	_, _, port, err := e.resolveHealth(ctx, app, envName(env))
	if err != nil {
		return HealthReport{}, fmt.Errorf("%s %s: %w", op, app, err)
	}
	return NewHealthReport(app, envName(env), ep, port, applied), nil
}

// hasRunningRelease reports whether app has a release that reached the cluster in env — the fact
// that separates "the probe is on your pods" from "the probe lands on your next deploy".
func (e *Engine) hasRunningRelease(ctx context.Context, app, env string) (bool, error) {
	releases, err := e.db.Releases(ctx, app, env)
	if err != nil {
		return false, fmt.Errorf("reading release history: %w", err)
	}
	_, ok := lastDeployed(releases)
	return ok, nil
}

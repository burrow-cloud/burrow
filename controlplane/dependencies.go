// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"
)

// Deploy-time dependency checks, derived from what Burrow provisioned (ADR-0076 §4).
//
// THIS IS THE OTHER SIDE OF THE LINE health.go DRAWS. A readiness probe structurally cannot name a
// host or a command — ReadinessCheck has nowhere to put one — because a readiness probe that tested
// the shared database would remove every replica of every app from service the moment that database
// blipped (§2). "Can I reach my database" is still a real and valuable question. It is answered HERE,
// ONCE, AT DEPLOY TIME, where a failure catches the misconfiguration that makes a deploy silently bad
// and cannot amplify a degraded dependency into an outage.
//
// DERIVED, NOT CONFIGURED. Burrow provisioned these things and recorded them, so it does not have to
// ask what an app depends on to check what it gave it:
//
//	an attached Postgres database    Burrow provisioned the database and role on this environment's
//	                                 instance and wrote the app's DATABASE_URL into its Secret, so
//	                                 the app database listing IS the record that the dependency
//	                                 exists — connect with the app's own credential and SELECT 1
//	a published exposure             Burrow recorded the container port the exposure routes to and
//	                                 created the Service in front of it — request it and report the
//	                                 status code
//
// This is the part no generic platform can offer: a PaaS that did not attach your database cannot
// test your database. There is nothing for a user to declare and nothing that can drift, because the
// list is recomputed from the registry on every deploy.
//
// REPORTED, NEVER FATAL (§4, and §6's posture behind it). A failed check does not roll back, does not
// fail the deploy retroactively, and does not stop a release being recorded deployed — ADR-0072 §6
// already decided Burrow does not roll back by itself. The deploy has landed by the time the check
// runs and the result rides back on it. A check that could fail a deploy would be a new way for an
// app to become undeployable during an incident, which §6 says users respond to by turning health
// checking off entirely.
//
// WHY THE RESULT IS NOT A FAILURE-LEDGER ROW. ADR-0074's ledger is the obvious-looking home and it is
// the wrong one, for a mechanical reason and a definitional one. Mechanically, the ledger resolves BY
// ABSENCE: ResolveFailures closes every active row the current sweep did not observe, and the
// observer does not — and must not — run a Job in every app's image on every sweep, so a row written
// here would be closed within one observation cadence and would read as "it recovered on its own",
// which is a false statement about a dependency nobody rechecked. Definitionally, ADR-0074 §7 splits
// the two records by who asked: the ledger records what happened afterwards, unrequested, and the
// audit log records what Burrow was asked to do and what it then executed. A dependency check is a
// step of a deploy someone requested, so it is audited (auditOpDependencyCheck) and returned on the
// deploy result — the same surface as any other deploy outcome, which is what §4 asks for.
//
// NO SECRET VALUE CROSSES ANY OF THIS. The check needs the app's credential by nature, and the
// credential never leaves the app's own container: Kubernetes injects the app's Secret into the check
// pod via envFrom, and the probe reads DATABASE_URL from its own environment. What comes back is a
// reason from the closed set below plus, at most, the host and port that were dialled — extracted
// from the URL structurally, never the userinfo, never the query. See probeFailure in
// cmd/burrowd/checkdeps.go, which is the one place a driver error is turned into a reason and where
// the driver's own message is deliberately discarded.

// DependencyKind names a class of dependency Burrow provisioned for an app and can therefore check.
// It is a closed set, and it is closed for the same reason ADR-0074 §2's IssueReason set is: the
// consumer that matters is an agent branching on the value, not a person reading prose.
type DependencyKind string

const (
	// DependencyPostgres is a database and login role Burrow provisioned for the app on its
	// environment's Postgres instance, reachable through the DATABASE_URL Burrow wrote into the
	// app's Secret (ADR-0031).
	DependencyPostgres DependencyKind = "postgres"
	// DependencyExposure is the app's published port, reachable through the Service the exposure
	// created in front of it (ADR-0018).
	DependencyExposure DependencyKind = "exposure"
)

// DependencyOutcome is what one check found. Three values rather than a boolean, because "the
// dependency answered", "the dependency did not answer" and "Burrow could not ask" are three
// different facts and collapsing the third into the second reports a healthy database as broken.
type DependencyOutcome string

const (
	// DependencyPassed means the dependency answered.
	DependencyPassed DependencyOutcome = "passed"
	// DependencyFailed means the dependency did not answer, or answered with a refusal.
	DependencyFailed DependencyOutcome = "failed"
	// DependencySkipped means the check did not run: Burrow could not stand the check up, or could
	// not read what it needed to derive it. It is NOT a failure of the dependency.
	DependencySkipped DependencyOutcome = "skipped"
)

// The closed reason vocabulary for a dependency check. It is deliberately SEPARATE from ADR-0074 §2's
// IssueReason set and §6's discrepancy reasons rather than an extension of either: every IssueReason
// is a blocking condition the cluster reported about a pod, and every discrepancy reason is a
// registry-versus-cluster verdict, whereas these are the answers a connection attempt made from
// inside the app's own container can produce. Merging them would put a value that no Kubernetes
// controller can ever emit into a field documented as the cluster's own reason.
//
// Every one of these is Burrow-authored. None is a driver message, and none can carry a value the
// user's environment supplied — which is what keeps a DSN out of an audit row and out of a deploy
// result.
const (
	// ReasonCredentialUnset is the credential Burrow wrote not being present in the container's
	// environment at all. It is the misconfiguration this check exists for: the app was attached, and
	// something — a Secret that was not remounted, an env var shadowed by config — means the running
	// container does not see it.
	ReasonCredentialUnset = "CredentialUnset"
	// ReasonCredentialUnparsable is a credential present but not a connection string the driver can
	// read. The value itself is never quoted.
	ReasonCredentialUnparsable = "CredentialUnparsable"
	// ReasonHostUnresolvable is the dependency's hostname not resolving from inside the app's own
	// container — most often an app pointed at a Service in another namespace.
	ReasonHostUnresolvable = "HostUnresolvable"
	// ReasonConnectionRefused is the host resolving and nothing accepting on the port.
	ReasonConnectionRefused = "ConnectionRefused"
	// ReasonAuthenticationFailed is the dependency rejecting the credential. It names no part of the
	// credential.
	ReasonAuthenticationFailed = "AuthenticationFailed"
	// ReasonTimedOut is the dependency not answering inside the check's own dial window.
	ReasonTimedOut = "TimedOut"
	// ReasonQueryFailed is a connection that succeeded and a trivial query that did not — the shape a
	// role without CONNECT on the database it was pointed at leaves.
	ReasonQueryFailed = "QueryFailed"
	// ReasonUnreachable is every other way the dependency did not answer. It is the catch-all so that
	// an unclassified failure is still a member of the closed set rather than prose.
	ReasonUnreachable = "Unreachable"
	// ReasonCheckNotRun is a SKIPPED reason: the check pod could not be run to completion — it could
	// not be scheduled, the image could not be pulled, or Burrow stopped waiting. It says nothing
	// about the dependency.
	ReasonCheckNotRun = "CheckNotRun"
	// ReasonNotDerivable is a SKIPPED reason: Burrow could not read what it needed to decide whether
	// the dependency exists. An unreachable Postgres instance leaves this rather than an empty list,
	// because "no dependency" and "could not ask" must not read the same.
	ReasonNotDerivable = "NotDerivable"
)

// DependencyReasons returns the closed reason vocabulary a dependency-check result may carry.
func DependencyReasons() []string {
	return []string{
		ReasonCredentialUnset,
		ReasonCredentialUnparsable,
		ReasonHostUnresolvable,
		ReasonConnectionRefused,
		ReasonAuthenticationFailed,
		ReasonTimedOut,
		ReasonQueryFailed,
		ReasonUnreachable,
		ReasonCheckNotRun,
		ReasonNotDerivable,
	}
}

// IsDependencyReason reports whether reason is a member of the closed vocabulary. The engine checks
// it before a result reaches a caller or an audit row, so a reason nobody decided on — including one
// a future probe build invented — cannot escape into the surface an agent branches on.
func IsDependencyReason(reason string) bool {
	for _, r := range DependencyReasons() {
		if r == reason {
			return true
		}
	}
	return false
}

// Dependency is one thing Burrow gave an app and can therefore check: what it is, what Burrow
// provisioned that makes it a fact rather than a guess, and the non-secret handle the check uses.
//
// It carries no credential and no value. EnvKey is a KEY NAME — the env var the app already reads —
// and the value behind it is resolved by Kubernetes inside the check pod and never travels here.
type Dependency struct {
	// Kind is the class of dependency.
	Kind DependencyKind `json:"kind"`
	// Provisioned is the one-line, non-secret statement of WHAT Burrow gave the app, so a reader can
	// see that the check was derived rather than configured.
	Provisioned string `json:"provisioned"`
	// EnvKey is the environment variable the app reads the dependency's credential from, for a
	// dependency reached with one. Never a value.
	EnvKey string `json:"env_key,omitempty"`
	// Endpoint is the in-cluster address the check dials, for a dependency reached without a
	// credential. Never a value.
	Endpoint string `json:"endpoint,omitempty"`
}

// DependencyResult is what one check found. It is the unit reported on a deploy and by
// `burrow app checks`.
type DependencyResult struct {
	// Kind is the dependency this result is about.
	Kind DependencyKind `json:"kind"`
	// Outcome is passed, failed, or skipped.
	Outcome DependencyOutcome `json:"outcome"`
	// Reason is a member of DependencyReasons, empty on a passed check.
	Reason string `json:"reason,omitempty"`
	// Detail is one bounded, Burrow-authored line: what was tried and what came back. It never
	// carries a credential, a driver message, or any part of a connection string beyond the host and
	// port that were dialled.
	Detail string `json:"detail,omitempty"`
	// Status is the HTTP status code an exposure check received. Zero when none was.
	Status int `json:"status,omitempty"`
}

// Failed reports whether this result is a dependency that did not answer — as distinct from a check
// that did not run.
func (r DependencyResult) Failed() bool { return r.Outcome == DependencyFailed }

// dependencyDetailBytes bounds a result's detail, for the reason LedgerDetailBytes bounds a ledger
// row's: a detail is one line of context beside a reason, not a report, and an unbounded one would
// make a deploy result the place output accumulates.
const dependencyDetailBytes = 300

// dependencyDetail is the single gate every detail passes through before it reaches a caller or an
// audit row: first line only, collapsed, bounded. It is the same shape as LedgerDetail and exists for
// the same reason — the probe runs in the user's own image, and anything it printed past the first
// line is the image's output rather than Burrow's.
func dependencyDetail(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return boundText(strings.TrimSpace(strings.Join(strings.Fields(line), " ")), dependencyDetailBytes)
}

// ChecksReport is what `burrow app checks` answers and what the health surface carries: whether the
// default check runs for this app, and what Burrow would check if it did.
//
// Both halves are needed. ADR-0072 described `post-deploy` as user-configured, and this adds a
// Burrow-SUPPLIED default on that path, so a hook the user never configured can still run — which
// ADR-0076's consequences say must be visible and disableable rather than silent. Listing the derived
// dependencies is the "visible" half: an operator can see exactly what runs in their app's image
// after a deploy, without reading this file.
type ChecksReport struct {
	App         string `json:"app"`
	Environment string `json:"environment,omitempty"`
	// Enabled reports whether the default deploy-time check runs. True unless it was turned off for
	// this app and environment.
	Enabled bool `json:"enabled"`
	// Dependencies are what Burrow derived from what it provisioned. Empty means there is nothing to
	// check, and then no check pod runs at all — an app with no database and no exposure costs
	// nothing.
	Dependencies []Dependency `json:"dependencies"`
	// Note explains an empty or partial list in one line, so "nothing to check" and "could not tell"
	// do not read the same.
	Note string `json:"note,omitempty"`
}

// DependencyChecksDisabledNote is what the surface says when the default is off for an app.
const DependencyChecksDisabledNote = "the deploy-time dependency check is turned off for this app; a deploy runs no check and reports none. `burrow app checks enable <app>` turns it back on."

// NoDependenciesNote is what the surface says when there is genuinely nothing to check.
const NoDependenciesNote = "Burrow has provisioned nothing for this app that it can check: no database is attached and the app is not published. Attaching a database or publishing the app gives the deploy-time check something to verify."

// The probe's contract with the check pod. These are the two halves of ADR-0076's consequence that
// "Burrow needs a way to run a check inside the app's container, and the app's image may contain no
// shell, no psql, no curl" — which is exactly the minimal image users are told to build.
//
// THE DECISION: an init container running BURROW'S OWN IMAGE copies the burrowd binary into an
// emptyDir, and the check container runs THE APP'S IMAGE with that emptyDir mounted, executing the
// copied binary. The check therefore runs with the app's filesystem, the app's service account, the
// app's namespace and network policy, the app's config, and — through envFrom — the app's Secret,
// while depending on nothing being present in the image at all.
//
// The alternatives were both worse. REQUIRING THE IMAGE TO CARRY TOOLS fails on precisely the images
// this is most needed for: a scratch or distroless image has no shell to run and no client to run,
// so the check would be absent wherever an app is best packaged. RUNNING THE CHECK FROM BURROWD'S OWN
// POD proves the CONTROL PLANE can reach the database, not that the APP can — and the difference is
// exactly where misconfiguration lives (§4), since the app's DNS search path, its network policy and
// its credential are the things that go wrong.
//
// The binary copied is burrowd itself rather than a purpose-built one, following the shape ADR-0063
// §7's backup shipper already established: the same binary under a different subcommand, so there is
// no second image to publish, no second artifact to keep in step with a release, and no second
// implementation of the connection logic to drift from the one the control plane uses. The init
// container copies its own executable, so it needs no `cp` and no shell either.
const (
	// ProbeMountPath is where the emptyDir carrying the probe is mounted in both containers. It is
	// deliberately an unlikely path: it lands in the USER's image, and a mount that shadowed a real
	// directory would break the check pod in a way that looks like the app's fault.
	ProbeMountPath = "/burrow-probe"
	// ProbeBinaryName is the filename the init container writes its own executable to.
	ProbeBinaryName = "burrowd"
	// ProbePath is the full path the check container executes.
	ProbePath = ProbeMountPath + "/" + ProbeBinaryName
	// ProbeVolumeName is the emptyDir's name in the pod spec.
	ProbeVolumeName = "burrow-probe"
	// ProbeInstallCommand is the init container's subcommand: burrowd copying its own executable into
	// the directory named by its argument.
	ProbeInstallCommand = "install-probe"
	// ProbeCheckCommand is the check container's subcommand: burrowd running the plan and printing
	// the results.
	ProbeCheckCommand = "check-dependencies"
	// ProbePlanEnv is the environment variable the check container reads its plan from. The plan is
	// non-secret by construction — it carries key NAMES and endpoints — which is what makes it safe
	// to put in a Job spec anything that can read Jobs in the namespace can see.
	ProbePlanEnv = "BURROW_CHECK_PLAN"
	// ProbeResultPrefix marks the single line of the probe's output that carries its results. The
	// probe runs in the user's image, so the engine reads the marked line rather than trusting the
	// whole stream: an entrypoint wrapper or a shell profile that printed a banner would otherwise
	// make a working check unparseable.
	ProbeResultPrefix = "burrow-dependency-check: "
)

// ProbePlan is what the check container is told to do — the whole of its input, and non-secret by
// construction. It travels as JSON in one environment variable.
type ProbePlan struct {
	// Checks are the checks to run, in the order they are reported.
	Checks []ProbeCheck `json:"checks"`
}

// ProbeCheck is one check in a plan.
type ProbeCheck struct {
	// Kind selects what the probe does.
	Kind DependencyKind `json:"kind"`
	// EnvKey is the environment variable holding the connection string, for a postgres check. The
	// probe reads it from ITS OWN environment inside the app's container: the value is never in the
	// plan, never in the Job spec, and never crosses the control plane.
	EnvKey string `json:"env_key,omitempty"`
	// URL is the address an exposure check requests. It is an in-cluster Service address Burrow
	// composed, not a user value.
	URL string `json:"url,omitempty"`
}

// ProbeReport is what the probe prints on its marked line and what the engine parses back.
type ProbeReport struct {
	Results []DependencyResult `json:"results"`
}

// MarshalProbePlan renders a plan for the check container's environment.
func MarshalProbePlan(p ProbePlan) (string, error) {
	b, err := json.Marshal(p)
	if err != nil {
		return "", fmt.Errorf("rendering the dependency-check plan: %w", err)
	}
	return string(b), nil
}

// ParseProbePlan reads a plan back inside the check container.
func ParseProbePlan(s string) (ProbePlan, error) {
	var p ProbePlan
	if err := json.Unmarshal([]byte(s), &p); err != nil {
		return ProbePlan{}, fmt.Errorf("reading the dependency-check plan: %w", err)
	}
	return p, nil
}

// The two ways reading the check pod's output can fail. They are sentinels rather than prose because
// the engine tells a reader WHICH of them happened, and the two have different fixes: output that
// carried no marked line at all is the shape an image whose entrypoint wraps the command and
// discards its stdout leaves, while a marked line that will not parse means the line was reached and
// mangled.
var (
	// ErrNoProbeResult is a captured stream with no marked result line anywhere in it.
	ErrNoProbeResult = errors.New("the dependency check produced no result line")
	// ErrProbeResultUnreadable is a marked result line that is not the report Burrow prints.
	ErrProbeResultUnreadable = errors.New("the dependency check's result line could not be read")
)

// ParseProbeReport finds the probe's marked line in a captured output stream and reads the results
// from it. It takes the LAST marked line: the probe prints exactly one, and taking the last means a
// user image that somehow echoed an earlier one cannot displace the real answer.
//
// Every result is passed through the same gates the engine applies to its own: an unknown outcome or
// a reason outside the closed vocabulary is normalised rather than trusted, so a probe build that
// invented a value cannot put it on the surface an agent branches on.
func ParseProbeReport(out string) (ProbeReport, error) {
	marked := ""
	for _, line := range strings.Split(out, "\n") {
		if s, ok := strings.CutPrefix(strings.TrimSpace(line), strings.TrimSpace(ProbeResultPrefix)); ok {
			marked = strings.TrimSpace(s)
		}
	}
	if marked == "" {
		return ProbeReport{}, ErrNoProbeResult
	}
	var rep ProbeReport
	if err := json.Unmarshal([]byte(marked), &rep); err != nil {
		return ProbeReport{}, fmt.Errorf("%w: %w", ErrProbeResultUnreadable, err)
	}
	for i, r := range rep.Results {
		rep.Results[i] = normalizeDependencyResult(r)
	}
	return rep, nil
}

// normalizeDependencyResult forces one result into the closed vocabulary. It is applied to everything
// that comes back from the check pod, because that pod ran in the USER's image: the binary is
// Burrow's, but the surface must hold even if it were not.
func normalizeDependencyResult(r DependencyResult) DependencyResult {
	switch r.Outcome {
	case DependencyPassed, DependencyFailed, DependencySkipped:
	default:
		r.Outcome = DependencySkipped
		r.Reason = ReasonCheckNotRun
	}
	if r.Outcome == DependencyPassed {
		r.Reason = ""
	} else if !IsDependencyReason(r.Reason) {
		r.Reason = ReasonUnreachable
	}
	r.Detail = dependencyDetail(r.Detail)
	if r.Status < 0 || r.Status > 599 {
		r.Status = 0
	}
	return r
}

// deriveDependencies computes what Burrow would check for one app, from the registry alone. It is
// the whole of §4's "derived, not configured", and it is a read: nothing here runs a check.
//
// ns is the app's namespace, needed only to compose the in-cluster Service address the exposure check
// requests. It is Burrow's own composition, never a user value.
//
// A derivation that cannot be COMPLETED is reported rather than silently short: the second return is
// the note explaining a partial answer, and an unreachable Postgres instance produces one instead of
// an empty list. "No dependency" and "could not ask" must not read the same, which is the same
// distinction attachedApps' second return exists for.
func (e *Engine) deriveDependencies(ctx context.Context, app, env, ns string) ([]Dependency, string, error) {
	var (
		deps  []Dependency
		notes []string
	)

	// Postgres. The app database listing on this environment's instance IS the record that Burrow
	// provisioned a database for this app (ADR-0031): attach creates the database and role there and
	// writes DATABASE_URL into the app's Secret, so an app in that listing has, by construction, a
	// credential in its environment pointing at a database Burrow made.
	attached, known, err := e.hasProvisionedDatabase(ctx, app, env)
	switch {
	case err != nil:
		return nil, "", err
	case attached:
		// WHETHER a database is attached is derived; WHAT THE VARIABLE IS CALLED is recorded, because
		// no derivation can produce a name somebody chose (issue #462). A missing record answers
		// AppDatabaseURLKey, which is what every attachment made before the name was a choice used, so
		// this reads identically for them.
		key, err := e.db.AddonEnvKey(ctx, string(AddonPostgres), app, envName(env))
		if err != nil {
			return nil, "", fmt.Errorf("reading the attachment's variable name: %w", err)
		}
		deps = append(deps, Dependency{
			Kind:        DependencyPostgres,
			Provisioned: fmt.Sprintf("a database and login role on environment %s's Postgres instance, with the connection string written into this app's Secret", env),
			EnvKey:      key,
		})
	case !known:
		notes = append(notes, "Burrow could not ask this environment's Postgres instance whether a database is attached to this app, so a database dependency may exist and is not listed")
	}

	// The published exposure. Burrow recorded the container port and created the Service in front of
	// it, so the address below is Burrow's own composition end to end.
	ex, err := e.db.Exposure(ctx, app, envName(env))
	switch {
	case err == nil && ex.Port > 0:
		deps = append(deps, Dependency{
			Kind:        DependencyExposure,
			Provisioned: fmt.Sprintf("a Service in front of container port %d, published at %s", ex.Port, ex.Host),
			Endpoint:    appServiceURL(app, ns),
		})
	case err == nil, errors.Is(err, ErrNotFound):
		// Not published: nothing was provisioned, so there is nothing to check. Not a note — this is
		// the ordinary state of a deployed-but-unpublished app.
	default:
		return nil, "", fmt.Errorf("reading the recorded exposure: %w", err)
	}

	// ADR-0076 §4 names a third dependency — a mounted volume, checked by creating, reading back and
	// deleting a file under it — and it is deliberately absent. Burrow mounts no volume on a USER's
	// workload today: WorkloadSpec has no volume field, and every PersistentVolumeClaim in the tree
	// belongs to an add-on or to the backup path. Deriving a volume dependency would therefore mean
	// inventing one, which is the thing this whole file exists not to do. It lands here, as a third
	// case, when apps can mount volumes. See TestDependencyVolumeCheckAwaitsAppVolumes.

	return deps, strings.Join(notes, "; "), nil
}

// hasProvisionedDatabase reports whether Burrow provisioned app a database on env's Postgres
// instance, and whether it could tell. It reuses attachedApps rather than re-deriving the same fact,
// so the set a removal warns about and the set a deploy checks are the same set read the same way.
func (e *Engine) hasProvisionedDatabase(ctx context.Context, app, env string) (attached, known bool, err error) {
	instance, err := AddonInstanceName(AddonPostgres, envName(env))
	if err != nil {
		return false, false, err
	}
	info, err := e.db.Addon(ctx, instance)
	switch {
	case errors.Is(err, ErrNotFound):
		// No Postgres add-on in this environment: nothing was provisioned, and that is known.
		return false, true, nil
	case err != nil:
		return false, false, fmt.Errorf("reading the %s add-on: %w", instance, err)
	}
	apps, known := e.attachedApps(ctx, info)
	if !known {
		return false, false, nil
	}
	for _, a := range apps {
		if a == app {
			return true, true, nil
		}
	}
	return false, true, nil
}

// AppDatabaseURLKey is the DEFAULT environment variable Burrow writes an attached app's connection
// string into (ADR-0031). It is the KEY NAME and nothing else; the value lives only in the app's
// Kubernetes Secret.
//
// It is a default rather than the answer: an attach may name its own variable, and the name it chose
// is recorded with the attachment (issue #462). This constant is what an attachment that named
// nothing uses — which is every attachment made before naming existed — so it is the value the store
// returns for a missing record, and it is the only place the string appears. Nothing that acts on an
// attachment reads it directly: attach, detach, the dependency derivation and the restore cutover all
// go through Database.AddonEnvKey, so there is one name per attachment rather than four opinions
// about it.
const AppDatabaseURLKey = "DATABASE_URL"

// appServiceURL is the in-cluster address of an app's own Service. The Service listens on port 80 and
// forwards to the container port the exposure recorded, so requesting this requests the app's port
// (ADR-0076 §4) through the same route an external request takes once it is past the Ingress.
func appServiceURL(app, ns string) string {
	if ns == "" {
		return "http://" + app
	}
	return fmt.Sprintf("http://%s.%s.svc", app, ns)
}

// AppChecks reports what Burrow checks after a deploy of app, and whether it checks at all
// (ADR-0076 §4). It is a read: nothing is changed and no check is run.
func (e *Engine) AppChecks(ctx context.Context, app, env string) (ChecksReport, error) {
	if err := (App{Name: app}).Validate(); err != nil {
		return ChecksReport{}, fmt.Errorf("app checks: %w: %w", ErrInvalid, err)
	}
	ns, err := e.resolveNamespace(ctx, env)
	if err != nil {
		return ChecksReport{}, fmt.Errorf("app checks %s: %w", app, err)
	}
	return e.checksReport(ctx, app, envName(env), ns)
}

// SetAppChecks turns the deploy-time dependency check on or off for one app and environment
// (ADR-0076's consequence that a Burrow-supplied default on the post-deploy path must be visible and
// DISABLEABLE rather than silent).
//
// There is no guardrail on it, for the reason `health set` has none: it changes what Burrow observes,
// not what the app can reach, and it is reversible with the opposite call. Disabling it cannot break
// anything — the check never blocked a deploy — so the failure direction of the setting matches §6's
// posture as well.
func (e *Engine) SetAppChecks(ctx context.Context, app, env string, enabled bool) (ChecksReport, error) {
	if err := (App{Name: app}).Validate(); err != nil {
		return ChecksReport{}, fmt.Errorf("set app checks: %w: %w", ErrInvalid, err)
	}
	ns, err := e.resolveMutatingNamespace(ctx, env)
	if err != nil {
		return ChecksReport{}, fmt.Errorf("set app checks %s: %w", app, err)
	}
	if err := e.db.SetDependencyChecks(ctx, app, envName(env), enabled, e.clock.Now()); err != nil {
		return ChecksReport{}, fmt.Errorf("set app checks %s: %w", app, err)
	}
	return e.checksReport(ctx, app, envName(env), ns)
}

// checksReport builds the answer for one app: whether the check runs, and what it would check.
func (e *Engine) checksReport(ctx context.Context, app, env, ns string) (ChecksReport, error) {
	enabled, err := e.db.DependencyChecksEnabled(ctx, app, env)
	if err != nil {
		return ChecksReport{}, fmt.Errorf("app checks %s: %w", app, err)
	}
	deps, note, err := e.deriveDependencies(ctx, app, env, ns)
	if err != nil {
		return ChecksReport{}, fmt.Errorf("app checks %s: %w", app, err)
	}
	rep := ChecksReport{App: app, Environment: env, Enabled: enabled, Dependencies: deps, Note: note}
	if rep.Dependencies == nil {
		rep.Dependencies = []Dependency{}
	}
	switch {
	case !enabled:
		rep.Note = joinNotes(DependencyChecksDisabledNote, rep.Note)
	case len(rep.Dependencies) == 0 && rep.Note == "":
		rep.Note = NoDependenciesNote
	}
	return rep, nil
}

// joinNotes joins two optional notes into one line.
func joinNotes(a, b string) string {
	switch {
	case a == "":
		return b
	case b == "":
		return a
	}
	return a + " " + b
}

// dependencyCheckDeadline bounds the whole deploy-time check: standing up the pod, pulling whatever
// is not on the node, running every check, and reading the result back.
//
// It is a CONSTANT and not an operational limit, deliberately. ADR-0068 §2's test for the limits
// mechanism is whether a human has a real reason to pick a different number, and ADR-0076 §4 asks for
// no such knob: this is not a bound anyone is enforcing, it is how long a report-only step is allowed
// to delay a deploy that has ALREADY LANDED and been recorded. Two minutes is chosen against that —
// the app's image was just pulled for the rollout so it is usually warm, and a check that has not
// finished by then is worth less than the deploy result it is holding up. Expiry is a skipped result,
// never a failed one.
const dependencyCheckDeadline = 2 * time.Minute

// dependencyJobTTLSeconds is how long a finished check Job lingers before Kubernetes reaps it — the
// same one-hour window a hook Job and `burrow app run` take (ADR-0048 §7), so a check whose result
// needs looking at in the cluster is still there to look at.
const dependencyJobTTLSeconds int32 = 3600

// runDependencyChecks is the deploy-time check (ADR-0076 §4). It derives what Burrow provisioned,
// runs a single check pod in the app's own image with the app's environment, and returns what it
// found.
//
// IT NEVER RETURNS AN ERROR, and that is the point rather than a shortcut. The deploy has already
// landed and been recorded deployed by the time this is called; the release is superseded, the pods
// are rolling, and the traffic switch has happened. Turning a check failure — or a store read failure,
// or an unschedulable check pod — into a deploy error at that point would report a working deploy as
// broken, which is exactly the asymmetry §6 forbids. Everything that goes wrong here becomes a
// result, a log line, or nothing.
//
// It runs nothing at all when the app has no derived dependency, which is the common case for an
// unpublished app with no database: no Job, no pull, no added latency.
//
// progress reports the check as a deploy stage (issue #480). It is emitted below the disabled and
// nothing-derived returns and after the settle, so a deploy that runs no check pod reports no check
// stage and the stage's clock covers the check itself rather than the rollout wait in front of it.
func (e *Engine) runDependencyChecks(ctx context.Context, k Kubernetes, app, env, ns, image string, cfg map[string]string, settle rolloutSettle, progress deployProgress) []DependencyResult {
	enabled, err := e.db.DependencyChecksEnabled(ctx, app, env)
	if err != nil {
		slog.WarnContext(ctx, "reading whether the deploy-time dependency check is enabled failed",
			"app", app, "env", env, "error", err)
		return nil
	}
	if !enabled {
		return nil
	}
	deps, _, err := e.deriveDependencies(ctx, app, env, ns)
	if err != nil {
		slog.WarnContext(ctx, "deriving an app's dependencies for the deploy-time check failed",
			"app", app, "env", env, "error", err)
		return nil
	}
	if len(deps) == 0 {
		return nil
	}

	// Wait for the rollout to SETTLE before checking anything. This is what ADR-0072 §4's
	// `post-deploy` phase means by when it fires, and it is what makes the exposure check honest: the
	// Service routes to ready pods, so a check run mid-rollout would reach the PREVIOUS release's
	// replicas and report on the version this deploy just replaced.
	//
	// The outcome is deliberately NOT a gate. A rollout that did not settle is the case where the
	// check has the most to say — an app that cannot reach the database it was given is a common
	// reason a rollout never becomes ready — so the checks run either way and the reason for the
	// stall is reported by the surfaces that own it (the post-deploy hook, `burrow app status`).
	//
	// The wait is the deploy's ONE observation, shared with the `post-deploy` hook that follows
	// (issue #407). Forcing it here is what puts the settle before the check; the hook then reads the
	// same answer instead of waiting out the bound a second time. It is asked for after the
	// nothing-to-check returns above, so an app Burrow provisioned nothing for still waits for
	// nothing.
	rollout := settle()

	plan := ProbePlan{}
	for _, d := range deps {
		plan.Checks = append(plan.Checks, ProbeCheck{Kind: d.Kind, EnvKey: d.EnvKey, URL: d.Endpoint})
	}
	encoded, err := MarshalProbePlan(plan)
	if err != nil {
		slog.WarnContext(ctx, "rendering the dependency-check plan failed", "app", app, "env", env, "error", err)
		return nil
	}

	// The check gets its own deadline rather than the run Job's ten-minute one: this is a report-only
	// step on a deploy that has already succeeded, and a caller waiting ten minutes for it would be a
	// worse outcome than not checking.
	runCtx, cancel := context.WithTimeout(ctx, dependencyCheckDeadline)
	defer cancel()

	// The stage reports WHETHER THE CHECK RAN, not what it found. A dependency that failed its check
	// is a result on a successful deploy (§6) and travels back as one; a check pod that could not be
	// scheduled is the case where the stage itself did not work, and that is what the mark means.
	progress.started(StageDependencyCheck)
	res, runErr := k.RunJob(runCtx, RunSpec{
		App:        app,
		ID:         "check-" + e.ids.NewID(),
		Image:      image,
		Command:    []string{ProbePath, ProbeCheckCommand},
		Env:        cfg,
		TTLSeconds: dependencyJobTTLSeconds,
		// Probe asks the adapter for the init container that puts a working binary in an image that
		// may contain nothing at all. Without it the check container would try to execute a path the
		// app's image does not have.
		Probe: &ProbeSpec{Env: map[string]string{ProbePlanEnv: encoded}},
	})
	progress.finish(StageDependencyCheck, runErr == nil)

	var results []DependencyResult
	if runErr == nil {
		rep, parseErr := ParseProbeReport(res.Stdout + res.Stderr)
		if parseErr == nil {
			results = rep.Results
		} else {
			runErr = parseErr
		}
	}
	if runErr != nil {
		// The check did not run to a readable answer. Every dependency is SKIPPED, not failed: a check
		// pod that could not be scheduled says nothing whatever about the database.
		//
		// The DETAIL is where the diagnosis lives; the reason stays ReasonCheckNotRun so an agent
		// branching on it still sees that nothing was learned about the dependency.
		detail := checkNotRunDetail(res, runErr, rollout)
		slog.InfoContext(ctx, "the deploy-time dependency check did not produce a result",
			"app", app, "env", env, "error", runErr)
		for _, d := range deps {
			results = append(results, DependencyResult{
				Kind:    d.Kind,
				Outcome: DependencySkipped,
				Reason:  ReasonCheckNotRun,
				Detail:  detail,
			})
		}
	}

	e.recordDependencyCheck(ctx, app, env, image, results)
	return results
}

// rolloutNotReadyClause is what a check that produced no result adds when the rollout it waited for
// did not settle. It is the common state right after `burrow addon attach postgres <app>`, which
// rolls the workload: the check ran against pods that had not become ready, which explains most of
// the ways a check ends without an answer, and it is a thing the reader can act on.
const rolloutNotReadyClause = "the app's new pods had not become ready when the check ran"

// detailTruncationBytes is the room boundText's own truncation marker needs, so a composed detail can
// leave space for it rather than discovering the overflow after the fact.
const detailTruncationBytes = len("… (truncated)")

// checkNotRunDetail says what Burrow attempted and what stopped it, for a check that produced no
// result at all (issue #474). It replaces one string for every cause: "the check did not run to
// completion" is the status name in a sentence, and it left a reader with no next move.
//
// It classifies from the run's own error rather than guessing. A blocked Job carries the pod's own
// message on ADR-0074 §2's closed vocabulary, so it is passed straight through and the operator
// reads the same prose the status surface would show for the same pod (issue #478). Everything
// else names the thing that was attempted — a check pod running Burrow's probe inside the app's own
// image — and then which way it ended, because "a pod ran and printed nothing readable" and "the
// cluster never accepted the Job" have different fixes.
func checkNotRunDetail(res RunResult, runErr error, rollout RolloutOutcome) string {
	detail := checkNotRunCause(res, runErr)
	if rollout.Settled {
		return dependencyDetail(detail)
	}
	// The clause is kept INSIDE the same 300-byte bound every detail takes, by trimming the cause
	// rather than the clause: a pod message long enough to overflow is exactly the case where "the
	// app never became ready" explains the most, so it must not be the half that is cut.
	if room := dependencyDetailBytes - len(rolloutNotReadyClause) - len("; ") - detailTruncationBytes; len(detail) > room {
		detail = boundText(detail, room)
	}
	return dependencyDetail(detail + "; " + rolloutNotReadyClause)
}

// checkNotRunCause names what stopped the check, in the style of the probe's own statuses: what was
// tried, and what came back.
func checkNotRunCause(res RunResult, runErr error) string {
	var blocked *JobBlockedError
	switch {
	case res.TimedOut:
		return fmt.Sprintf("the check pod Burrow ran in the app's own image did not finish inside its %s window", dependencyCheckDeadline)
	case errors.As(runErr, &blocked):
		return blocked.Issue
	case errors.Is(runErr, ErrNoProbeResult):
		return "Burrow ran a check pod in the app's own image and it printed no result line, which is what an image whose entrypoint wraps the command and discards its output leaves"
	case errors.Is(runErr, ErrProbeResultUnreadable):
		return "Burrow ran a check pod in the app's own image and could not read the result line it printed"
	case errors.Is(runErr, context.Canceled), errors.Is(runErr, context.DeadlineExceeded):
		return "Burrow stopped waiting for the check pod it ran in the app's own image before the pod reported"
	default:
		return "Burrow could not run a check pod in the app's own image to completion: the cluster did not accept or finish the check Job"
	}
}

// recordDependencyCheck writes the check's outcome to the audit log — the durable, after-the-fact
// record of a step Burrow executed (ADR-0074 §7's half of the split; see this file's header for why
// it is not a ledger row).
//
// The row carries the KIND, the OUTCOME and the REASON of each check and nothing else. Not the
// detail, and certainly not the credential: the detail is bounded and Burrow-authored, but a stored
// row outlives the request that produced it, and the reason from the closed set is the part a
// reviewer branches on anyway — the same trade auditableHookError makes when it drops a hook's
// captured output.
func (e *Engine) recordDependencyCheck(ctx context.Context, app, env, image string, results []DependencyResult) {
	if len(results) == 0 {
		return
	}
	parts := make([]string, 0, len(results))
	var failed []string
	for _, r := range results {
		part := fmt.Sprintf("%s=%s", r.Kind, r.Outcome)
		if r.Reason != "" {
			part += "(" + r.Reason + ")"
		}
		parts = append(parts, part)
		if r.Failed() {
			failed = append(failed, string(r.Kind))
		}
	}
	sort.Strings(parts)
	args := map[string]string{
		"env":     env,
		"image":   image,
		"results": strings.Join(parts, " "),
	}
	var outcome error
	if len(failed) > 0 {
		outcome = fmt.Errorf("dependency check failed for %s: %s", app, strings.Join(failed, ", "))
	}
	e.recordExecution(ctx, auditOpDependencyCheck, app, args, outcome)
}

// DependencyFailureHint is the non-blocking note a deploy carries when a dependency check failed. It
// states the two things a reader needs and would otherwise guess at: that the release is serving,
// and that the failure is about a thing Burrow itself provisioned, so it is worth believing.
//
// It says the deploy is LIVE and stops there. Saying it "was NOT rolled back" named a mechanism that
// does not exist — no dependency check reverts a deploy — so a reader who took the reassurance
// literally would expect a rollback the next time a check failed (issue #474).
const DependencyFailureHint = "a dependency Burrow provisioned for this app did not answer from inside the app's own container after this deploy. The deploy is live and the running version is the one just deployed. See `dependencies` on this result for which one and why, and `burrow app checks <app>` for what Burrow checks and how to turn it off."

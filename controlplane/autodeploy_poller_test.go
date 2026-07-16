// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package controlplane_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand"
	"strings"
	"testing"
	"time"

	cp "github.com/burrow-cloud/burrow/controlplane"
	"github.com/burrow-cloud/burrow/controlplane/internal/fake"
	"github.com/burrow-cloud/burrow/controlplane/registry"
)

// pollerHarness bundles an engine wired with a fake registry, its fakes, and the poller under test,
// so a test can seed running releases and tags, drive one reconcile pass, and assert what deployed.
type pollerHarness struct {
	poller *cp.AutoDeployPoller
	db     *fake.Database
	k8s    *fake.Kubernetes
	clock  *fake.Clock
	reg    *fake.Registry
}

// newPollerHarness builds the harness. cfg tunes the poller; the zero value applies the production
// defaults. Guardrails are permissive so a deploy is not held for reasons unrelated to auto-deploy.
func newPollerHarness(t *testing.T, cfg cp.AutoDeployConfig) *pollerHarness {
	t.Helper()
	k := fake.NewKubernetes()
	d := fake.NewDatabase()
	d.SetPolicy(permissive())
	c := fake.NewClock(time.Date(2026, 6, 23, 12, 0, 0, 0, time.UTC))
	reg := fake.NewRegistry()
	e, err := cp.New(cp.Deps{
		Kubernetes: k, Database: d, Clock: c, IDs: fake.NewIDs(), Resolver: fake.NewResolver(),
		Credentials: fake.NewCredentials(), DNS: fake.NewDNSFactory(), RegistryClient: reg,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return &pollerHarness{poller: e.NewAutoDeployPoller(cfg), db: d, k8s: k, clock: c, reg: reg}
}

// seedRelease records a deployed release for app in the default environment at image, so
// LatestRelease and AutoDeployCandidates surface it. id is unique per call.
func seedRelease(t *testing.T, d *fake.Database, id, app, image string) {
	t.Helper()
	if err := d.SaveRelease(context.Background(), cp.Release{
		ID: id, App: app, Image: image, Environment: "default", Status: cp.ReleaseDeployed,
	}); err != nil {
		t.Fatalf("SaveRelease: %v", err)
	}
}

// optIn turns auto-deploy on for app in the default environment at level — auto-deploy is off by
// default (opt-in, ADR-0054), so a poller test that expects the watcher to move an app must set a
// level first.
func optIn(t *testing.T, d *fake.Database, app string, level cp.AutoDeployLevel) {
	t.Helper()
	if err := d.SetAutoDeployLevel(context.Background(), app, "default", level); err != nil {
		t.Fatalf("SetAutoDeployLevel(%s, %s): %v", app, level, err)
	}
}

// latest returns the newest release for app in the default environment.
func latest(t *testing.T, d *fake.Database, app string) cp.Release {
	t.Helper()
	r, err := d.LatestRelease(context.Background(), app, "default")
	if err != nil {
		t.Fatalf("LatestRelease(%s): %v", app, err)
	}
	return r
}

// releaseCount returns how many releases app has in the default environment.
func releaseCount(t *testing.T, d *fake.Database, app string) int {
	t.Helper()
	rels, err := d.Releases(context.Background(), app, "default")
	if err != nil {
		t.Fatalf("Releases(%s): %v", app, err)
	}
	return len(rels)
}

// runningImage returns the image of the newest DEPLOYED release for app — the one actually running,
// skipping any failed attempt left on top of the history.
func runningImage(t *testing.T, d *fake.Database, app string) string {
	t.Helper()
	rels, err := d.Releases(context.Background(), app, "default")
	if err != nil {
		t.Fatalf("Releases(%s): %v", app, err)
	}
	for i := len(rels) - 1; i >= 0; i-- {
		if rels[i].Status == cp.ReleaseDeployed {
			return rels[i].Image
		}
	}
	return ""
}

// imageTagOf extracts the tag from a pullable image reference (the segment after the final ':', which
// follows the final '/'), or "" when there is none.
func imageTagOf(ref string) string {
	slash := strings.LastIndexByte(ref, '/')
	colon := strings.LastIndexByte(ref, ':')
	if colon <= slash {
		return ""
	}
	return ref[colon+1:]
}

// TestPollerDeploysNewerInScopeTag: a newer tag within the level is auto-deployed through the guarded
// path, stamped with the auto provenance, and listed anonymously (ADR-0052 §1/§2/§5).
func TestPollerDeploysNewerInScopeTag(t *testing.T) {
	h := newPollerHarness(t, cp.AutoDeployConfig{})
	seedRelease(t, h.db, "web-1", "web", "ghcr.io/u/web:1.2.5")
	optIn(t, h.db, "web", cp.AutoDeployMinor) // auto-deploy is opt-in (ADR-0054)
	h.reg.SetTags("1.2.5", "1.2.6")

	h.poller.ReconcileOnceForTest(context.Background())

	got := latest(t, h.db, "web")
	if got.Image != "ghcr.io/u/web:1.2.6" {
		t.Fatalf("running image = %q, want ghcr.io/u/web:1.2.6", got.Image)
	}
	if got.Trigger != cp.TriggerAuto {
		t.Errorf("trigger = %q, want auto", got.Trigger)
	}
	if got.AutoLevel != cp.AutoDeployMinor || got.AutoTag != "1.2.6" {
		t.Errorf("provenance = {level %q, tag %q}, want {minor, 1.2.6}", got.AutoLevel, got.AutoTag)
	}
	if got.Status != cp.ReleaseDeployed {
		t.Errorf("status = %q, want deployed", got.Status)
	}
	// The read/watch lists the repository (tag stripped) anonymously in this phase (ADR-0052 §7).
	if ref := h.reg.LastRef(); ref != "ghcr.io/u/web" {
		t.Errorf("listed ref = %q, want ghcr.io/u/web (repository, tag stripped)", ref)
	}
	if auth := h.reg.LastAuth(); auth != (cp.RegistryAuth{}) {
		t.Errorf("listed with auth %+v, want anonymous (zero value)", auth)
	}
}

// TestPollerDoesNotDeployAboveLevel: a tag above the level's cap is never auto-taken — it is only a
// surfaced available upgrade (ADR-0052 §3). A 1.3.0 under patch and a 2.0.0 under minor both stay put.
func TestPollerDoesNotDeployAboveLevel(t *testing.T) {
	cases := []struct {
		name  string
		level cp.AutoDeployLevel
		tags  []string
	}{
		{"patch does not cross minor", cp.AutoDeployPatch, []string{"1.2.5", "1.3.0"}},
		{"minor does not cross major", cp.AutoDeployMinor, []string{"1.2.5", "2.0.0"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newPollerHarness(t, cp.AutoDeployConfig{})
			seedRelease(t, h.db, "web-1", "web", "ghcr.io/u/web:1.2.5")
			if err := h.db.SetAutoDeployLevel(context.Background(), "web", "default", tc.level); err != nil {
				t.Fatalf("SetAutoDeployLevel: %v", err)
			}
			h.reg.SetTags(tc.tags...)

			h.poller.ReconcileOnceForTest(context.Background())

			if n := releaseCount(t, h.db, "web"); n != 1 {
				t.Fatalf("release count = %d, want 1 (nothing above the level auto-deploys)", n)
			}
			if img := latest(t, h.db, "web").Image; img != "ghcr.io/u/web:1.2.5" {
				t.Errorf("running image = %q, want unchanged 1.2.5", img)
			}
		})
	}
}

// TestPollerSkipsOffAndDisabled: an app set to off, and one turned off by the rollback safety stop,
// are both skipped — and the poller never re-enables a disabled app (ADR-0052 §5).
func TestPollerSkipsOffAndDisabled(t *testing.T) {
	ctx := context.Background()
	t.Run("explicit off", func(t *testing.T) {
		h := newPollerHarness(t, cp.AutoDeployConfig{})
		seedRelease(t, h.db, "web-1", "web", "ghcr.io/u/web:1.2.5")
		if err := h.db.SetAutoDeployLevel(ctx, "web", "default", cp.AutoDeployOff); err != nil {
			t.Fatalf("SetAutoDeployLevel off: %v", err)
		}
		h.reg.SetTags("1.2.5", "1.2.6", "1.3.0")

		h.poller.ReconcileOnceForTest(ctx)

		if n := releaseCount(t, h.db, "web"); n != 1 {
			t.Fatalf("release count = %d, want 1 (off never deploys)", n)
		}
		if h.reg.Calls() != 0 {
			t.Errorf("registry calls = %d, want 0 (an off app is not even listed)", h.reg.Calls())
		}
	})
	t.Run("disabled by rollback stays disabled", func(t *testing.T) {
		h := newPollerHarness(t, cp.AutoDeployConfig{})
		seedRelease(t, h.db, "web-1", "web", "ghcr.io/u/web:1.2.5")
		if err := h.db.DisableAutoDeploy(ctx, "web", "default", "disabled by rollback"); err != nil {
			t.Fatalf("DisableAutoDeploy: %v", err)
		}
		h.reg.SetTags("1.2.5", "1.2.6")

		h.poller.ReconcileOnceForTest(ctx)

		if n := releaseCount(t, h.db, "web"); n != 1 {
			t.Fatalf("release count = %d, want 1 (a disabled app never deploys)", n)
		}
		if lvl, _ := h.db.AutoDeployLevel(ctx, "web", "default"); lvl != cp.AutoDeployOff {
			t.Errorf("level = %q, want still off (poller must not re-enable)", lvl)
		}
		if reason, _ := h.db.AutoDeployReason(ctx, "web", "default"); reason != "disabled by rollback" {
			t.Errorf("reason = %q, want preserved 'disabled by rollback'", reason)
		}
	})
}

// TestPollerSkipsAppWithNoLevelSet: an app that has never opted into auto-deploy (no stored level, so
// it reads the off default — ADR-0054) is not polled at all. This is the fix for the post-upgrade
// regression (#270): a pre-existing app is read as off and skipped BEFORE any registry call, so no
// tag listing happens and no 401 is logged, even though a newer in-scope tag exists.
func TestPollerSkipsAppWithNoLevelSet(t *testing.T) {
	ctx := context.Background()
	h := newPollerHarness(t, cp.AutoDeployConfig{})
	seedRelease(t, h.db, "web-1", "web", "ghcr.io/u/web:1.2.5")
	// Deliberately set NO level: the app predates auto-deploy and never opted in.
	h.reg.SetTags("1.2.5", "1.2.6", "1.3.0") // newer in-scope tags are available, but must be ignored

	// Sanity: with no row the level reads off, matching the opt-in default.
	if lvl, _ := h.db.AutoDeployLevel(ctx, "web", "default"); lvl != cp.AutoDeployOff {
		t.Fatalf("unset level = %q, want off (the opt-in default)", lvl)
	}

	h.poller.ReconcileOnceForTest(ctx)

	if n := releaseCount(t, h.db, "web"); n != 1 {
		t.Fatalf("release count = %d, want 1 (an app that never opted in is never auto-deployed)", n)
	}
	if h.reg.Calls() != 0 {
		t.Errorf("registry calls = %d, want 0 (an unopted app is never even listed, so no 401)", h.reg.Calls())
	}
}

// TestPollerDedupesListingFailureLog: a standing tag-listing failure for an opted-in app (e.g. a
// persistent 401 for a private repo the poller has no read credential for — read auth is #279) is
// logged once, not every interval, so it is not spammy. The line logs again only when the error
// changes or clears.
func TestPollerDedupesListingFailureLog(t *testing.T) {
	ctx := context.Background()
	h := newPollerHarness(t, cp.AutoDeployConfig{})
	seedRelease(t, h.db, "web-1", "web", "ghcr.io/u/web:1.2.5")
	optIn(t, h.db, "web", cp.AutoDeployMinor)
	h.reg.SetError(errors.New("token request failed (http 401): authentication required"))

	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	defer slog.SetDefault(prev)

	// Three passes with the same standing 401.
	for i := 0; i < 3; i++ {
		h.poller.ReconcileOnceForTest(ctx)
	}
	if n := strings.Count(buf.String(), "listing tags failed"); n != 1 {
		t.Errorf("listing failure logged %d times for a standing error, want 1 (de-duplicated)", n)
	}

	// A DIFFERENT error is a new fact and logs again.
	h.reg.SetError(errors.New("registry unreachable"))
	h.poller.ReconcileOnceForTest(ctx)
	if n := strings.Count(buf.String(), "listing tags failed"); n != 2 {
		t.Errorf("after a changed error, logged %d times, want 2 (the new error logs)", n)
	}
}

// TestPollerIsolatesRegistryError: a registry failure for app A does not stop the loop — app B is
// still reconciled and deployed in the same pass (ADR-0040 fail-soft isolation).
func TestPollerIsolatesRegistryError(t *testing.T) {
	ctx := context.Background()
	h := newPollerHarness(t, cp.AutoDeployConfig{})
	seedRelease(t, h.db, "aaa-1", "aaa", "ghcr.io/u/aaa:1.0.0")
	seedRelease(t, h.db, "bbb-1", "bbb", "ghcr.io/u/bbb:1.0.0")
	optIn(t, h.db, "aaa", cp.AutoDeployMinor)
	optIn(t, h.db, "bbb", cp.AutoDeployMinor)
	// aaa's registry listing fails; bbb lists a newer in-scope tag.
	h.reg.SetErrorFor("ghcr.io/u/aaa", errors.New("registry unreachable"))
	h.reg.SetTagsFor("ghcr.io/u/bbb", "1.0.0", "1.1.0")

	h.poller.ReconcileOnceForTest(ctx)

	if n := releaseCount(t, h.db, "aaa"); n != 1 {
		t.Errorf("aaa release count = %d, want 1 (its registry error is isolated)", n)
	}
	if img := latest(t, h.db, "bbb").Image; img != "ghcr.io/u/bbb:1.1.0" {
		t.Errorf("bbb running image = %q, want 1.1.0 (progress despite aaa's error)", img)
	}
}

// TestPollerEqualOrOlderTagNoOp: neither the running tag nor an older backport is ever taken —
// ResolveAutoDeploy is upgrades-only, so the watcher can only move an app forward (ADR-0052 §2).
func TestPollerEqualOrOlderTagNoOp(t *testing.T) {
	h := newPollerHarness(t, cp.AutoDeployConfig{})
	seedRelease(t, h.db, "web-1", "web", "ghcr.io/u/web:1.5.0")
	optIn(t, h.db, "web", cp.AutoDeployMinor)
	h.reg.SetTags("1.5.0", "1.4.9") // equal + an older backport

	h.poller.ReconcileOnceForTest(context.Background())

	if n := releaseCount(t, h.db, "web"); n != 1 {
		t.Fatalf("release count = %d, want 1 (never redeploys equal, never downgrades)", n)
	}
}

// TestPollerNonSemverCurrentSkipped: an app running a non-semver tag cannot be classified, so nothing
// auto-deploys (ADR-0052 §4).
func TestPollerNonSemverCurrentSkipped(t *testing.T) {
	h := newPollerHarness(t, cp.AutoDeployConfig{})
	seedRelease(t, h.db, "web-1", "web", "ghcr.io/u/web:latest")
	optIn(t, h.db, "web", cp.AutoDeployMinor)
	h.reg.SetTags("1.2.6", "1.3.0")

	h.poller.ReconcileOnceForTest(context.Background())

	if n := releaseCount(t, h.db, "web"); n != 1 {
		t.Fatalf("release count = %d, want 1 (a non-semver running tag is never auto-upgraded)", n)
	}
}

// TestPollerBacksOffFailedTag: after a tag's deploy fails, the poller does not re-attempt that tag on
// the next pass — no redeploy crash-loop — but recovers once the backoff elapses and the fault clears
// (ADR-0052 §5).
func TestPollerBacksOffFailedTag(t *testing.T) {
	ctx := context.Background()
	backoff := 30 * time.Minute
	h := newPollerHarness(t, cp.AutoDeployConfig{Backoff: backoff})
	seedRelease(t, h.db, "web-1", "web", "ghcr.io/u/web:1.2.5")
	optIn(t, h.db, "web", cp.AutoDeployMinor)
	h.reg.SetTags("1.2.5", "1.2.6")

	// The cluster rejects the apply, so the auto-deploy of 1.2.6 fails and is recorded Failed.
	h.k8s.SetError(fake.OpApply, errors.New("apiserver unavailable"))
	h.poller.ReconcileOnceForTest(ctx)
	if n := releaseCount(t, h.db, "web"); n != 2 {
		t.Fatalf("after first pass: release count = %d, want 2 (seed + one failed attempt)", n)
	}
	if got := latest(t, h.db, "web"); got.Status != cp.ReleaseFailed || got.Image != "ghcr.io/u/web:1.2.6" {
		t.Fatalf("latest after failure = {%q, %q}, want {failed, 1.2.6}", got.Status, got.Image)
	}

	// A second pass at the same time must NOT re-attempt 1.2.6 (still within backoff), even though the
	// fault would still be present — no crash-loop.
	h.poller.ReconcileOnceForTest(ctx)
	if n := releaseCount(t, h.db, "web"); n != 2 {
		t.Fatalf("after second pass within backoff: release count = %d, want still 2 (no retry)", n)
	}

	// Clear the fault and advance past the backoff: the poller retries and succeeds.
	h.k8s.SetError(fake.OpApply, nil)
	h.clock.Advance(backoff + time.Minute)
	h.poller.ReconcileOnceForTest(ctx)
	got := latest(t, h.db, "web")
	if got.Status != cp.ReleaseDeployed || got.Image != "ghcr.io/u/web:1.2.6" {
		t.Fatalf("latest after recovery = {%q, %q}, want {deployed, 1.2.6}", got.Status, got.Image)
	}
}

// TestPollerHonorsRetryAfter: a registry 429 with a Retry-After mutes that app for the honored window
// — it is not polled again until the window elapses, then it deploys (ADR-0052 §7). It uses the real
// registry.RateLimitError to prove the hint interface is wired end-to-end.
func TestPollerHonorsRetryAfter(t *testing.T) {
	ctx := context.Background()
	h := newPollerHarness(t, cp.AutoDeployConfig{})
	seedRelease(t, h.db, "web-1", "web", "ghcr.io/u/web:1.2.5")
	optIn(t, h.db, "web", cp.AutoDeployMinor)

	// First pass: the registry rate-limits with Retry-After: 120 (seconds).
	h.reg.SetError(&registry.RateLimitError{RetryAfter: "120"})
	h.poller.ReconcileOnceForTest(ctx)
	callsAfterRateLimit := h.reg.Calls()
	if callsAfterRateLimit == 0 {
		t.Fatalf("expected the registry to be listed once on the first pass")
	}

	// Tags are now available and the error is cleared, but within the Retry-After window the app is
	// muted: it is not even listed again, and nothing deploys.
	h.reg.SetError(nil)
	h.reg.SetTags("1.2.5", "1.2.6")
	h.clock.Advance(119 * time.Second)
	h.poller.ReconcileOnceForTest(ctx)
	if h.reg.Calls() != callsAfterRateLimit {
		t.Errorf("registry listed during the Retry-After window (calls %d -> %d); want muted", callsAfterRateLimit, h.reg.Calls())
	}
	if n := releaseCount(t, h.db, "web"); n != 1 {
		t.Fatalf("release count = %d, want 1 while muted", n)
	}

	// Past the window, the app is polled again and the newer tag deploys.
	h.clock.Advance(2 * time.Second)
	h.poller.ReconcileOnceForTest(ctx)
	if img := latest(t, h.db, "web").Image; img != "ghcr.io/u/web:1.2.6" {
		t.Errorf("running image = %q, want 1.2.6 after the Retry-After window", img)
	}
}

// TestPollerRunDisabledWithoutRegistry: with no registry seam wired, Run returns immediately rather
// than spinning — the watcher is optional and degrades to off (ADR-0040).
func TestPollerRunDisabledWithoutRegistry(t *testing.T) {
	k := fake.NewKubernetes()
	d := fake.NewDatabase()
	d.SetPolicy(permissive())
	e, err := cp.New(cp.Deps{
		Kubernetes: k, Database: d, Clock: fake.NewClock(time.Now()), IDs: fake.NewIDs(),
		Resolver: fake.NewResolver(), Credentials: fake.NewCredentials(), DNS: fake.NewDNSFactory(),
		// no RegistryClient
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	p := e.NewAutoDeployPoller(cp.AutoDeployConfig{})
	done := make(chan struct{})
	go func() { p.Run(context.Background()); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return promptly with no registry wired")
	}
}

// TestPollerRunReconcilesThenStops: Run runs a reconcile pass immediately (deploying a newer tag),
// then honors context cancellation and returns. The After seam is injected so no real time passes.
func TestPollerRunReconcilesThenStops(t *testing.T) {
	h := newPollerHarness(t, cp.AutoDeployConfig{
		Interval: time.Second,
		// After never fires: the loop blocks on it after the first pass, so cancellation is the only
		// way out — proving the pass runs before any wait and that cancellation is honored.
		After: func(time.Duration) <-chan time.Time { return make(chan time.Time) },
	})
	seedRelease(t, h.db, "web-1", "web", "ghcr.io/u/web:1.2.5")
	optIn(t, h.db, "web", cp.AutoDeployMinor)
	h.reg.SetTags("1.2.5", "1.2.6")

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { h.poller.Run(ctx); close(done) }()

	// Wait for the first pass to land the deploy.
	deadline := time.After(2 * time.Second)
	for {
		if releaseCount(t, h.db, "web") == 2 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("first reconcile pass did not deploy within the deadline")
		case <-time.After(5 * time.Millisecond):
		}
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after cancellation")
	}
	if img := latest(t, h.db, "web").Image; img != "ghcr.io/u/web:1.2.6" {
		t.Errorf("running image = %q, want 1.2.6", img)
	}
}

// TestPollerAdversarialSchedule is the ADR-0010-style deterministic fault-injection test: it drives a
// fleet of apps through many reconcile passes under a seeded schedule of registry faults, rate
// limits, and shifting tag sets, advancing only the injected clock, and asserts the load-bearing
// invariants hold throughout — never a downgrade, never a duplicate deploy of a tag already running,
// isolation of per-app failures, and forward progress.
func TestPollerAdversarialSchedule(t *testing.T) {
	ctx := context.Background()
	const backoff = 20 * time.Minute
	h := newPollerHarness(t, cp.AutoDeployConfig{Backoff: backoff})

	// A small fleet, each on a different level, starting on a known semver.
	type app struct {
		name  string
		repo  string
		level cp.AutoDeployLevel
	}
	apps := []app{
		{"web", "ghcr.io/u/web", cp.AutoDeployMinor},
		{"api", "ghcr.io/u/api", cp.AutoDeployPatch},
		{"job", "ghcr.io/u/job", cp.AutoDeployMajor},
		{"pin", "ghcr.io/u/pin", cp.AutoDeployOff},
	}
	for i, a := range apps {
		seedRelease(t, h.db, fmt.Sprintf("%s-seed-%d", a.name, i), a.name, a.repo+":1.0.0")
		if err := h.db.SetAutoDeployLevel(ctx, a.name, "default", a.level); err != nil {
			t.Fatalf("SetAutoDeployLevel(%s): %v", a.name, err)
		}
	}

	// Per-app running-version history, so we can assert monotonic (never-downgrading) progress and
	// detect a duplicate redeploy of the tag already running.
	seen := map[string][]string{}
	record := func() {
		for _, a := range apps {
			cur := imageTagOf(runningImage(t, h.db, a.name))
			hist := seen[a.name]
			if n := len(hist); n > 0 && hist[n-1] == cur {
				continue // unchanged this pass
			}
			seen[a.name] = append(hist, cur)
		}
	}
	record()

	rng := rand.New(rand.NewSource(0xB0BA))
	// A pool of tags the registries drift through over the run; a mix of in-scope, above-cap, older
	// backports, and non-semver noise.
	pool := []string{"1.0.0", "1.0.1", "1.1.0", "1.2.0", "0.9.9", "2.0.0", "latest", "sha-abc"}

	for pass := 0; pass < 40; pass++ {
		// Reseed each registry with a random subset of the pool, and randomly inject a fault or a rate
		// limit for some apps — a seeded, reproducible adversarial schedule.
		for _, a := range apps {
			h.reg.SetErrorFor(a.repo, nil)
			switch rng.Intn(6) {
			case 0:
				h.reg.SetErrorFor(a.repo, errors.New("transient registry error"))
			case 1:
				h.reg.SetErrorFor(a.repo, &registry.RateLimitError{RetryAfter: "60"})
			default:
				tags := make([]string, 0, len(pool))
				for _, tg := range pool {
					if rng.Intn(2) == 0 {
						tags = append(tags, tg)
					}
				}
				h.reg.SetTagsFor(a.repo, tags...)
			}
		}
		// Occasionally the cluster rejects applies, exercising the failed-tag backoff.
		if rng.Intn(4) == 0 {
			h.k8s.SetError(fake.OpApply, errors.New("apiserver flake"))
		} else {
			h.k8s.SetError(fake.OpApply, nil)
		}

		h.poller.ReconcileOnceForTest(ctx)
		record()
		h.clock.Advance(6 * time.Minute)
	}

	// Invariant checks.
	for _, a := range apps {
		hist := seen[a.name]
		// The pinned (off) app must never have moved off its seed.
		if a.level == cp.AutoDeployOff {
			if len(hist) != 1 || hist[0] != "1.0.0" {
				t.Errorf("%s (off) history = %v, want [1.0.0] (never auto-deployed)", a.name, hist)
			}
			continue
		}
		for i := 1; i < len(hist); i++ {
			prev, cur := hist[i-1], hist[i]
			// Never a downgrade: each recorded move is strictly greater by semver.
			if cp.CompareTagsForTest(cur, prev) <= 0 {
				t.Errorf("%s history not monotonically increasing: %v (%s not > %s)", a.name, hist, cur, prev)
			}
			// A patch-level app never crosses its minor; a minor-level app never crosses its major.
			switch a.level {
			case cp.AutoDeployPatch:
				if !cp.SameMinorForTest(cur, "1.0.0") {
					t.Errorf("%s (patch) moved to %s, crossing minor from 1.0.x", a.name, cur)
				}
			case cp.AutoDeployMinor:
				if !cp.SameMajorForTest(cur, "1.0.0") {
					t.Errorf("%s (minor) moved to %s, crossing major from 1.x", a.name, cur)
				}
			}
		}
		// No duplicate: the recorded history has no repeated version (a tag already running is never
		// redeployed).
		for i := 1; i < len(hist); i++ {
			for j := 0; j < i; j++ {
				if hist[i] == hist[j] {
					t.Errorf("%s redeployed a tag it already ran: %v", a.name, hist)
				}
			}
		}
	}
}

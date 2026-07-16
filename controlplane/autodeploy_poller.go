// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package controlplane

import (
	"context"
	"errors"
	"log/slog"
	"math/rand"
	"strconv"
	"strings"
	"sync"
	"time"
)

// DefaultAutoDeployInterval is the conservative default poll cadence (ADR-0052 §7): ~5 minutes.
// The watcher is deliberately not a low-latency channel, because it need not be — the explicit
// deploy is always the immediate path — so a conservative interval protects registry rate limits
// without a latency complaint.
const DefaultAutoDeployInterval = 5 * time.Minute

// DefaultAutoDeployBackoff is how long the watcher holds off after a tag's deploy fails (or after a
// registry rate-limits it with no Retry-After hint): a failed tag is not re-attempted before this,
// so a bad image cannot become a redeploy crash-loop (ADR-0052 §5). A strictly newer tag is always
// tried; the hold applies only to re-attempting the exact tag that failed.
const DefaultAutoDeployBackoff = time.Hour

// retryAfterHinter is implemented by a registry error that carries a raw Retry-After value
// (registry.RateLimitError), so the poller can honor the registry's pushback (ADR-0052 §7) without
// importing the registry adapter — that package imports controlplane, so importing it back would
// cycle.
type retryAfterHinter interface {
	RetryAfterHint() string
}

// AutoDeployConfig configures the pull-based watcher (ADR-0052 Phase 4b). The zero value is valid:
// every field falls back to a safe default, so cmd/burrowd can start the poller with an empty
// config, and a test can drive it deterministically by supplying Interval, Jitter, and After.
type AutoDeployConfig struct {
	// Interval is the base poll cadence. Zero or negative applies DefaultAutoDeployInterval.
	Interval time.Duration
	// Backoff is how long a failed tag (or a hint-less rate-limit) is held off. Zero applies
	// DefaultAutoDeployBackoff.
	Backoff time.Duration
	// Jitter returns the actual wait before the next cycle, given the base interval, so many
	// pollers do not stampede the registry in lockstep (ADR-0052 §7). Nil applies a default of
	// the base interval +/- up to 10%. It is injected so a test drives the cadence deterministically.
	Jitter func(base time.Duration) time.Duration
	// After returns a channel that fires after d — the seam the run loop waits on between cycles.
	// Nil applies time.After. It is injected (rather than reading the wall clock) so a test drives
	// the loop with no real sleeping and no ambient time (ADR-0010).
	After func(d time.Duration) <-chan time.Time
}

// AutoDeployPoller is the pull-based passive-deploy watcher (ADR-0052): on a bounded, jittered
// cadence it lists each app's image tags and, when a newer tag within the app's auto-deploy level
// exists, drives the SAME guarded deploy an explicit call would — same rollout, deploy record,
// rollback handle, and audit entry, distinguished only by its auto provenance. It is outbound-only
// (it never accepts an inbound connection) and fail-soft: one app's registry error or deploy
// failure is logged and never stops the loop or affects another app (ADR-0040). It reads no ambient
// time or randomness — the cadence comes from the injected After/Jitter seams and every backoff
// deadline from the engine's injected clock (ADR-0010).
type AutoDeployPoller struct {
	engine   *Engine
	interval time.Duration
	backoff  time.Duration
	jitter   func(base time.Duration) time.Duration
	after    func(d time.Duration) <-chan time.Time

	mu    sync.Mutex
	state map[AppEnvRef]*autoDeployState
}

// autoDeployState is the poller's in-memory per-(app,env) memory that prevents thrashing (ADR-0052
// §5). It is not persisted: on restart the watcher simply re-evaluates from the registry and the
// running release, which is safe because ResolveAutoDeploy is upgrades-only.
type autoDeployState struct {
	failedTag   string    // the deploy tag that last failed for this pair, "" if none
	retryTagAt  time.Time // failedTag is not re-attempted before this
	mutedUntil  time.Time // the registry rate-limited this pair; skip it until this
	lastListErr string    // the last tag-listing error logged for this pair, "" once it cleared
}

// NewAutoDeployPoller builds the watcher bound to this engine. It reads the engine's registry,
// database, and clock seams, and drives the same unexported guarded deploy path an explicit call
// uses, stamped with an auto provenance (ADR-0052 §5). Constructing it does not start it — call Run.
func (e *Engine) NewAutoDeployPoller(cfg AutoDeployConfig) *AutoDeployPoller {
	interval := cfg.Interval
	if interval <= 0 {
		interval = DefaultAutoDeployInterval
	}
	backoff := cfg.Backoff
	if backoff <= 0 {
		backoff = DefaultAutoDeployBackoff
	}
	jitter := cfg.Jitter
	if jitter == nil {
		// Seed the default jitter from the injected clock rather than an ambient source, so the
		// poller reads no ambient randomness at construction (ADR-0010). Jitter runs only in Run's
		// single goroutine, so an unshared *rand.Rand is safe.
		rng := rand.New(rand.NewSource(e.clock.Now().UnixNano()))
		jitter = func(base time.Duration) time.Duration { return jitterAround(rng, base) }
	}
	after := cfg.After
	if after == nil {
		after = time.After
	}
	return &AutoDeployPoller{
		engine:   e,
		interval: interval,
		backoff:  backoff,
		jitter:   jitter,
		after:    after,
		state:    make(map[AppEnvRef]*autoDeployState),
	}
}

// Run polls until ctx is cancelled, reconciling every candidate (app, env) on each cycle. It runs a
// reconcile pass immediately, then waits a jittered interval before the next, honoring cancellation
// promptly (ADR-0052 §7). With no registry seam wired it logs and returns rather than spinning — the
// watcher is optional and degrades to off when the outbound registry client is absent (ADR-0040).
func (p *AutoDeployPoller) Run(ctx context.Context) {
	if p.engine.registry == nil {
		slog.InfoContext(ctx, "auto-deploy poller disabled: no registry seam wired")
		return
	}
	slog.InfoContext(ctx, "auto-deploy poller started", "interval", p.interval)
	for {
		p.reconcile(ctx)
		select {
		case <-ctx.Done():
			slog.InfoContext(ctx, "auto-deploy poller stopped", "reason", ctx.Err())
			return
		case <-p.after(p.jitter(p.interval)):
		}
	}
}

// reconcile runs one poll pass over every candidate (app, env). A failure listing candidates aborts
// only this pass; a per-app failure is isolated inside reconcileOne and never stops the pass.
func (p *AutoDeployPoller) reconcile(ctx context.Context) {
	candidates, err := p.engine.db.AutoDeployCandidates(ctx)
	if err != nil {
		slog.WarnContext(ctx, "auto-deploy poll: listing candidates failed", "error", err)
		return
	}
	for _, ref := range candidates {
		if ctx.Err() != nil {
			return
		}
		p.reconcileOne(ctx, ref)
	}
}

// reconcileOne reconciles a single (app, env): read the level, skip if off; read the running
// release and list the repository's tags; and if ResolveAutoDeploy returns a tag strictly newer than
// running and within the level, drive the guarded auto-deploy. Every failure here is contained — it
// logs and returns, so one app never affects another (ADR-0040). A tag above the level's cap is
// only surfaced (elsewhere) as an available upgrade, never auto-taken (ADR-0052 §3).
func (p *AutoDeployPoller) reconcileOne(ctx context.Context, ref AppEnvRef) {
	now := p.engine.clock.Now()
	st := p.stateFor(ref)

	// The registry pushed back (429): stay quiet for this pair until its Retry-After window elapses.
	if now.Before(st.mutedUntil) {
		return
	}

	level, err := p.engine.db.AutoDeployLevel(ctx, ref.App, ref.Env)
	if err != nil {
		slog.WarnContext(ctx, "auto-deploy poll: reading level failed", "app", ref.App, "env", ref.Env, "error", err)
		return
	}
	// off covers a human off and the rollback/downgrade safety stop, which set the level off with a
	// reason (ADR-0052 §5): either way the watcher does not move the app.
	if level == AutoDeployOff {
		return
	}

	// Compare against the currently RUNNING (last deployed) release, not merely the newest record: a
	// prior failed auto-deploy leaves a failed row on top, and reading that as "current" would poison
	// the comparison. lastDeployed skips failed/pending rows to the real running image.
	releases, err := p.engine.db.Releases(ctx, ref.App, ref.Env)
	if err != nil {
		slog.WarnContext(ctx, "auto-deploy poll: reading release history failed", "app", ref.App, "env", ref.Env, "error", err)
		return
	}
	rel, ok := lastDeployed(releases)
	if !ok {
		return // nothing currently running to compare a registry tag against
	}
	repo := imageRepository(rel.Image)
	current := imageTag(rel.Image)
	if stableSemver(current) == "" {
		// A non-semver running tag cannot be classified, so there is no basis to auto-upgrade it
		// (ADR-0052 §4); the non-semver hint is surfaced on the deploy path, not here.
		return
	}

	// Only opted-in apps reach here (an unset level is off — ADR-0058), so a listing failure below
	// belongs to an app that deliberately turned auto-deploy on. The listing is still anonymous: the
	// poller has no registry READ credential of its own, so a private repository answers 401. Wiring
	// read auth for the poller is tracked separately (issue #279, tied to provider credentials) and
	// is deliberately out of scope here; until it lands, a genuinely opted-in private app logs the
	// failure — but only when it CHANGES, not every interval, so it is not spammy.
	tags, err := p.engine.registry.ListTags(ctx, repo, RegistryAuth{})
	if err != nil {
		var rl retryAfterHinter
		if errors.As(err, &rl) {
			wait := parseRetryAfter(rl.RetryAfterHint(), p.backoff)
			st.mutedUntil = now.Add(wait)
			slog.WarnContext(ctx, "auto-deploy poll: registry rate limited, backing off", "app", ref.App, "env", ref.Env, "backoff", wait)
			return
		}
		// Fail-soft: a registry error for one app is logged and skipped; the loop and other apps go on.
		// De-duplicate the repeated identical failure (e.g. a persistent 401 for a private repo with no
		// poller read credential) to one line per distinct error, so a standing fault is not spammy.
		if msg := err.Error(); msg != st.lastListErr {
			st.lastListErr = msg
			slog.WarnContext(ctx, "auto-deploy poll: listing tags failed", "app", ref.App, "env", ref.Env, "error", err)
		}
		return
	}
	// The listing succeeded: clear any standing failure so the next distinct error logs again.
	st.lastListErr = ""

	target, _ := ResolveAutoDeploy(current, tags, level)
	if target == "" {
		// Nothing within the level to move to. Any higher tag above the cap is surfaced elsewhere as
		// an available upgrade, never auto-taken (ADR-0052 §3). Clear a stale failure hold.
		st.failedTag = ""
		return
	}
	// Do not re-attempt a tag whose deploy just failed until its backoff elapses — no redeploy
	// crash-loop (ADR-0052 §5). A strictly newer tag (target != failedTag) is always tried.
	if target == st.failedTag && now.Before(st.retryTagAt) {
		return
	}

	// The guarded deploy: the same rollout, deploy record, rollback handle, and audit entry an
	// explicit call runs, distinguished only by its auto provenance (ADR-0052 §1/§5). Replicas 0 so
	// the deploy preserves the running count and never rescales.
	req := DeployRequest{App: ref.App, Env: ref.Env, Image: repo + ":" + target}
	if _, err := p.engine.deploy(ctx, req, deployProvenance{trigger: TriggerAuto, level: level, tag: target}); err != nil {
		st.failedTag = target
		st.retryTagAt = now.Add(p.backoff)
		slog.WarnContext(ctx, "auto-deploy failed, holding off this tag", "app", ref.App, "env", ref.Env, "tag", target, "error", err)
		return
	}
	st.failedTag = ""
	slog.InfoContext(ctx, "auto-deployed newer in-scope tag", "app", ref.App, "env", ref.Env, "level", level, "tag", target)
}

// stateFor returns the mutable per-(app,env) backoff state, creating it on first use.
func (p *AutoDeployPoller) stateFor(ref AppEnvRef) *autoDeployState {
	p.mu.Lock()
	defer p.mu.Unlock()
	st := p.state[ref]
	if st == nil {
		st = &autoDeployState{}
		p.state[ref] = st
	}
	return st
}

// parseRetryAfter interprets a registry Retry-After value as a backoff duration. It handles the
// integer-seconds form registries use; an empty, HTTP-date, or otherwise unparseable value falls
// back to fallback, so the poller always backs off by at least a sensible amount (ADR-0052 §7).
func parseRetryAfter(v string, fallback time.Duration) time.Duration {
	if secs, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && secs > 0 {
		return time.Duration(secs) * time.Second
	}
	return fallback
}

// jitterAround returns base perturbed by up to +/-10%, so pollers do not poll the registry in
// lockstep. A non-positive base is returned unchanged.
func jitterAround(rng *rand.Rand, base time.Duration) time.Duration {
	if base <= 0 {
		return base
	}
	spread := int64(base) / 10
	if spread <= 0 {
		return base
	}
	delta := rng.Int63n(2*spread+1) - spread
	return base + time.Duration(delta)
}

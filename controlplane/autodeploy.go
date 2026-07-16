// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package controlplane

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/mod/semver"
)

// AutoDeployLevel is how far the pull-based passive watcher may move an app on its own
// (ADR-0052): it caps auto-deploy to patch, minor, or major upgrades, or turns it off. The
// level is set per app and per environment; the watcher only ever moves an app forward, to the
// highest semver version within the level's cap that is greater than the running release.
type AutoDeployLevel string

const (
	// AutoDeployOff disables auto-deploy: only an explicit CLI or agent deploy ships a release
	// (ADR-0052 §2). Setting off is also how a version is pinned. It is the default: auto-deploy
	// is opt-in, so an app is never polled until an operator sets a level (ADR-0054).
	AutoDeployOff AutoDeployLevel = "off"
	// AutoDeployPatch auto-deploys patches within the current minor only (e.g. 1.2.6, 1.2.7 for
	// an app on 1.2.5); crossing to a new minor is a manual step (ADR-0052 §2).
	AutoDeployPatch AutoDeployLevel = "patch"
	// AutoDeployMinor auto-deploys any patch or minor upgrade within the current major (e.g.
	// 1.2.6, 1.3.0, 1.4.0 for an app on 1.2.5), never a major (ADR-0052 §2).
	AutoDeployMinor AutoDeployLevel = "minor"
	// AutoDeployMajor auto-deploys anything newer, including a breaking major (e.g. 2.0.0) —
	// fully hands-off updates for the operator who accepts the risk (ADR-0052 §2).
	AutoDeployMajor AutoDeployLevel = "major"
)

// DefaultAutoDeployLevel is the level that applies to every app, new or already deployed, until
// an operator sets one: off, so auto-deploy is opt-in — the watcher never polls or moves an app
// that has not deliberately opted in (ADR-0054, revising ADR-0052 §2's on-by-default default).
// This keeps a pre-existing app from being silently polled the moment a cluster is upgraded to a
// version carrying the poller; set a level (patch/minor/major) to turn auto-deploy on.
const DefaultAutoDeployLevel = AutoDeployOff

// AppEnvRef names one (app, environment) pair — a unit the pull-based watcher reconciles
// (ADR-0052 Phase 4b). Env is the canonical environment name (the reserved "default" for the
// implicit default environment).
type AppEnvRef struct {
	App string `json:"app"`
	Env string `json:"env"`
}

// Valid reports whether l is a known auto-deploy level.
func (l AutoDeployLevel) Valid() bool {
	switch l {
	case AutoDeployOff, AutoDeployPatch, AutoDeployMinor, AutoDeployMajor:
		return true
	default:
		return false
	}
}

// ParseAutoDeployLevel parses an auto-deploy level from a CLI argument, rejecting unknown
// values with a message that lists the valid ones (ADR-0052 §6). It is case-insensitive and
// tolerates surrounding whitespace.
func ParseAutoDeployLevel(s string) (AutoDeployLevel, error) {
	l := AutoDeployLevel(strings.ToLower(strings.TrimSpace(s)))
	if !l.Valid() {
		return "", fmt.Errorf("auto-deploy level %q is not valid: want off, patch, minor, or major", s)
	}
	return l, nil
}

// ResolveAutoDeploy decides what an app on `current` should auto-deploy to, given the tags
// `available` in the registry and the auto-deploy `level`. It returns the tag to deploy
// (target, "" if none) and the highest version that exists ABOVE the level's cap (upgrade,
// "" if none) for surfacing as an available upgrade (ADR-0052 §2/§3). Upgrades only; only
// semver tags count.
//
// The returned strings are the ORIGINAL tags as they appeared in `available` (e.g. "v1.2.7"
// stays "v1.2.7"), so the caller deploys the exact tag rather than a normalized form.
//
// Non-semver tags in `available` (latest, a git SHA, a date) are ignored — they cannot be
// classified patch/minor/major, so the watcher never chases them (ADR-0052 §4). Prerelease
// versions (e.g. 1.3.0-rc1) are excluded: only stable releases are eligible for auto-deploy or
// for surfacing as an available upgrade. If `current` does not parse as semver there is no basis
// to compare, so the result is ("", "").
func ResolveAutoDeploy(current string, available []string, level AutoDeployLevel) (target string, upgrade string) {
	// off never deploys and never surfaces an upgrade — the explicit call stays canonical.
	if level == AutoDeployOff {
		return "", ""
	}
	cur := stableSemver(current)
	if cur == "" {
		return "", ""
	}

	var targetTag, upgradeTag string
	var targetVer, upgradeVer string // canonical forms, for comparison only
	for _, tag := range available {
		v := stableSemver(tag)
		if v == "" {
			// Not a stable semver tag (non-semver or a prerelease): ignore it.
			continue
		}
		// Upgrades only: strictly greater than the running release, by semver order — never
		// equal, never lower (a backport like 1.4.9 while on 1.5.0 is skipped).
		if semver.Compare(v, cur) <= 0 {
			continue
		}
		if withinCap(cur, v, level) {
			if targetVer == "" || semver.Compare(v, targetVer) > 0 {
				targetVer, targetTag = v, tag
			}
		} else if upgradeVer == "" || semver.Compare(v, upgradeVer) > 0 {
			// Above the level's cap: not deployed, but the highest such version is surfaced as an
			// available upgrade to take with an explicit deploy (ADR-0052 §3). Under major nothing
			// is above the cap, so upgrade stays "".
			upgradeVer, upgradeTag = v, tag
		}
	}
	return targetTag, upgradeTag
}

// AutoDeployStatus is the enriched, read-only view of an app's auto-deploy configuration in one
// environment (ADR-0052 §2/§3): the level plus, when the registry could be listed, the current
// running version, the tag auto-deploy would move to within the level, and the highest version
// above the level's cap surfaced as an available upgrade. When the upgrade check could not run —
// the registry is unreachable or needs credentials, the current tag is not semver, there is no
// running release, or the check is not wired — Checked is false and Note carries a short human
// reason; the level is still reported. The check is deliberately best-effort: a registry failure
// never errors the show, keeping this path independent of registry reachability (ADR-0040).
type AutoDeployStatus struct {
	App        string          `json:"app"`
	Env        string          `json:"env"`
	Level      AutoDeployLevel `json:"level"`
	Repository string          `json:"repository,omitempty"` // current image reference with the tag stripped (e.g. "ghcr.io/user/app"), for building the deploy hint
	Current    string          `json:"current,omitempty"`    // current running semver tag, when known
	Target     string          `json:"target,omitempty"`     // tag auto-deploy would move to within the level, "" if none / already current
	Upgrade    string          `json:"upgrade,omitempty"`    // highest version above the level's cap, surfaced as an available upgrade, "" if none
	Checked    bool            `json:"checked"`              // whether the registry upgrade check actually ran
	Note       string          `json:"note,omitempty"`       // when Checked is false, a short human reason
	// DisabledReason is why auto-deploy is off, when it was turned off by the safety stop rather than
	// by a human (ADR-0052 §5): "disabled by rollback" or "disabled by downgrade". Empty when the
	// level was human-set or is not off.
	DisabledReason string `json:"disabled_reason,omitempty"`
}

// AutoDeployStatus returns the enriched, read-only auto-deploy view for app in env (ADR-0052
// §2/§3). It reads the level exactly as AutoDeploy does, then — when the registry seam is wired —
// lists the tags of the app's current image and computes what auto-deploy would take within the
// level and any higher upgrade above its cap. The registry listing is anonymous in this phase
// (RegistryAuth{}): public GHCR (the reference registry), public Docker Hub, DO, and GCR-token
// registries all list anonymously. Authenticated private-repo listing — reading the client-side
// burrow-registry pull secret — needs a deliberate burrowd RBAC grant withheld today under the
// least-privilege boundary (ADR-0017/ADR-0040); it lands with the Phase 4 poller, for which the
// seam is already ready via RegistryAuth. Any registry failure (unreachable, a private-repo 401,
// a non-semver current tag, or an absent seam) degrades to Checked=false with a Note and no error;
// only a validation or environment error returns a non-nil error.
func (e *Engine) AutoDeployStatus(ctx context.Context, app, env string) (AutoDeployStatus, error) {
	if err := (App{Name: app}).Validate(); err != nil {
		return AutoDeployStatus{}, fmt.Errorf("auto-deploy status: %w: %w", ErrInvalid, err)
	}
	if _, err := e.resolveNamespace(ctx, env); err != nil {
		return AutoDeployStatus{}, fmt.Errorf("auto-deploy status %s: %w", app, err)
	}
	level, err := e.db.AutoDeployLevel(ctx, app, envName(env))
	if err != nil {
		return AutoDeployStatus{}, fmt.Errorf("auto-deploy status %s: %w", app, err)
	}
	status := AutoDeployStatus{App: app, Env: envName(env), Level: level}

	// When the level was turned off by the safety stop (a rollback or a manual downgrade), surface
	// why (ADR-0052 §5). A store error here is a real failure, unlike the best-effort registry check.
	reason, err := e.db.AutoDeployReason(ctx, app, envName(env))
	if err != nil {
		return AutoDeployStatus{}, fmt.Errorf("auto-deploy status %s: reading disable reason: %w", app, err)
	}
	status.DisabledReason = reason

	// Find the current running release for its image reference. A missing release degrades (no
	// version to compare against); a genuine store error is a real failure and is returned.
	rel, err := e.db.LatestRelease(ctx, app, envName(env))
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			status.Note = "no running release to compare against"
			return status, nil
		}
		return AutoDeployStatus{}, fmt.Errorf("auto-deploy status %s: reading current release: %w", app, err)
	}
	status.Repository = imageRepository(rel.Image)
	status.Current = imageTag(rel.Image)

	if e.registry == nil {
		status.Note = "auto-deploy upgrade check is not wired"
		return status, nil
	}
	if stableSemver(status.Current) == "" {
		status.Note = fmt.Sprintf("current tag %q is not semver, so an upgrade cannot be computed", status.Current)
		return status, nil
	}

	// Anonymous listing in this read-only phase (ADR-0052 §7). Phase 4 supplies pull-secret creds
	// via RegistryAuth without a seam change; the adapter's basic-auth path is already ready.
	tags, err := e.registry.ListTags(ctx, rel.Image, RegistryAuth{})
	if err != nil {
		status.Note = fmt.Sprintf("registry tag listing unavailable: %v", err)
		return status, nil
	}
	status.Target, status.Upgrade = ResolveAutoDeploy(status.Current, tags, level)
	status.Checked = true
	return status, nil
}

// NextTags are the suggested next release tags after a current semver tag (ADR-0052 §8): the next
// patch, minor, and major version, each preserving the current tag's leading "v" style. They turn
// "please use semver" into concrete numbers the agent applies to its build.
type NextTags struct {
	Patch string `json:"patch"`
	Minor string `json:"minor"`
	Major string `json:"major"`
}

// NextTagResult is the read-only suggestion of an app's next semver release tags in one environment
// (ADR-0052 §8). Current is the app's running tag; Next carries the suggested patch/minor/major tags
// when that tag is stable semver. When there is no running release or the current tag is not semver,
// Next is nil and Note carries a short human reason — this degrades gracefully rather than erroring
// (ADR-0040), matching how AutoDeployStatus reports Checked=false with a Note.
type NextTagResult struct {
	App     string    `json:"app"`
	Env     string    `json:"env"`
	Current string    `json:"current,omitempty"`
	Next    *NextTags `json:"next,omitempty"`
	Note    string    `json:"note,omitempty"`
}

// NextTag suggests the next semver release tags for app in env, read from its current running tag
// (ADR-0052 §8). It reads the current tag exactly as AutoDeployStatus does (the latest release's
// image), and when that tag parses as stable semver returns the next patch, minor, and major tags.
// A missing release or a non-semver current tag degrades to a Note with no suggestion and no error,
// so the guidance is best-effort and never blocks the agent (ADR-0040). It changes nothing.
func (e *Engine) NextTag(ctx context.Context, app, env string) (NextTagResult, error) {
	if err := (App{Name: app}).Validate(); err != nil {
		return NextTagResult{}, fmt.Errorf("next tag: %w: %w", ErrInvalid, err)
	}
	if _, err := e.resolveNamespace(ctx, env); err != nil {
		return NextTagResult{}, fmt.Errorf("next tag %s: %w", app, err)
	}
	res := NextTagResult{App: app, Env: envName(env)}
	rel, err := e.db.LatestRelease(ctx, app, envName(env))
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			res.Note = "no running release yet; deploy a major.minor.patch tag to enable a suggestion"
			return res, nil
		}
		return NextTagResult{}, fmt.Errorf("next tag %s: reading current release: %w", app, err)
	}
	res.Current = imageTag(rel.Image)
	patch, minor, major, ok := nextSemverTags(res.Current)
	if !ok {
		res.Note = fmt.Sprintf("current tag %q is not semver; tag releases major.minor.patch to enable a suggestion", res.Current)
		return res, nil
	}
	res.Next = &NextTags{Patch: patch, Minor: minor, Major: major}
	return res, nil
}

// nextSemverTags computes the next patch, minor, and major release tags after a current tag, or
// ok=false when current is not a stable semver tag (ADR-0052 §8). The suggestions preserve the
// current tag's leading "v" style (a "v1.2.3" suggests "v1.2.4"; a "1.2.3" suggests "1.2.4") so they
// match the app's own convention, and they are always three-part (a "1.2" tag suggests "1.2.1").
func nextSemverTags(current string) (patch, minor, major string, ok bool) {
	canon := stableSemver(current)
	if canon == "" {
		return "", "", "", false
	}
	// canon is "vMAJOR.MINOR.PATCH": stable (no prerelease), with build metadata dropped.
	var maj, min, pat int
	if _, err := fmt.Sscanf(canon, "v%d.%d.%d", &maj, &min, &pat); err != nil {
		return "", "", "", false
	}
	prefix := ""
	if strings.HasPrefix(current, "v") {
		prefix = "v"
	}
	patch = fmt.Sprintf("%s%d.%d.%d", prefix, maj, min, pat+1)
	minor = fmt.Sprintf("%s%d.%d.0", prefix, maj, min+1)
	major = fmt.Sprintf("%s%d.0.0", prefix, maj+1)
	return patch, minor, major, true
}

// nonSemverDeployHint is the non-blocking hint attached to a deploy result when the deployed image's
// tag does not parse as stable semver (ADR-0052 §8). It nudges toward semver without gating the
// deploy: any reference still deploys (ADR-0007), it just does not get auto-update until it adopts
// semver.
const nonSemverDeployHint = "auto-update cannot classify this tag: it is not semver. Tag releases major.minor.patch (never a bare git SHA or latest) so Burrow can auto-deploy new versions of this app safely."

// imageTag extracts the tag from a pullable image reference (e.g. "1.2.3" from
// "ghcr.io/user/app:1.2.3"), or "" when the reference carries no tag. A digest reference
// (repo@sha256:...) has its digest stripped first; the tag is the last ':'-separated segment after
// the final '/', so a registry host's ":port" is never mistaken for a tag.
func imageTag(ref string) string {
	if i := strings.IndexByte(ref, '@'); i >= 0 {
		ref = ref[:i]
	}
	slash := strings.LastIndexByte(ref, '/')
	colon := strings.LastIndexByte(ref, ':')
	if colon <= slash {
		return ""
	}
	return ref[colon+1:]
}

// imageRepository returns a pullable image reference with any tag and digest stripped (e.g.
// "ghcr.io/user/app" from "ghcr.io/user/app:1.2.3"), so a caller can build a "--image <repo>:<tag>"
// deploy hint. A reference that carries no tag is returned unchanged (minus any digest).
func imageRepository(ref string) string {
	if i := strings.IndexByte(ref, '@'); i >= 0 {
		ref = ref[:i]
	}
	slash := strings.LastIndexByte(ref, '/')
	colon := strings.LastIndexByte(ref, ':')
	if colon > slash {
		return ref[:colon]
	}
	return ref
}

// The reasons recorded when the safety stop turns auto-deploy off (ADR-0052 §5), surfaced next to
// the off level in status and to the agent.
const (
	reasonDisabledByRollback  = "disabled by rollback"
	reasonDisabledByDowngrade = "disabled by downgrade"
)

// isDowngrade reports whether moving from fromTag to toTag is a strict semver downgrade. Both must
// parse as stable semver (via stableSemver); otherwise it is not a downgrade — a non-semver move is
// never treated as one (ADR-0052 §5).
func isDowngrade(fromTag, toTag string) bool {
	f, t := stableSemver(fromTag), stableSemver(toTag)
	if f == "" || t == "" {
		return false
	}
	return semver.Compare(t, f) < 0
}

// withinCap reports whether upgrading from cur to v stays within level's cap. Both cur and v
// are canonical semver (leading "v"); level is assumed not off (handled by the caller).
func withinCap(cur, v string, level AutoDeployLevel) bool {
	switch level {
	case AutoDeployPatch:
		return semver.MajorMinor(v) == semver.MajorMinor(cur)
	case AutoDeployMinor:
		return semver.Major(v) == semver.Major(cur)
	case AutoDeployMajor:
		return true
	default:
		return false
	}
}

// stableSemver normalizes an image tag to its canonical semver form (adding the leading "v" the
// golang.org/x/mod/semver package requires) and returns it, or "" if the tag is not a valid
// stable semver — that is, not semver at all, or a prerelease (which is excluded from
// auto-deploy). Build metadata is dropped by canonicalization; it does not affect comparison.
func stableSemver(tag string) string {
	v := tag
	if !strings.HasPrefix(v, "v") {
		v = "v" + v
	}
	if !semver.IsValid(v) {
		return ""
	}
	if semver.Prerelease(v) != "" {
		return ""
	}
	return semver.Canonical(v)
}

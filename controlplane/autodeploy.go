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
	// (ADR-0052 §2). Setting off is also how a version is pinned.
	AutoDeployOff AutoDeployLevel = "off"
	// AutoDeployPatch auto-deploys patches within the current minor only (e.g. 1.2.6, 1.2.7 for
	// an app on 1.2.5); crossing to a new minor is a manual step (ADR-0052 §2).
	AutoDeployPatch AutoDeployLevel = "patch"
	// AutoDeployMinor auto-deploys any patch or minor upgrade within the current major (e.g.
	// 1.2.6, 1.3.0, 1.4.0 for an app on 1.2.5), never a major (ADR-0052 §2). This is the default.
	AutoDeployMinor AutoDeployLevel = "minor"
	// AutoDeployMajor auto-deploys anything newer, including a breaking major (e.g. 2.0.0) —
	// fully hands-off updates for the operator who accepts the risk (ADR-0052 §2).
	AutoDeployMajor AutoDeployLevel = "major"
)

// DefaultAutoDeployLevel is the level that applies to every app, new or already deployed, until
// an operator changes it: minor, so an app auto-takes patches and minors within its major but not
// a breaking major (ADR-0052 §2). Auto-deploy is on by default; set an app to off to disable it.
const DefaultAutoDeployLevel = AutoDeployMinor

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

	// Find the current running release for its image reference. A missing release degrades (no
	// version to compare against); a genuine store error is a real failure and is returned.
	rel, err := e.db.LatestRelease(ctx, app)
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

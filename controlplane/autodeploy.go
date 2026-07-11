// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package controlplane

import (
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

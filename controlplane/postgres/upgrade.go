// SPDX-License-Identifier: FSL-1.1-ALv2
// Copyright 2026 Nicholas Phillips

package postgres

import (
	"fmt"
	"strconv"
	"strings"
)

// parseMajorMinor extracts the major and minor numbers from a semver-ish version
// string (a leading "v" and any -prerelease/+build suffix are ignored).
func parseMajorMinor(v string) (major, minor int, err error) {
	s := strings.TrimPrefix(strings.TrimSpace(v), "v")
	if i := strings.IndexAny(s, "-+"); i >= 0 {
		s = s[:i]
	}
	parts := strings.Split(s, ".")
	if len(parts) < 2 {
		return 0, 0, fmt.Errorf("want major.minor[.patch], got %q", v)
	}
	major, err = strconv.Atoi(parts[0])
	if err != nil {
		return 0, 0, fmt.Errorf("major in %q: %w", v, err)
	}
	minor, err = strconv.Atoi(parts[1])
	if err != nil {
		return 0, 0, fmt.Errorf("minor in %q: %w", v, err)
	}
	return major, minor, nil
}

// checkUpgrade enforces the single-minor-step upgrade policy (ADR-0013): moving the
// database from version (dMaj.dMin) to a binary at (bMaj.bMin) is allowed only when it
// is the same version (a re-run) or exactly one minor step forward within the same
// major. Skips, downgrades, and cross-major moves are refused with an actionable error.
func checkUpgrade(dMaj, dMin, bMaj, bMin int) error {
	switch {
	case bMaj == dMaj && bMin == dMin:
		return nil // same version: re-running migrations is fine
	case bMaj != dMaj:
		return fmt.Errorf("postgres: refusing migration: database is at v%d.%d but this binary is v%d.%d; cross-major upgrades are not supported in place",
			dMaj, dMin, bMaj, bMin)
	case bMin < dMin:
		return fmt.Errorf("postgres: refusing migration: database is at v%d.%d but this binary is older (v%d.%d); downgrades are not supported",
			dMaj, dMin, bMaj, bMin)
	case bMin-dMin > 1:
		return fmt.Errorf("postgres: refusing migration: database is at v%d.%d but this binary is v%d.%d; upgrade one minor version at a time (install v%d.%d first)",
			dMaj, dMin, bMaj, bMin, dMaj, dMin+1)
	default:
		return nil // exactly one minor step forward
	}
}

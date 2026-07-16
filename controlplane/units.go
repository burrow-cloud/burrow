// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package controlplane

import (
	"fmt"
	"strconv"
	"strings"
)

// HumanCPU renders a milli-CPU amount in plain language — "½ a CPU", "¼ of a CPU", "a tenth of a
// CPU", "2 CPUs" — never machine units like "500m" (issue #275/#277). Sub-CPU amounts snap to the
// nearest familiar fraction (a tenth, ¼, ⅓, ½, ⅔, ¾); amounts of one CPU or more show the whole
// count with a fractional glyph (e.g. "1½ CPUs"). Amounts that do not land on a named fraction are
// prefixed "about", so an arbitrary reading (e.g. a DOKS node's ~620m of system overhead) still
// reads as "about ⅔ of a CPU". This is the plain-language convention the README uses.
func HumanCPU(millis int64) string {
	if millis <= 0 {
		return "no CPU"
	}
	whole := millis / 1000
	rem := millis % 1000
	frac, exact := nearestFraction(rem)
	if frac.roundsUp {
		whole++
		frac = cpuFraction{}
	}
	approx := ""
	if !exact {
		approx = "about "
	}
	if whole == 0 {
		if frac.phrase == "" {
			return "under a tenth of a CPU"
		}
		return approx + frac.phrase
	}
	unit := "CPUs"
	if whole == 1 && frac.glyph == "" {
		unit = "CPU"
	}
	return fmt.Sprintf("%s%d%s %s", approx, whole, frac.glyph, unit)
}

// cpuFraction is one named sub-CPU fraction: its position in milli-CPU, the standalone phrase used
// when there is no whole part ("¼ of a CPU"), and the glyph used when combined with a whole count
// ("1¼ CPUs"). roundsUp marks the sentinel at a full CPU, which rolls into the next whole.
type cpuFraction struct {
	millis   int64
	phrase   string
	glyph    string
	roundsUp bool
}

// cpuFractions are the fraction points HumanCPU snaps to, including the endpoints (0 → no fraction,
// 1000 → round up to the next whole CPU).
var cpuFractions = []cpuFraction{
	{millis: 0, phrase: "", glyph: ""},
	{millis: 100, phrase: "a tenth of a CPU", glyph: "⅒"},
	{millis: 250, phrase: "¼ of a CPU", glyph: "¼"},
	{millis: 333, phrase: "⅓ of a CPU", glyph: "⅓"},
	{millis: 500, phrase: "½ a CPU", glyph: "½"},
	{millis: 667, phrase: "⅔ of a CPU", glyph: "⅔"},
	{millis: 750, phrase: "¾ of a CPU", glyph: "¾"},
	{millis: 1000, roundsUp: true},
}

// fractionTolerance is how close (in milli-CPU) a reading must be to a named fraction to be called
// exact rather than "about".
const fractionTolerance int64 = 15

// nearestFraction returns the fraction point closest to rem (a milli-CPU remainder in [0,1000)) and
// whether rem is within fractionTolerance of it (an exact, un-prefixed reading).
func nearestFraction(rem int64) (cpuFraction, bool) {
	best := cpuFractions[0]
	bestDist := abs(rem - best.millis)
	for _, f := range cpuFractions[1:] {
		if d := abs(rem - f.millis); d < bestDist {
			best, bestDist = f, d
		}
	}
	return best, bestDist <= fractionTolerance
}

func abs(n int64) int64 {
	if n < 0 {
		return -n
	}
	return n
}

// HumanMemory renders a byte count in MB or GB (issue #275/#277). It divides by the binary MiB/GiB
// (so a 512Mi request reads as "512 MB" and 2Gi as "2 GB", matching how the manifests and README
// state these) and trims a trailing ".0". Amounts below a gigabyte show as MB; a gigabyte or more
// as GB with up to one decimal.
func HumanMemory(bytes int64) string {
	if bytes <= 0 {
		return "no memory"
	}
	const (
		mib = int64(1) << 20
		gib = int64(1) << 30
	)
	if bytes >= gib {
		return trimDecimal(float64(bytes)/float64(gib)) + " GB"
	}
	return trimDecimal(float64(bytes)/float64(mib)) + " MB"
}

// trimDecimal formats v to one decimal place and drops a trailing ".0" so whole values read cleanly
// ("512", "1.5").
func trimDecimal(v float64) string {
	s := strconv.FormatFloat(v, 'f', 1, 64)
	return strings.TrimSuffix(s, ".0")
}

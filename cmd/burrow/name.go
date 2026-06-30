// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package main

import (
	"math/rand/v2"
	"strings"
)

// friendlyName generates a memorable adjective-animal environment name (e.g. "sequestered-pirate")
// for `burrow install` when no --environment is given (ADR-0036/0037). The name is a human handle,
// not a security boundary, so ordinary CLI-layer randomness is fine; collisions are caught by
// localconfig.Add (and the user can rename). The word lists are small and deliberately neutral.
func friendlyName() string {
	adj := nameAdjectives[rand.IntN(len(nameAdjectives))]
	animal := nameAnimals[rand.IntN(len(nameAnimals))]
	return strings.ToLower(adj + "-" + animal)
}

var nameAdjectives = []string{
	"sequestered", "amber", "brisk", "cobalt", "dapper", "eager", "fabled", "gilded",
	"hardy", "ivory", "jolly", "keen", "lucid", "mellow", "nimble", "opal",
	"plucky", "quiet", "rugged", "spry", "tidy", "umber", "vivid", "witty",
	"zesty", "ample", "bold", "crisp", "deft", "swift",
}

var nameAnimals = []string{
	"pirate", "otter", "badger", "heron", "lynx", "marmot", "narwhal", "osprey",
	"panda", "quokka", "raven", "seal", "tapir", "urchin", "vole", "walrus",
	"yak", "zebra", "ferret", "gecko", "hare", "ibis", "jackal", "koala",
	"lemur", "moose", "newt", "owl", "puffin", "robin",
}

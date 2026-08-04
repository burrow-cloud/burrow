// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package kube

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strconv"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"

	"github.com/burrow-cloud/burrow/controlplane"
)

// Issue #478, pinned. Two pods run Burrow's own image — the backup shipper (ADR-0063 §7) and the
// dependency check's probe installer (ADR-0076 §4) — and both set `command` to `/burrowd`, a path
// that has never existed in a published image. burrowd is built with ko, which lays the binary down
// at /ko-app/burrowd and makes it the entrypoint. Every backup that had a durable destination and
// every deploy-time dependency check therefore died before its first instruction, on every release,
// with `exec: "/burrowd": stat /burrowd: no such file or directory`.
//
// The unit tests passed the whole time. They asserted the command was `/burrowd ship-backup` — the
// exact string that was wrong — because a test written against the code cannot notice that the code
// and the image disagree. So there are two guards, and neither is the old one:
//
//   - This file pins the SHAPE that removes the assumption: args only, no command, no absolute path
//     anywhere in the package. It costs nothing and runs in `task check` on every commit.
//   - test/e2e/install-and-deploy.sh runs the real ko-built image, in a cluster, under both
//     subcommands, and asserts the whole probe mechanism works end to end. That is the half a unit
//     test cannot do, and it runs in the `kubernetes integration (k3d)` job on every code push.

// TestBurrowdContainersRunTheImageEntrypoint asserts that every container Burrow authors from its own
// image passes ARGUMENTS and leaves the entrypoint alone.
//
// `command` replaces the image's entrypoint, so setting it means writing down where the build tool
// put the binary — a fact this repository does not own and cannot see. `args` is appended to the
// entrypoint the image already carries, so it names only the subcommand, which this repository does
// own (controlplane.ProbeInstallCommand, controlplane.ShipBackupCommand, shared with the dispatcher
// in cmd/burrowd/main.go).
func TestBurrowdContainersRunTheImageEntrypoint(t *testing.T) {
	a := New(nil, "apps").WithAddonNamespace("burrow-addons").WithShipperImage("ghcr.io/burrow-cloud/burrowd:v9.9.9")

	cases := []struct {
		name      string
		container corev1.Container
		wantFirst string
	}{
		{
			name:      "the probe installer",
			container: a.probeInitContainer(),
			wantFirst: controlplane.ProbeInstallCommand,
		},
		{
			name:      "the backup shipper",
			container: a.shipContainer(testDestination(), controlplane.BackupPath("shop", "bk1")),
			wantFirst: controlplane.ShipBackupCommand,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := tc.container
			if len(c.Command) != 0 {
				t.Errorf("command = %v, want none: overriding the entrypoint means naming a path the build owns, and the last one Burrow named was not in the image", c.Command)
			}
			if len(c.Args) == 0 || c.Args[0] != tc.wantFirst {
				t.Fatalf("args = %v, want the %q subcommand first", c.Args, tc.wantFirst)
			}
			if c.Image == "" {
				t.Error("image = \"\", want Burrow's own image")
			}
		})
	}
}

// TestNoAuthoredPodNamesAPathToBurrowd is the guard against the fix being undone by the obvious
// wrong repair.
//
// Replacing `/burrowd` with `/ko-app/burrowd` would make every test in this package pass and the
// cluster work — today, with this build tool. It re-breaks, silently and identically, the moment the
// image is built any other way, which is exactly the failure mode that cost a release. So no
// absolute path to burrowd may appear in this package's source at all: the image's entrypoint is
// the only thing allowed to know where the binary is.
//
// It scans the package's own non-test source rather than asserting on built containers, because the
// case it exists for is a THIRD call site that no test constructs yet.
func TestNoAuthoredPodNamesAPathToBurrowd(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("reading the package directory: %v", err)
	}
	fset := token.NewFileSet()
	scanned := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parsing %s: %v", name, err)
		}
		scanned++
		ast.Inspect(file, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			s, err := strconv.Unquote(lit.Value)
			if err != nil {
				return true
			}
			if strings.HasPrefix(s, "/") && strings.Contains(s, "burrowd") {
				t.Errorf("%s: the literal %q names a path to burrowd inside the image. Where the binary lives is the build tool's decision (ko puts it at /ko-app/burrowd; a Dockerfile would put it elsewhere) and this repository must not depend on it. Pass the subcommand as an ARG and let the image's entrypoint run — see burrowdcontainer.go",
					fset.Position(lit.Pos()), s)
			}
			return true
		})
	}
	if scanned == 0 {
		t.Fatal("scanned no source files, so this guard proved nothing")
	}
}

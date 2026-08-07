// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

// Package ambienttest isolates a test binary from the machine it is running on.
//
// Burrow's client-side state is ambient by design, in two layers that both end at the home
// directory: the kubeconfig resolves $KUBECONFIG else ~/.kube/config, and the local config
// resolves $BURROW_CONFIG else ~/.burrow/config, with the scoped agent credentials
// (ADR-0038) as siblings of that file. Any code that reaches one of those resolvers without
// an explicit path therefore reads whatever the person running the tests happens to have
// configured.
//
// In a test that is not merely non-determinism. On a maintainer's laptop the ambient
// kubeconfig names live production clusters, so a test that picks it up is a test that could
// act on a real one; and a test that writes through localconfig.Path with nothing set edits
// the developer's own ~/.burrow/config. Both have happened (issue #486): four tests resolved
// pinned kube contexts against the ambient kubeconfig and so failed on a configured machine
// while passing on CI's bare runners, and a `burrow install` run outside a fixture wrote a
// handle into a real ~/.burrow/config and broke the CLI it was installed from.
//
// Isolate closes the whole class for a package by moving $HOME, $KUBECONFIG and
// $BURROW_CONFIG to an empty directory for the lifetime of the test binary. Every package's
// tests then start from exactly the bare machine CI runs on, whatever the developer has set
// up, so `task check` gives the same answer in both places. A test that wants state still
// supplies it: t.Setenv over the top of the sentinel is unaffected, and that is how the
// fixtures in this repo already work.
//
// # What this does not catch
//
//   - A path a test is handed EXPLICITLY. Isolation is of the defaults only; a test that
//     passes a real kubeconfig path, or reads BURROW_TEST_KUBECONFIG (the gated integration
//     suites), reaches exactly what it names. That is deliberate — those suites exist to talk
//     to a cluster — and it is why the integration gate is its own variable rather than the
//     ambient one.
//   - Any state that is not keyed on $HOME or those two variables: a hardcoded absolute path,
//     a Docker socket, $KUBERNETES_SERVICE_HOST, a network service on localhost.
//   - A home-derived path a dependency froze into a package-level var at init, since package
//     initialization runs before TestMain. client-go does exactly this with
//     clientcmd.RecommendedHomeFile, which is why $KUBECONFIG is set here rather than unset —
//     but the same trick in another dependency would go unnoticed, and moving $HOME would not
//     move it.
//   - Code paths run outside a Go test binary. `burrow` invoked from a shell — an agent's
//     sanity check, a script in CI — has no TestMain and is isolated by nothing here.
//   - A package that does not opt in. Isolate has to be called from a TestMain, and nothing
//     forces a new package to add one; a package that resolves ambient state and has no
//     TestMain is unguarded and looks no different from one that needs no guard.
package ambienttest

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Isolate points $HOME, $KUBECONFIG and $BURROW_CONFIG at a fresh empty directory, runs the
// package's tests against it, removes the directory, and exits with the tests' status. Call it
// as the package's whole TestMain:
//
//	func TestMain(m *testing.M) { ambienttest.Isolate(m) }
//
// Any package whose tests can reach a kubeconfig or a localconfig path — directly, or through
// a command they exercise — belongs behind it.
func Isolate(m *testing.M) {
	os.Exit(isolate(m))
}

// isolate is Isolate without the exit, so the deferred cleanup of the sentinel directory
// actually runs.
func isolate(m *testing.M) int {
	dir, err := os.MkdirTemp("", "burrow-ambienttest-")
	if err != nil {
		fmt.Fprintf(os.Stderr, "ambienttest: cannot create the sentinel home: %v\n", err)
		return 1
	}
	defer func() { _ = os.RemoveAll(dir) }()

	// Freeze the Go caches at the locations they resolve to on this machine BEFORE $HOME
	// moves, so a test that shells out to the toolchain keeps its warm build and module
	// caches instead of resolving GOPATH under the sentinel home and re-downloading the
	// module graph.
	pinGoToolchainDirs()

	// Both variables are POINTED INSIDE the sentinel rather than unset, and moving $HOME is
	// not enough on its own. client-go computes clientcmd.RecommendedHomeFile — the
	// ~/.kube/config leg of the default loading rules — into a package-level var at init,
	// which runs before TestMain does, so by the time this function moves $HOME that path is
	// already frozen at the developer's real home and no later redirect can reach it. An
	// explicit $KUBECONFIG is evaluated on each call and takes precedence over the frozen
	// one, so it is the only lever that actually holds.
	//
	// Neither file is created. A named-but-absent kubeconfig is skipped by the loading rules
	// exactly as an absent ~/.kube/config is, so the result is the empty configuration a CI
	// runner gets — while a test that WRITES through one of these paths (`burrow install`
	// recording a handle) lands in the sentinel instead of the developer's real config, and
	// goes away with it.
	for k, v := range map[string]string{
		// HOME is what os.UserHomeDir and client-go's homedir read on unix; USERPROFILE is
		// the Windows spelling of the same thing.
		"HOME":          dir,
		"USERPROFILE":   dir,
		"KUBECONFIG":    filepath.Join(dir, ".kube", "config"),
		"BURROW_CONFIG": filepath.Join(dir, ".burrow", "config"),
	} {
		if err := os.Setenv(k, v); err != nil {
			fmt.Fprintf(os.Stderr, "ambienttest: cannot set $%s: %v\n", k, err)
			return 1
		}
	}

	// Fail the binary rather than run unisolated. If the home directory still resolves
	// somewhere else, every test in the package is reading the developer's machine, and a
	// silent pass is the outcome this package exists to prevent.
	home, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintf(os.Stderr, "ambienttest: cannot resolve the home directory after redirecting it: %v\n", err)
		return 1
	}
	if home != dir {
		fmt.Fprintf(os.Stderr, "ambienttest: home resolves to %s, not the sentinel %s; these tests would read the real machine\n", home, dir)
		return 1
	}

	return m.Run()
}

// pinGoToolchainDirs records where GOPATH and the Go build and module caches resolve to right
// now, as explicit environment variables, so they survive the move of $HOME.
//
// Only cmd/burrow-agent currently shells out to the toolchain (its structural-reduction test
// runs `go list -deps`), but the cost is one process per test binary and it makes Isolate safe
// to add to a package without first auditing what its tests exec.
//
// It is best effort: with no `go` on PATH there is nothing to ask, and the worst case of
// guessing wrong is a cold cache, never a wrong answer.
func pinGoToolchainDirs() {
	var want []string
	for _, v := range []string{"GOPATH", "GOCACHE", "GOMODCACHE"} {
		if os.Getenv(v) == "" {
			want = append(want, v)
		}
	}
	if len(want) == 0 {
		return
	}
	if _, err := exec.LookPath("go"); err != nil {
		return
	}
	out, err := exec.Command("go", append([]string{"env"}, want...)...).Output()
	if err != nil {
		return
	}
	got := strings.Split(strings.TrimRight(string(out), "\n"), "\n")
	if len(got) != len(want) {
		return
	}
	for i, v := range want {
		if p := strings.TrimSpace(got[i]); filepath.IsAbs(p) {
			_ = os.Setenv(v, p)
		}
	}
}

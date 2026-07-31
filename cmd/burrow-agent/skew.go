// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/burrow-cloud/burrow/client"
)

// When the ADR-0039 skew gate refuses this binary, the control plane can only give a generic remedy:
// it knows the version that called, but not how that caller was installed. This file closes that gap
// on the side that can. burrow-agent knows which binary it is and where it lives, so it replaces the
// server's message with the ONE command that updates the binary now running.
//
// This is the half of issue #308 that the wording alone cannot fix. `burrow` and `burrow-agent` are
// two binaries (ADR-0049 §1) that Homebrew installs together but that drift apart the moment either
// is installed another way. A user who has already run `brew upgrade burrow` and is told, again, to
// run `brew upgrade burrow` concludes the tool is broken. Naming the installed path and the matching
// command turns that dead end into one step.
//
// burrow-agent does NOT update itself. Distribution is Homebrew and signed release archives, with the
// CLI as the unit a user installs (ADR-0016), and burrow-agent is deliberately capability-reduced —
// rewriting a binary on PATH is exactly the kind of privileged act it is built to lack (ADR-0049 §2a).
// So it refuses clearly and instructs exactly, which is the strongest thing it may do.

// agentModulePath is the go-installable package path of this binary, used in the source remedy.
const agentModulePath = "github.com/burrow-cloud/burrow/cmd/burrow-agent"

// installKind is how the running burrow-agent binary got onto disk, inferred from its resolved path.
// Each kind has a different update command, which is the whole point of distinguishing them.
type installKind int

const (
	// installUnknown covers a release archive unpacked by hand, a distro package, a container image,
	// or anything else the path does not identify. Its remedy names every option rather than guessing.
	installUnknown installKind = iota
	// installHomebrew is a `brew install burrow-cloud/tap/burrow`. The formula installs both binaries,
	// so `brew upgrade burrow` really does update burrow-agent.
	installHomebrew
	// installGoBin is a `go install` into GOBIN/GOPATH/bin. Homebrew will never update it, and saying
	// otherwise is the misdirection this change exists to remove.
	installGoBin
)

// classifyInstall infers how the binary at execPath was installed. execPath is expected to be
// symlink-resolved by the caller: Homebrew puts the real file under a Cellar directory and links it
// into its bin, so the Cellar segment is only visible after resolution. gobin, gopath, and home are
// passed in (rather than read here) so the whole inference is pure and unit-testable.
func classifyInstall(execPath, gobin, gopath, home string) installKind {
	if execPath == "" {
		return installUnknown
	}
	// A path element literally named "Cellar" is Homebrew's install root on both the Apple-silicon
	// (/opt/homebrew) and Intel (/usr/local) prefixes, and Linuxbrew's (~/.linuxbrew) too.
	for _, seg := range strings.Split(filepath.ToSlash(execPath), "/") {
		if seg == "Cellar" {
			return installHomebrew
		}
	}
	dir := filepath.Dir(execPath)
	for _, bin := range goBinDirs(gobin, gopath, home) {
		if bin != "" && filepath.Clean(bin) == dir {
			return installGoBin
		}
	}
	return installUnknown
}

// goBinDirs lists the directories `go install` writes to, in the order the go command resolves them:
// GOBIN if set, else each GOPATH entry's bin, else the default GOPATH ($HOME/go) bin.
func goBinDirs(gobin, gopath, home string) []string {
	if gobin != "" {
		return []string{gobin}
	}
	if gopath != "" {
		var dirs []string
		for _, p := range filepath.SplitList(gopath) {
			if p != "" {
				dirs = append(dirs, filepath.Join(p, "bin"))
			}
		}
		return dirs
	}
	if home != "" {
		return []string{filepath.Join(home, "go", "bin")}
	}
	return nil
}

// tooOldRemedy is the sentence naming the command that updates the binary at execPath to
// serverVersion. An empty serverVersion (a control plane that predates the server_version field)
// degrades to "@latest" rather than printing an empty tag.
func tooOldRemedy(kind installKind, serverVersion string) string {
	ref := "latest"
	if serverVersion != "" {
		ref = serverVersion
	}
	goInstall := fmt.Sprintf("`go install %s@%s`", agentModulePath, ref)
	switch kind {
	case installHomebrew:
		return "run `brew upgrade burrow` to update it (the formula installs burrow and burrow-agent together)"
	case installGoBin:
		return fmt.Sprintf("it was installed from source, so `brew upgrade burrow` will not touch it: run %s", goInstall)
	default:
		return fmt.Sprintf("update THIS binary, not the `burrow` CLI: they are separate binaries and upgrading the CLI on its own does not replace it. From Homebrew, `brew upgrade burrow` updates both; from source, %s; otherwise replace it from the %s release archive", goInstall, ref)
	}
}

// tooOldMessage is the whole replacement message: which binary, which version, where it is installed,
// the command that fixes it, and the session restart. The restart is not decoration — a running agent
// session keeps executing the binary it launched with, so updating the file is only half the fix.
func tooOldMessage(execPath, clientVersion, serverVersion string, kind installKind) string {
	where := ""
	if execPath != "" {
		where = fmt.Sprintf(" at %s", execPath)
	}
	against := "this control plane"
	if serverVersion != "" {
		against = fmt.Sprintf("this control plane (%s)", serverVersion)
	}
	return fmt.Sprintf("your burrow-agent (%s)%s is too old for %s; %s. Then restart your agent session so it runs the new binary.",
		clientVersion, where, against, tooOldRemedy(kind, serverVersion))
}

// resolvedExecutable returns the symlink-resolved path of the running binary, or "" if it cannot be
// determined. It is a package var so tests can drive the message end to end without depending on
// where the test binary happens to live.
var resolvedExecutable = func() string {
	p, err := os.Executable()
	if err != nil {
		return ""
	}
	if resolved, err := filepath.EvalSymlinks(p); err == nil {
		return resolved
	}
	return p
}

// retargetTooOld rewrites a client_too_old refusal (ADR-0039) into a remedy for the binary that was
// actually refused, and returns every other error untouched. It is applied at both places an error
// leaves this binary — the mutating verbs' outcome envelope and main's stderr line — so the agent
// reads the same actionable text whichever verb it ran.
func retargetTooOld(err error) error {
	var api *client.APIError
	if !errors.As(err, &api) || api.Code != client.CodeClientTooOld {
		return err
	}
	execPath := resolvedExecutable()
	home, _ := os.UserHomeDir()
	kind := classifyInstall(execPath, os.Getenv("GOBIN"), os.Getenv("GOPATH"), home)
	retargeted := *api
	retargeted.Message = tooOldMessage(execPath, agentVersion(), api.ServerVersion, kind)
	return &retargeted
}

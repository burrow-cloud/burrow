// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package main

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/mod/module"
	"golang.org/x/mod/semver"

	"github.com/burrow-cloud/burrow/client"
)

// TestAgentVersionDoesNotClaimAStaleRelease is the root-cause pin for issue #308.
//
// burrow-agent used to default its version var to the literal "v0.1.0". That is a VALID RELEASE TAG,
// so the ADR-0039 skew gate read an unstamped source build as an ancient release and hard-refused
// every call — "can't do much of anything" — while the `burrow` CLI, whose default is the invalid
// "dev", was served. The gate deliberately exempts a locally built client because there is nothing
// for it to upgrade to; the agent binary was excluded from that exemption by a default that lied
// about what it was.
//
// The assertion is the PROPERTY, not the literal: whatever an unstamped burrow-agent reports must be
// something the gate exempts (not a plain release tag), so no future default can reintroduce this.
func TestAgentVersionDoesNotClaimAStaleRelease(t *testing.T) {
	got := agentVersion()
	if semver.IsValid(got) && !module.IsPseudoVersion(got) {
		t.Errorf("agentVersion() = %q for an unstamped build: a plain release tag makes the ADR-0039 "+
			"skew gate refuse a source build as an ancient release. It must report something the gate "+
			"exempts (\"dev\" or a Go pseudo-version), like the burrow CLI's cliVersion().", got)
	}
	if got != "dev" {
		t.Errorf("agentVersion() = %q, want dev for an unversioned test binary (mirrors cliVersion())", got)
	}
}

// TestAgentReportsItsVersion confirms `burrow-agent --version` reports the handshake version as JSON.
// It is how a stranded agent, and `burrow version`, read this binary's version without a
// control-plane call. It is a FLAG rather than a subcommand on purpose: the agent command surface is
// a closed allow-list (agent_surface_guard_test.go) and self-reporting is not a capability.
func TestAgentReportsItsVersion(t *testing.T) {
	var out, errb strings.Builder
	if err := run(t.Context(), []string{"--version"}, &out, &errb); err != nil {
		t.Fatalf("--version: %v\n%s", err, errb.String())
	}
	var body struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal([]byte(out.String()), &body); err != nil {
		t.Fatalf("--version output %q is not JSON: %v", out.String(), err)
	}
	if body.Version != agentVersion() {
		t.Errorf("--version reported %q, want %q (the version sent in the handshake)", body.Version, agentVersion())
	}
}

func TestClassifyInstall(t *testing.T) {
	home := filepath.FromSlash("/Users/dev")
	cases := []struct {
		name                string
		execPath            string
		gobin, gopath, home string
		want                installKind
	}{
		{
			name:     "Homebrew on Apple silicon resolves into the Cellar",
			execPath: filepath.FromSlash("/opt/homebrew/Cellar/burrow/0.13.0/bin/burrow-agent"),
			home:     home,
			want:     installHomebrew,
		},
		{
			name:     "Homebrew on Intel resolves into the Cellar too",
			execPath: filepath.FromSlash("/usr/local/Cellar/burrow/0.13.0/bin/burrow-agent"),
			home:     home,
			want:     installHomebrew,
		},
		{
			name:     "Linuxbrew is the same shape under a different prefix",
			execPath: filepath.FromSlash("/home/dev/.linuxbrew/Cellar/burrow/0.13.0/bin/burrow-agent"),
			home:     home,
			want:     installHomebrew,
		},
		{
			name:     "GOBIN wins when it is set",
			execPath: filepath.FromSlash("/Users/dev/bin/burrow-agent"),
			gobin:    filepath.FromSlash("/Users/dev/bin"),
			gopath:   filepath.FromSlash("/Users/dev/go"),
			home:     home,
			want:     installGoBin,
		},
		{
			name:     "GOPATH/bin when GOBIN is unset",
			execPath: filepath.FromSlash("/Users/dev/go/bin/burrow-agent"),
			gopath:   filepath.FromSlash("/Users/dev/go"),
			home:     home,
			want:     installGoBin,
		},
		{
			name:     "the default GOPATH ($HOME/go) when neither is set",
			execPath: filepath.FromSlash("/Users/dev/go/bin/burrow-agent"),
			home:     home,
			want:     installGoBin,
		},
		{
			name:     "an unpacked release archive is neither, and must not be guessed at",
			execPath: filepath.FromSlash("/usr/local/bin/burrow-agent"),
			home:     home,
			want:     installUnknown,
		},
		{
			name: "an unknown executable path stays unknown",
			home: home,
			want: installUnknown,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyInstall(tc.execPath, tc.gobin, tc.gopath, tc.home); got != tc.want {
				t.Errorf("classifyInstall(%q) = %v, want %v", tc.execPath, got, tc.want)
			}
		})
	}
}

// TestTooOldMessageNamesTheCommandThatFixesThisBinary is the client half of the issue #308 pin. The
// control plane can only give a generic remedy because it cannot know how the caller was installed;
// burrow-agent can, so its message must name ONE command that works for the binary now running —
// and, for a source install, must say outright that Homebrew will not help, because "run `brew
// upgrade burrow`" is precisely the advice the reporter had already followed.
func TestTooOldMessageNamesTheCommandThatFixesThisBinary(t *testing.T) {
	cases := []struct {
		name     string
		kind     installKind
		execPath string
		server   string
		want     []string
		notWant  []string
	}{
		{
			name:     "Homebrew: one command, and it really does update both binaries",
			kind:     installHomebrew,
			execPath: "/opt/homebrew/bin/burrow-agent",
			server:   "v0.13.0",
			want: []string{
				"your burrow-agent (v0.11.0)",
				"at /opt/homebrew/bin/burrow-agent",
				"this control plane (v0.13.0)",
				"brew upgrade burrow",
				"restart your agent session",
			},
			notWant: []string{"go install"},
		},
		{
			name:     "source install: say Homebrew will NOT fix it, and give the command that does",
			kind:     installGoBin,
			execPath: "/Users/dev/go/bin/burrow-agent",
			server:   "v0.13.0",
			want: []string{
				"`brew upgrade burrow` will not touch it",
				"go install github.com/burrow-cloud/burrow/cmd/burrow-agent@v0.13.0",
				"restart your agent session",
			},
		},
		{
			name:     "unknown install: name every option rather than guess at one",
			kind:     installUnknown,
			execPath: "/usr/local/bin/burrow-agent",
			server:   "v0.13.0",
			want: []string{
				"not the `burrow` CLI: they are separate binaries",
				"brew upgrade burrow",
				"go install github.com/burrow-cloud/burrow/cmd/burrow-agent@v0.13.0",
				"release archive",
			},
		},
		{
			name:     "a control plane too old to report its version degrades to @latest",
			kind:     installGoBin,
			execPath: "/Users/dev/go/bin/burrow-agent",
			server:   "",
			want:     []string{"go install github.com/burrow-cloud/burrow/cmd/burrow-agent@latest"},
			notWant:  []string{"@)", "()"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tooOldMessage(tc.execPath, "v0.11.0", tc.server, tc.kind)
			for _, want := range tc.want {
				if !strings.Contains(got, want) {
					t.Errorf("message %q\n  want substring %q", got, want)
				}
			}
			for _, no := range tc.notWant {
				if strings.Contains(got, no) {
					t.Errorf("message %q\n  should not contain %q", got, no)
				}
			}
		})
	}
}

// TestRetargetTooOldReplacesTheServerRemedy confirms the swap happens on a client_too_old refusal and
// ONLY on it: every other error, including another APIError, is passed through untouched.
func TestRetargetTooOldReplacesTheServerRemedy(t *testing.T) {
	orig := resolvedExecutable
	resolvedExecutable = func() string { return filepath.FromSlash("/opt/homebrew/Cellar/burrow/0.13.0/bin/burrow-agent") }
	t.Cleanup(func() { resolvedExecutable = orig })

	serverSaid := "your burrow client (v0.11.0) is too old for this control plane (v0.13.0); run `brew upgrade burrow` (or reinstall the CLI) to update it"
	tooOld := &client.APIError{StatusCode: 426, Code: client.CodeClientTooOld, Message: serverSaid, ServerVersion: "v0.13.0"}

	var got *client.APIError
	if !errors.As(retargetTooOld(tooOld), &got) {
		t.Fatal("retargetTooOld dropped the APIError type")
	}
	if got.Message == serverSaid {
		t.Error("retargetTooOld left the control plane's generic remedy in place")
	}
	if !strings.Contains(got.Message, "burrow-agent") || !strings.Contains(got.Message, "Cellar") {
		t.Errorf("message %q, want it to name this binary and where it is installed", got.Message)
	}
	if got.Code != client.CodeClientTooOld || got.StatusCode != 426 {
		t.Errorf("retargetTooOld changed the machine-readable fields: %+v", got)
	}
	// The original must not be mutated: the caller may still hold it.
	if tooOld.Message != serverSaid {
		t.Error("retargetTooOld mutated the error it was given")
	}

	other := &client.APIError{StatusCode: 404, Code: "not_found", Message: "no such app"}
	if retargetTooOld(other) != error(other) {
		t.Error("retargetTooOld rewrote an unrelated APIError")
	}
	plain := errors.New("dial tcp: connection refused")
	if retargetTooOld(plain) != plain {
		t.Error("retargetTooOld rewrote a plain error")
	}
}

// TestTooOldSurfacesThroughTheOutcomeEnvelope confirms a mutating verb's JSON envelope — the only
// thing an agent reads on a failed deploy — carries the retargeted remedy, not the server's.
func TestTooOldSurfacesThroughTheOutcomeEnvelope(t *testing.T) {
	orig := resolvedExecutable
	resolvedExecutable = func() string { return filepath.FromSlash("/Users/dev/go/bin/burrow-agent") }
	t.Cleanup(func() { resolvedExecutable = orig })
	t.Setenv("GOBIN", filepath.FromSlash("/Users/dev/go/bin"))

	oc := classify("app.deploy", &client.APIError{
		StatusCode:    426,
		Code:          client.CodeClientTooOld,
		Message:       "your burrow client (v0.11.0) is too old for this control plane (v0.13.0); run `brew upgrade burrow` (or reinstall the CLI) to update it",
		ServerVersion: "v0.13.0",
	})
	if oc.Outcome != outcomeError {
		t.Fatalf("outcome = %q, want %q", oc.Outcome, outcomeError)
	}
	if !strings.Contains(oc.Message, "go install github.com/burrow-cloud/burrow/cmd/burrow-agent@v0.13.0") {
		t.Errorf("envelope message %q, want the source-install remedy for this binary", oc.Message)
	}
}

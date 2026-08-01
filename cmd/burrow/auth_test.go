// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package main

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/burrow-cloud/burrow/connect"
	"github.com/burrow-cloud/burrow/localconfig"
)

// authFixture is the isolated world an auth test runs in: a $BURROW_CONFIG in a temp dir, a fake
// home directory the coding-agent detection is rooted at, and a substituted kubeconfig context list.
// Nothing here reads or writes the real ~/.burrow, ~/.claude, or ~/.kube.
type authFixture struct {
	configPath string
	home       string
}

// stubAuth isolates every seam an auth command touches and returns the fixture. contexts is what the
// kubeconfig appears to hold; terminal drives the interactive/non-interactive branch.
func stubAuth(t *testing.T, contexts []connect.Context, terminal bool) *authFixture {
	t.Helper()
	f := &authFixture{
		configPath: filepath.Join(t.TempDir(), "config"),
		home:       t.TempDir(),
	}
	t.Setenv("BURROW_CONFIG", f.configPath)

	origList := listContexts
	listContexts = func(string) ([]connect.Context, error) { return contexts, nil }

	origTerm := stdinIsTerminal
	stdinIsTerminal = func(io.Reader) bool { return terminal }

	origHome := agentHomeDir
	agentHomeDir = func() (string, error) { return f.home, nil }

	origSettings := claudeSettingsPath
	claudeSettingsPath = func() (string, error) { return filepath.Join(f.home, ".claude", "settings.json"), nil }

	origMemory := claudeMemoryPath
	claudeMemoryPath = func() (string, error) { return filepath.Join(f.home, ".claude", "CLAUDE.md"), nil }

	t.Cleanup(func() {
		listContexts = origList
		stdinIsTerminal = origTerm
		agentHomeDir = origHome
		claudeSettingsPath = origSettings
		claudeMemoryPath = origMemory
	})
	return f
}

// installClaudeConfigDir creates the fake home's ~/.claude directory, so detection finds Claude Code
// the way it finds a real one: by its config directory, never by a name on $PATH.
func (f *authFixture) installClaudeConfigDir(t *testing.T) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(f.home, ".claude"), 0o700); err != nil {
		t.Fatalf("creating the fake ~/.claude: %v", err)
	}
}

// loadAuthConfig reads back the config the command wrote.
func loadAuthConfig(t *testing.T) *localconfig.Config {
	t.Helper()
	cfg, err := localconfig.Load()
	if err != nil {
		t.Fatalf("loading the config the command wrote: %v", err)
	}
	return cfg
}

func authContexts() []connect.Context {
	return []connect.Context{
		{Name: "kind-dev", Cluster: "kind"},
		{Name: "do-nyc1-cluster", Cluster: "do-cluster", Current: true},
	}
}

// TestAuthLoginReturnReachesTheManagedProduct confirms ADR-0078 §2's default: pressing return at the
// first prompt selects burrow-cloud.dev, and a person with no cluster is never shown a Kubernetes
// concept on the way there. Signing in is cloud ADR-0028 and does not exist in this build, so the
// selection lands on the seam and says so truthfully rather than pretending (ADR-0009).
func TestAuthLoginReturnReachesTheManagedProduct(t *testing.T) {
	stubAuth(t, authContexts(), true)

	var out bytes.Buffer
	err := runAuthLogin(context.Background(), authLoginOpts{}, strings.NewReader("\n"), &out)
	if err == nil {
		t.Fatal("want the cloud sign-in seam's truthful stop, got a success")
	}
	if !strings.Contains(err.Error(), localconfig.CloudEndpoint) {
		t.Errorf("error = %q, want it to name the managed product", err)
	}
	if !strings.Contains(err.Error(), "not available in this build") {
		t.Errorf("error = %q, want it to say plainly that sign-in is not built here", err)
	}

	// The prompt itself is the ADR's: the managed product first and default, Other second, and no
	// Kubernetes vocabulary before the answer.
	prompt := out.String()
	if !strings.Contains(prompt, "Where do you use Burrow?") {
		t.Errorf("prompt = %q, want ADR-0078 §2's question", prompt)
	}
	before, _, _ := strings.Cut(prompt, "Select [1]")
	if strings.Contains(before, "kubeconfig") || strings.Contains(before, "context") {
		t.Errorf("the first prompt shows a Kubernetes concept before any answer:\n%s", before)
	}
	if idx := strings.Index(prompt, localconfig.CloudEndpoint); idx < 0 || idx > strings.Index(prompt, "Other") {
		t.Errorf("the managed product is not the first entry:\n%s", prompt)
	}

	// A failed sign-in records nothing: a target nothing can reach is worse than no target.
	if cfg := loadAuthConfig(t); len(cfg.Targets) != 0 {
		t.Errorf("targets = %v, want none recorded when sign-in did not happen", cfg.Targets)
	}
}

// TestAuthLoginOtherPicksAKubeContext confirms choosing Other lists the contexts already in the
// kubeconfig and records the chosen one as the active target, storing only its NAME.
func TestAuthLoginOtherPicksAKubeContext(t *testing.T) {
	stubAuth(t, authContexts(), true)

	var out bytes.Buffer
	// "2" selects Other; "1" selects the first context; "n" declines the agent-wiring offer.
	if err := runAuthLogin(context.Background(), authLoginOpts{}, strings.NewReader("2\n1\nn\n"), &out); err != nil {
		t.Fatalf("runAuthLogin: %v", err)
	}

	cfg := loadAuthConfig(t)
	if cfg.CurrentTarget != "kind-dev" {
		t.Fatalf("active target = %q, want kind-dev", cfg.CurrentTarget)
	}
	tgt, ok := cfg.LookupTarget("kind-dev")
	if !ok {
		t.Fatal("the chosen target was not recorded")
	}
	if tgt.Kind != localconfig.TargetKindKubernetes || tgt.Context != "kind-dev" {
		t.Errorf("target = %+v, want a kubernetes target naming the context", tgt)
	}
	if !strings.Contains(out.String(), "do-nyc1-cluster") {
		t.Errorf("the context list did not offer every context:\n%s", out.String())
	}
}

// TestAuthLoginOtherWithNoKubeconfigStops confirms ADR-0078 §2's stop: no kubeconfig means the CLI
// says exactly that and stops. Not a prompt for a server URL, not a degraded mode.
func TestAuthLoginOtherWithNoKubeconfigStops(t *testing.T) {
	stubAuth(t, nil, true)

	var out bytes.Buffer
	err := runAuthLogin(context.Background(), authLoginOpts{}, strings.NewReader("2\n"), &out)
	if err == nil {
		t.Fatal("want a stop when there is no kubeconfig")
	}
	msg := err.Error()
	if !strings.Contains(msg, "no kubeconfig was found") {
		t.Errorf("error = %q, want it to name what is missing", msg)
	}
	if !strings.Contains(msg, "Burrow Cloud needs none of that") {
		t.Errorf("error = %q, want it to say the managed product needs none of it", msg)
	}
	for _, forbidden := range []string{"server URL", "https://"} {
		if strings.Contains(msg, forbidden) {
			t.Errorf("error = %q, want no prompt for %q", msg, forbidden)
		}
	}
	if cfg := loadAuthConfig(t); cfg.CurrentTarget != "" {
		t.Errorf("active target = %q, want nothing recorded", cfg.CurrentTarget)
	}
}

// TestAuthLoginContextFlagIsNonInteractive confirms --context selects a target with no prompt at
// all, which is what makes login scriptable and what a non-terminal run is told to use.
func TestAuthLoginContextFlagIsNonInteractive(t *testing.T) {
	stubAuth(t, authContexts(), false)

	var out bytes.Buffer
	err := runAuthLogin(context.Background(), authLoginOpts{kubeContext: "do-nyc1-cluster"}, strings.NewReader(""), &out)
	if err != nil {
		t.Fatalf("runAuthLogin: %v", err)
	}
	if got := loadAuthConfig(t).CurrentTarget; got != "do-nyc1-cluster" {
		t.Errorf("active target = %q, want do-nyc1-cluster", got)
	}
	if strings.Contains(out.String(), "Where do you use Burrow?") {
		t.Errorf("--context prompted anyway:\n%s", out.String())
	}
}

// TestAuthLoginContextFlagRejectsAnUnknownContext confirms a typo is caught at login, naming the
// contexts that do exist, rather than surfacing at the next command that tries to connect.
func TestAuthLoginContextFlagRejectsAnUnknownContext(t *testing.T) {
	stubAuth(t, authContexts(), false)

	var out bytes.Buffer
	err := runAuthLogin(context.Background(), authLoginOpts{kubeContext: "typo"}, strings.NewReader(""), &out)
	if err == nil {
		t.Fatal("want an error for a context that is not in the kubeconfig")
	}
	if !strings.Contains(err.Error(), "kind-dev") {
		t.Errorf("error = %q, want it to list the available contexts", err)
	}
}

// TestAuthLoginNonInteractiveNeedsAFlag confirms a run with no terminal and no selection flag stops
// with the two commands that select without prompting, instead of hanging on a prompt nobody can
// answer.
func TestAuthLoginNonInteractiveNeedsAFlag(t *testing.T) {
	stubAuth(t, authContexts(), false)

	var out bytes.Buffer
	err := runAuthLogin(context.Background(), authLoginOpts{}, strings.NewReader(""), &out)
	if err == nil {
		t.Fatal("want an error with no terminal and no selection flag")
	}
	for _, want := range []string{"--cloud", "--context"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want it to name %s", err, want)
		}
	}
}

// TestAuthLoginInstallsNothing confirms ADR-0078 §3: authenticating is not installing. The second
// person to use a cluster brings their own context and runs no cluster-admin operation, so login
// must not reach a cluster at all.
func TestAuthLoginInstallsNothing(t *testing.T) {
	stubAuth(t, authContexts(), true)

	applied := false
	origApply := applyFn
	applyFn = func(context.Context, string, string, string, bool, io.Writer, io.Writer) error {
		applied = true
		return nil
	}
	t.Cleanup(func() { applyFn = origApply })

	var out bytes.Buffer
	if err := runAuthLogin(context.Background(), authLoginOpts{}, strings.NewReader("2\n1\nn\n"), &out); err != nil {
		t.Fatalf("runAuthLogin: %v", err)
	}
	if applied {
		t.Error("login applied manifests to a cluster; authenticating is not installing")
	}
	if strings.Contains(out.String(), "install") && !strings.Contains(out.String(), "burrow agent claude install") {
		t.Errorf("login output talks about installing:\n%s", out.String())
	}
}

// TestAuthLoginSelfHostedMakesNoCloudCall confirms nothing about choosing Other is second-class: the
// self-hosted path never invokes the managed product's sign-in seam, which is the only thing in this
// CLI that could reach it.
func TestAuthLoginSelfHostedMakesNoCloudCall(t *testing.T) {
	stubAuth(t, authContexts(), true)

	called := false
	origSignIn := cloudSignInFn
	cloudSignInFn = func(context.Context, io.Writer) (localconfig.Target, error) {
		called = true
		return localconfig.CloudTarget(), nil
	}
	t.Cleanup(func() { cloudSignInFn = origSignIn })

	var out bytes.Buffer
	if err := runAuthLogin(context.Background(), authLoginOpts{}, strings.NewReader("2\n1\nn\n"), &out); err != nil {
		t.Fatalf("runAuthLogin: %v", err)
	}
	if called {
		t.Error("choosing Other contacted the managed product; the self-hosted path must need no account and no call")
	}
}

// TestAuthStatusReportsTheActiveTarget confirms status lists what is configured, marks the active
// one, and says what each target is.
func TestAuthStatusReportsTheActiveTarget(t *testing.T) {
	stubAuth(t, authContexts(), false)
	cfg := &localconfig.Config{}
	_ = cfg.SetTarget(localconfig.KubernetesTarget("kind-dev"))
	_ = cfg.SetTarget(localconfig.KubernetesTarget("do-nyc1-cluster"))
	if err := cfg.Save(); err != nil {
		t.Fatalf("save: %v", err)
	}

	var out bytes.Buffer
	if err := runAuthStatus(&out, authStatusOpts{}); err != nil {
		t.Fatalf("runAuthStatus: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "kind-dev") || !strings.Contains(got, "do-nyc1-cluster") {
		t.Errorf("status does not list both targets:\n%s", got)
	}
	if !strings.Contains(got, "* the active target") {
		t.Errorf("status does not explain the active marker:\n%s", got)
	}
	for _, line := range strings.Split(got, "\n") {
		if !strings.Contains(line, "kubernetes") { // table rows only, not the legend
			continue
		}
		marked := strings.HasPrefix(line, "*")
		if want := strings.Contains(line, "do-nyc1-cluster"); marked != want {
			t.Errorf("active marker is wrong on %q (marked=%v, want=%v)", line, marked, want)
		}
	}
}

// TestAuthStatusFlagsAMovedKubeconfig confirms a target whose context is no longer in the kubeconfig
// is reported legibly, which is the failure mode of keeping local state at all.
func TestAuthStatusFlagsAMovedKubeconfig(t *testing.T) {
	stubAuth(t, authContexts(), false)
	cfg := &localconfig.Config{}
	_ = cfg.SetTarget(localconfig.KubernetesTarget("a-cluster-that-moved"))
	if err := cfg.Save(); err != nil {
		t.Fatalf("save: %v", err)
	}

	var out bytes.Buffer
	if err := runAuthStatus(&out, authStatusOpts{}); err != nil {
		t.Fatalf("runAuthStatus: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "not in your kubeconfig") {
		t.Errorf("status does not flag the stale target:\n%s", got)
	}
	if !strings.Contains(got, "burrow auth login") {
		t.Errorf("status does not name the way out:\n%s", got)
	}
}

// TestAuthStatusEmptySaysWhatHappensToday confirms a person who has never logged in is told what
// their commands do now, and how to choose.
func TestAuthStatusEmptySaysWhatHappensToday(t *testing.T) {
	stubAuth(t, authContexts(), false)

	var out bytes.Buffer
	if err := runAuthStatus(&out, authStatusOpts{}); err != nil {
		t.Fatalf("runAuthStatus: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "No target is configured") {
		t.Errorf("status = %q, want it to say no target is configured", got)
	}
	if !strings.Contains(got, "burrow auth login") {
		t.Errorf("status = %q, want it to name how to choose one", got)
	}
}

// TestAuthSwitchChangesTheActiveTarget confirms switch changes the selection without touching the
// target list and without re-authenticating.
func TestAuthSwitchChangesTheActiveTarget(t *testing.T) {
	stubAuth(t, authContexts(), false)
	cfg := &localconfig.Config{}
	_ = cfg.SetTarget(localconfig.KubernetesTarget("kind-dev"))
	_ = cfg.SetTarget(localconfig.KubernetesTarget("do-nyc1-cluster"))
	if err := cfg.Save(); err != nil {
		t.Fatalf("save: %v", err)
	}

	var out bytes.Buffer
	if err := runAuthSwitch("kind-dev", &out); err != nil {
		t.Fatalf("runAuthSwitch: %v", err)
	}
	after := loadAuthConfig(t)
	if after.CurrentTarget != "kind-dev" {
		t.Errorf("active target = %q, want kind-dev", after.CurrentTarget)
	}
	if len(after.Targets) != 2 {
		t.Errorf("targets = %d, want the list unchanged at 2", len(after.Targets))
	}
}

// TestAuthSwitchUnknownNamesWhatIsConfigured confirms switching to a name that is not configured
// fails with the list of names that are.
func TestAuthSwitchUnknownNamesWhatIsConfigured(t *testing.T) {
	stubAuth(t, authContexts(), false)
	cfg := &localconfig.Config{}
	_ = cfg.SetTarget(localconfig.KubernetesTarget("kind-dev"))
	if err := cfg.Save(); err != nil {
		t.Fatalf("save: %v", err)
	}

	var out bytes.Buffer
	err := runAuthSwitch("nope", &out)
	if err == nil {
		t.Fatal("want an error for an unconfigured target")
	}
	if !strings.Contains(err.Error(), "kind-dev") {
		t.Errorf("error = %q, want it to list what is configured", err)
	}
}

// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package localconfig

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestKubernetesTargetStoresOnlyTheContextName is the load-bearing property of ADR-0078 §1: a
// Kubernetes target records the context NAME and nothing that could go stale. The assertion is on
// the serialized form, because that is what a rotated kubeconfig would have to disagree with.
func TestKubernetesTargetStoresOnlyTheContextName(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config")
	cfg := &Config{}
	if err := cfg.SetTarget(KubernetesTarget("do-nyc1-cluster")); err != nil {
		t.Fatalf("SetTarget: %v", err)
	}
	if err := cfg.saveTo(path); err != nil {
		t.Fatalf("save: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	got := string(data)
	if !strings.Contains(got, "context: do-nyc1-cluster") {
		t.Errorf("target file does not record the context name:\n%s", got)
	}
	// Nothing credential-shaped may be serialized: the kubeconfig stays the single source of truth.
	for _, forbidden := range []string{"token", "certificate", "client-key", "ca.crt", "server:", "password"} {
		if strings.Contains(got, forbidden) {
			t.Errorf("target file contains %q, but a target must never copy a credential:\n%s", forbidden, got)
		}
	}
}

// TestSetTargetReplacesAndActivates confirms re-authenticating against a target you already have
// updates it in place rather than duplicating it, and makes it active either way.
func TestSetTargetReplacesAndActivates(t *testing.T) {
	cfg := &Config{}
	if err := cfg.SetTarget(KubernetesTarget("dev")); err != nil {
		t.Fatalf("SetTarget dev: %v", err)
	}
	if err := cfg.SetTarget(KubernetesTarget("prod")); err != nil {
		t.Fatalf("SetTarget prod: %v", err)
	}
	if err := cfg.SetTarget(KubernetesTarget("dev")); err != nil {
		t.Fatalf("SetTarget dev again: %v", err)
	}
	if len(cfg.Targets) != 2 {
		t.Fatalf("targets = %d, want 2 (a repeat login must not duplicate)", len(cfg.Targets))
	}
	if cfg.CurrentTarget != "dev" {
		t.Errorf("active target = %q, want dev", cfg.CurrentTarget)
	}
}

// TestSwitchTargetNamesWhatIsConfigured confirms switching to an unknown target fails with a message
// that lists what is actually there, and that a known one becomes active without touching anything
// else.
func TestSwitchTargetNamesWhatIsConfigured(t *testing.T) {
	cfg := &Config{}
	_ = cfg.SetTarget(KubernetesTarget("dev"))
	_ = cfg.SetTarget(KubernetesTarget("prod"))

	err := cfg.SwitchTarget("staging")
	if err == nil {
		t.Fatal("SwitchTarget on an unknown name: want an error")
	}
	if !strings.Contains(err.Error(), "dev, prod") {
		t.Errorf("error does not name the configured targets: %v", err)
	}
	if err := cfg.SwitchTarget("dev"); err != nil {
		t.Fatalf("SwitchTarget dev: %v", err)
	}
	if cfg.CurrentTarget != "dev" {
		t.Errorf("active target = %q, want dev", cfg.CurrentTarget)
	}
	if len(cfg.Targets) != 2 {
		t.Errorf("switching changed the target list (%d entries), it must only change the selection", len(cfg.Targets))
	}
}

// TestLoadRejectsHandEditedTargets confirms a hand-edited targeting block produces a legible error
// naming the file and the problem, rather than a confusing failure in a later command (ADR-0078
// "Consequences").
func TestLoadRejectsHandEditedTargets(t *testing.T) {
	cases := []struct {
		name string
		yaml string
		want string
	}{
		{
			name: "unknown kind",
			yaml: "apiVersion: burrow.dev/v1\nkind: Config\ntargets:\n  - name: weird\n    kind: mainframe\n",
			want: "unknown kind",
		},
		{
			name: "kubernetes target with no context",
			yaml: "apiVersion: burrow.dev/v1\nkind: Config\ntargets:\n  - name: dev\n    kind: kubernetes\n",
			want: "names no kube context",
		},
		{
			name: "active target that is not in the list",
			yaml: "apiVersion: burrow.dev/v1\nkind: Config\ncurrentTarget: ghost\ntargets:\n  - name: dev\n    kind: kubernetes\n    context: dev\n",
			want: "is not in the targets list",
		},
		{
			name: "duplicate target names",
			yaml: "apiVersion: burrow.dev/v1\nkind: Config\ntargets:\n  - name: dev\n    kind: kubernetes\n    context: a\n  - name: dev\n    kind: kubernetes\n    context: b\n",
			want: "listed twice",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config")
			if err := os.WriteFile(path, []byte(tc.yaml), 0o600); err != nil {
				t.Fatalf("write: %v", err)
			}
			_, err := loadFrom(path)
			if err == nil {
				t.Fatal("loadFrom: want an error for a hand-edited target")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to mention %q", err, tc.want)
			}
			if !strings.Contains(err.Error(), path) {
				t.Errorf("error = %q, want it to name the config file %s", err, path)
			}
		})
	}
}

// TestActiveTargetAbsentIsNotAnError confirms a config with no targeting block resolves to "no
// target", which is the pre-ADR-0078 world and must stay a working one.
func TestActiveTargetAbsentIsNotAnError(t *testing.T) {
	_, ok, err := (&Config{}).ActiveTarget()
	if err != nil {
		t.Fatalf("ActiveTarget: %v", err)
	}
	if ok {
		t.Error("ActiveTarget reported a target on an empty config")
	}
}

// TestCloudTargetDescribes confirms the managed target names its endpoint and carries no context.
func TestCloudTargetDescribes(t *testing.T) {
	tgt := CloudTarget()
	if tgt.Context != "" {
		t.Errorf("cloud target carries context %q, want none", tgt.Context)
	}
	if !strings.Contains(tgt.Describe(), CloudEndpoint) {
		t.Errorf("Describe() = %q, want it to name %s", tgt.Describe(), CloudEndpoint)
	}
}

// TestCloudIdentityAndAPIHostAreDistinct pins both halves of the split. The identity is what a
// recorded target and a credential filename are keyed to and must never move; the API host is the
// console, because the apex serves the marketing website and answers every API path with a 404. The
// literals are written out rather than derived, so redefining either constant fails here rather than
// quietly agreeing with itself.
func TestCloudIdentityAndAPIHostAreDistinct(t *testing.T) {
	if CloudEndpoint != "burrow-cloud.dev" {
		t.Errorf("CloudEndpoint = %q — changing the identity strands every credential and target already on disk", CloudEndpoint)
	}
	if CloudAPIEndpoint != "console.burrow-cloud.dev" {
		t.Errorf("CloudAPIEndpoint = %q, want the console the control plane answers on", CloudAPIEndpoint)
	}
	if CloudAPIEndpoint == CloudEndpoint {
		t.Error("the API host is the apex, which serves the marketing site")
	}
	// A target still records the identity: that is the name it carries and the endpoint it stores.
	tgt := CloudTarget()
	if tgt.Name != CloudEndpoint || tgt.Endpoint != CloudEndpoint {
		t.Errorf("CloudTarget() = %+v, want both name and endpoint to be %s", tgt, CloudEndpoint)
	}
}

// TestTargetWithoutInstallIDLoads is the compatibility property of ADR-0084 §5: every target already
// on disk carries no install id, and one written by `burrow auth login` carries none either, because
// that command contacts no cluster. Requiring one would turn every existing config into a load
// error, which is the outcome the whole design exists to avoid.
func TestTargetWithoutInstallIDLoads(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config")
	yaml := "apiVersion: burrow.dev/v1\nkind: Config\ncurrentTarget: do-nyc1-cluster\ntargets:\n  - name: do-nyc1-cluster\n    kind: kubernetes\n    context: do-nyc1-cluster\n"
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	cfg, err := loadFrom(path)
	if err != nil {
		t.Fatalf("loadFrom: %v", err)
	}
	tgt, ok := cfg.LookupTarget("do-nyc1-cluster")
	if !ok {
		t.Fatalf("target was not loaded: %+v", cfg.Targets)
	}
	if tgt.InstallID != "" {
		t.Errorf("InstallID = %q, want empty for a target recorded before install ids existed", tgt.InstallID)
	}
}

// TestSetInstallIDRecordsByContext confirms the id `burrow cluster install` learns is recorded on every
// target that names the context it installed into, and on no other (ADR-0084 §5). The writer knows a
// context; which targets point at it is this package's business.
func TestSetInstallIDRecordsByContext(t *testing.T) {
	cfg := &Config{Targets: []Target{
		{Name: "prod", Kind: TargetKindKubernetes, Context: "do-nyc3-burrow"},
		{Name: "prod-alias", Kind: TargetKindKubernetes, Context: "do-nyc3-burrow"},
		{Name: "staging", Kind: TargetKindKubernetes, Context: "do-lon1-burrow"},
		CloudTarget(),
	}}

	if !cfg.SetInstallID("do-nyc3-burrow", "install-1") {
		t.Fatal("SetInstallID reported nothing updated, want the two targets on that context")
	}
	for _, name := range []string{"prod", "prod-alias"} {
		tgt, _ := cfg.LookupTarget(name)
		if tgt.InstallID != "install-1" {
			t.Errorf("target %s InstallID = %q, want install-1", name, tgt.InstallID)
		}
	}
	if tgt, _ := cfg.LookupTarget("staging"); tgt.InstallID != "" {
		t.Errorf("a target on another context was given the id: %q", tgt.InstallID)
	}
	if tgt, _ := cfg.LookupTarget(CloudEndpoint); tgt.InstallID != "" {
		t.Errorf("the managed target was given an install id: %q", tgt.InstallID)
	}
}

// TestSetInstallIDWithNoTargetIsNotAnError confirms installing without having run `burrow auth
// login` first is an ordinary state: there is nowhere to record the id, nothing is changed, and the
// caller is told so rather than failing.
func TestSetInstallIDWithNoTargetIsNotAnError(t *testing.T) {
	cfg := &Config{}
	if cfg.SetInstallID("do-nyc3-burrow", "install-1") {
		t.Error("SetInstallID reported an update against a config with no targets")
	}
}

// TestResolveCarriesTheTargetInstallID confirms the id reaches the resolution a command connects
// with, which is the only way it can reach the wire.
func TestResolveCarriesTheTargetInstallID(t *testing.T) {
	cfg := &Config{
		CurrentTarget: "prod",
		Targets:       []Target{{Name: "prod", Kind: TargetKindKubernetes, Context: "do-nyc1-nonprod", InstallID: "install-1"}},
	}
	resolved, err := ResolveOperate(cfg, writeKubeconfig(t))
	if err != nil {
		t.Fatalf("ResolveOperate: %v", err)
	}
	if resolved.InstallID != "install-1" {
		t.Errorf("resolved InstallID = %q, want install-1", resolved.InstallID)
	}
}

// TestResolveCarriesTheInstallIDThroughAPinnedHandle confirms a pinned handle inside the target's
// cluster narrows WHICH ENVIRONMENT is acted on without changing WHICH INSTALL that is: the id
// survives the narrowing, so the check still holds on the path most people are actually on.
func TestResolveCarriesTheInstallIDThroughAPinnedHandle(t *testing.T) {
	cfg := &Config{
		Current:       "nonprod",
		CurrentTarget: "prod",
		Environments:  []Environment{{Name: "nonprod", Context: "do-nyc1-nonprod", AppNamespace: "apps"}},
		Targets:       []Target{{Name: "prod", Kind: TargetKindKubernetes, Context: "do-nyc1-nonprod", InstallID: "install-1"}},
	}
	resolved, err := ResolveOperate(cfg, writeKubeconfig(t))
	if err != nil {
		t.Fatalf("ResolveOperate: %v", err)
	}
	if resolved.Mode != ModePinned {
		t.Fatalf("mode = %q, want pinned (the handle narrows the target)", resolved.Mode)
	}
	if resolved.InstallID != "install-1" {
		t.Errorf("resolved InstallID = %q, want install-1", resolved.InstallID)
	}
}

// TestSetTargetClearsAStaleInstallID pins the behaviour the mismatch message depends on. Re-pointing
// at a context is exactly what somebody does after rebuilding the cluster behind it, so carrying the
// old id forward would preserve a mismatch through the act meant to resolve it. Login cannot learn
// the new id (it contacts no cluster), so it leaves the target unchecked — the state every target
// was in before ids existed, and one that is served.
func TestSetTargetClearsAStaleInstallID(t *testing.T) {
	cfg := &Config{Targets: []Target{
		{Name: "prod", Kind: TargetKindKubernetes, Context: "do-nyc3-burrow", InstallID: "install-old"},
	}}
	if err := cfg.SetTarget(KubernetesTarget("do-nyc3-burrow")); err != nil {
		t.Fatalf("SetTarget: %v", err)
	}
	tgt, _ := cfg.LookupTarget("do-nyc3-burrow")
	if tgt.InstallID != "" {
		t.Errorf("InstallID = %q, want it cleared: a re-login must not preserve an id for a cluster that may have been rebuilt", tgt.InstallID)
	}
}

// TestSetInstallIDRecordsOnHandlesToo confirms the id lands on environment handles as well as
// targets (ADR-0084 §5). The CLI resolves through a target and burrow-agent through a handle, so
// recording only one checks the operator and exempts the agent.
func TestSetInstallIDRecordsOnHandlesToo(t *testing.T) {
	cfg := &Config{
		Environments: []Environment{
			{Name: "prod", Context: "do-nyc3-burrow"},
			{Name: "staging", Context: "do-lon1-burrow"},
		},
	}
	if !cfg.SetInstallID("do-nyc3-burrow", "install-1") {
		t.Fatal("SetInstallID reported nothing updated, want the handle on that context")
	}
	env, _ := cfg.Lookup("prod")
	if env.InstallID != "install-1" {
		t.Errorf("handle InstallID = %q, want install-1", env.InstallID)
	}
	if other, _ := cfg.Lookup("staging"); other.InstallID != "" {
		t.Errorf("a handle on another context was given the id: %q", other.InstallID)
	}
}

// TestResolveFallsBackToTheHandleInstallID confirms a target written by `burrow auth login` — which
// carries no id — still resolves to the id its cluster's handle knows. Without the fallback the CLI
// would go unchecked on a cluster the local config can describe perfectly well.
func TestResolveFallsBackToTheHandleInstallID(t *testing.T) {
	cfg := &Config{
		CurrentTarget: "prod",
		Environments:  []Environment{{Name: "nonprod", Context: "do-nyc1-nonprod", InstallID: "install-1"}},
		Targets:       []Target{{Name: "prod", Kind: TargetKindKubernetes, Context: "do-nyc1-nonprod"}},
	}
	resolved, err := ResolveOperate(cfg, writeKubeconfig(t))
	if err != nil {
		t.Fatalf("ResolveOperate: %v", err)
	}
	if resolved.InstallID != "install-1" {
		t.Errorf("resolved InstallID = %q, want install-1 from the handle", resolved.InstallID)
	}
}

// TestRegisterTargetDoesNotSelect is the whole distinction between registering and signing in
// ([cloud#201](https://github.com/burrow-cloud/cloud/issues/201)). An install says a cluster runs
// Burrow, which is what a target records; it does not say the person wants their next command to go
// there, and re-pointing them would be the same silent redirection the record is about, aimed the
// other way.
func TestRegisterTargetDoesNotSelect(t *testing.T) {
	cfg := &Config{}
	if err := cfg.SetTarget(CloudTarget()); err != nil {
		t.Fatalf("SetTarget: %v", err)
	}

	added, err := cfg.RegisterTarget(KubernetesTarget("do-nyc1"))
	if err != nil {
		t.Fatalf("RegisterTarget: %v", err)
	}
	if !added {
		t.Error("RegisterTarget reported nothing added for a target that was not there")
	}
	if cfg.CurrentTarget != CloudEndpoint {
		t.Errorf("current target = %q, want the selection untouched", cfg.CurrentTarget)
	}
	if _, ok := cfg.LookupTarget("do-nyc1"); !ok {
		t.Errorf("the target was not registered: %+v", cfg.Targets)
	}
	// And it is reachable, which is the point: `burrow auth switch` had nothing to offer before.
	if !strings.Contains(cfg.TargetNames(), "do-nyc1") {
		t.Errorf("TargetNames() = %q, want the registered cluster listed", cfg.TargetNames())
	}
}

// TestRegisterTargetIsIdempotent covers the re-run install and the second person joining. An entry
// already recorded is left exactly as it is, install id and all — that id is a fact about the cluster
// and discarding it here would re-open the mismatch ADR-0084 §5 exists to catch.
func TestRegisterTargetIsIdempotent(t *testing.T) {
	cfg := &Config{}
	if err := cfg.SetTarget(KubernetesTarget("do-nyc1")); err != nil {
		t.Fatalf("SetTarget: %v", err)
	}
	if !cfg.SetInstallID("do-nyc1", "abc123") {
		t.Fatal("SetInstallID recorded nothing")
	}

	added, err := cfg.RegisterTarget(KubernetesTarget("do-nyc1"))
	if err != nil {
		t.Fatalf("RegisterTarget: %v", err)
	}
	if added {
		t.Error("RegisterTarget reported an addition for a target already recorded")
	}
	if len(cfg.Targets) != 1 {
		t.Errorf("targets = %+v, want the one entry", cfg.Targets)
	}
	if cfg.Targets[0].InstallID != "abc123" {
		t.Errorf("install id = %q, want the recorded id preserved", cfg.Targets[0].InstallID)
	}
}

// TestLoadDerivesATargetForEveryEnvironment holds the model's invariant: an environment is one of
// possibly several environments UNDER a target, so a handle with no target above it is a config that
// does not hold together. That state is what made a self-hosted person's own install invisible to
// `burrow auth status` and unreachable from `burrow auth switch` (cloud#201).
//
// Nothing is SELECTED by it. Somebody who has never run `burrow auth login` still follows their
// kubeconfig exactly as before.
func TestLoadDerivesATargetForEveryEnvironment(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config")
	saved := &Config{
		Current: "prod",
		Environments: []Environment{
			{Name: "prod", Context: "do-nyc1", InstallID: "abc123"},
			{Name: "staging", Context: "do-nyc1"}, // a second environment in the SAME cluster
			{Name: "lab", Context: "kind-lab"},
		},
	}
	if err := saved.saveTo(path); err != nil {
		t.Fatalf("save: %v", err)
	}

	cfg, err := loadFrom(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(cfg.Targets) != 2 {
		t.Fatalf("targets = %+v, want one per CLUSTER rather than one per handle", cfg.Targets)
	}
	tgt, ok := cfg.LookupTarget("do-nyc1")
	if !ok {
		t.Fatalf("no target derived for do-nyc1: %+v", cfg.Targets)
	}
	if tgt.Kind != TargetKindKubernetes || tgt.Context != "do-nyc1" {
		t.Errorf("derived target = %+v, want a kubernetes target on the handle's context", tgt)
	}
	// The handle knows which install answers there, and the derived target reaches the same one, so
	// the ADR-0084 §5 check applies from the moment it exists rather than waiting for another install.
	if tgt.InstallID != "abc123" {
		t.Errorf("derived install id = %q, want the handle's", tgt.InstallID)
	}
	if cfg.CurrentTarget != "" {
		t.Errorf("current target = %q; deriving a target must not select one", cfg.CurrentTarget)
	}
	if _, ok := cfg.LookupTarget("kind-lab"); !ok {
		t.Errorf("no target derived for kind-lab: %+v", cfg.Targets)
	}
}

// TestLoadLeavesRecordedTargetsAlone: the derivation fills a gap and never edits what is written
// down. A cluster already registered keeps its entry — including an entry deliberately named
// something other than its context — and a Burrow Cloud target is untouched by any of it.
func TestLoadLeavesRecordedTargetsAlone(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config")
	saved := &Config{
		Environments: []Environment{{Name: "prod", Context: "do-nyc1"}},
		Targets: []Target{
			{Name: "production", Kind: TargetKindKubernetes, Context: "do-nyc1", InstallID: "abc123"},
			CloudTarget(),
		},
		CurrentTarget: CloudEndpoint,
	}
	if err := saved.saveTo(path); err != nil {
		t.Fatalf("save: %v", err)
	}

	cfg, err := loadFrom(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(cfg.Targets) != 2 {
		t.Fatalf("targets = %+v, want the two recorded entries and no duplicate for do-nyc1", cfg.Targets)
	}
	if cfg.CurrentTarget != CloudEndpoint {
		t.Errorf("current target = %q, want the recorded selection", cfg.CurrentTarget)
	}
}

// TestLoadDerivationWritesNothing: it runs in memory. A read has never rewritten ~/.burrow/config and
// must not start; the derived entry is persisted only when a command that was going to save anyway
// does so.
func TestLoadDerivationWritesNothing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config")
	saved := &Config{Environments: []Environment{{Name: "prod", Context: "do-nyc1"}}}
	if err := saved.saveTo(path); err != nil {
		t.Fatalf("save: %v", err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	if _, err := loadFrom(path); err != nil {
		t.Fatalf("load: %v", err)
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(before) != string(after) {
		t.Errorf("loading the config rewrote it:\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

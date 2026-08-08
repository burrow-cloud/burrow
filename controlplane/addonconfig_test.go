// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package controlplane_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	cp "github.com/burrow-cloud/burrow/controlplane"
	"github.com/burrow-cloud/burrow/controlplane/internal/fake"
)

// These tests cover ADR-0082's engine half: an instance is configured after it exists, growing is
// not shrinking, and removing the last standby takes ADR-0081 §2's read address with it.
//
// The two that carry the record are TestConfigureStorageRefusesAShrink — the refusal happens HERE,
// with nothing written, rather than in a `Cluster` status field — and
// TestConfigureStandbysToZeroWithdrawsTheReadAddress, which is §3: the inverse of the operation that
// added it, so the failure arrives at the change rather than at the app's next read.

// newConfigEngine builds an engine with a Postgres instance installed in the default environment,
// which is the state every configuration change starts from: an instance created plain, with no
// standby, because install takes no shape flag (ADR-0081 §1).
func newConfigEngine(t *testing.T) (*cp.Engine, *fake.Kubernetes, *fake.Database, *fake.Provisioner) {
	t.Helper()
	e, k, d, _, _, prov := newPhysicalBackupEngine(t)
	if _, err := e.InstallAddon(context.Background(), cp.AddonPostgres, cp.DefaultEnvironment, cp.InstallAddonOptions{Confirm: true}); err != nil {
		t.Fatalf("InstallAddon: %v", err)
	}
	return e, k, d, prov
}

// defaultInstance is the name every consumer resolves the default environment's Postgres instance
// at, which is also the name a confirmation asks to have typed back.
func defaultInstance(t *testing.T) string {
	t.Helper()
	name, err := cp.AddonInstanceName(cp.AddonPostgres, cp.DefaultEnvironment)
	if err != nil {
		t.Fatalf("AddonInstanceName: %v", err)
	}
	return name
}

// attachApp attaches an app for real, so the read address is asserted against what the attach path
// actually leaves behind rather than against a hand-seeded Secret.
func attachApp(t *testing.T, e *cp.Engine, k *fake.Kubernetes, app string) {
	t.Helper()
	if err := k.ApplyWorkload(context.Background(), cp.WorkloadSpec{App: app, Kind: cp.WorkloadDeployment, Image: "img:1", Replicas: 1}); err != nil {
		t.Fatalf("ApplyWorkload %s: %v", app, err)
	}
	if _, err := e.AttachAddon(context.Background(), cp.AddonPostgres, app, cp.DefaultEnvironment, ""); err != nil {
		t.Fatalf("AttachAddon %s: %v", app, err)
	}
}

// TestAddonSettingsReportsTheShapeOfTheLiveInstance is the bare `burrow addon config postgres`: what
// can be set, and what it is set to. A freshly installed instance reads as standby-less, which is
// ADR-0081 §1's decision showing through the listing rather than being asserted about separately.
func TestAddonSettingsReportsTheShapeOfTheLiveInstance(t *testing.T) {
	e, _, _, _ := newConfigEngine(t)

	res, err := e.AddonSettings(context.Background(), cp.AddonPostgres, "")
	if err != nil {
		t.Fatalf("AddonSettings: %v", err)
	}
	if res.Instance != defaultInstance(t) {
		t.Errorf("instance = %q, want %q", res.Instance, defaultInstance(t))
	}
	values := map[cp.AddonSetting]string{}
	for _, s := range res.Settings {
		values[s.Setting] = s.Value
		if s.Consequence == "" {
			t.Errorf("setting %q carries no consequence; a setting listed without what changing it does is a flag's one line of help wearing a table (ADR-0082 §1)", s.Setting)
		}
	}
	if values[cp.AddonSettingStandbys] != "0" {
		t.Errorf("standbys = %q, want 0: an instance is always created without one (ADR-0081 §1)", values[cp.AddonSettingStandbys])
	}
	if values[cp.AddonSettingStorage] == "" {
		t.Error("storage has no value, so an operator cannot tell what a new size would be compared against")
	}
}

// TestConfigureStandbysGrowsWithoutAsking is ADR-0082 §2's growing half: adding capacity breaks
// nothing that exists, so it proceeds on the strength of having been typed.
func TestConfigureStandbysGrowsWithoutAsking(t *testing.T) {
	e, k, _, _ := newConfigEngine(t)

	res, err := e.ConfigureAddon(context.Background(), cp.AddonPostgres, "", cp.AddonSettingStandbys, "1", cp.ConfigureAddonOptions{})
	if err != nil {
		t.Fatalf("ConfigureAddon: %v", err)
	}
	if !res.Changed || res.From != "0" || res.To != "1" {
		t.Errorf("result = %+v, want a change from 0 to 1", res)
	}
	shape, ok := k.AddonShape(defaultInstance(t))
	if !ok || shape.Standbys != 1 {
		t.Errorf("the cluster holds %+v (found=%v), want 1 standby: the result is only a claim, the instance is the fact", shape, ok)
	}
}

// TestConfigureStandbysWritesTheReadAddressWithTheFirstStandby is ADR-0081 §2: the address appears
// only when there is a standby to read from, and the apps are restarted so they can see it.
func TestConfigureStandbysWritesTheReadAddressWithTheFirstStandby(t *testing.T) {
	e, k, _, _ := newConfigEngine(t)
	attachApp(t, e, k, "api")
	attachApp(t, e, k, "web")

	res, err := e.ConfigureAddon(context.Background(), cp.AddonPostgres, "", cp.AddonSettingStandbys, "1", cp.ConfigureAddonOptions{})
	if err != nil {
		t.Fatalf("ConfigureAddon: %v", err)
	}
	if res.ReadAddress.Action != cp.ReadAddressWritten {
		t.Fatalf("read address action = %q, want %q", res.ReadAddress.Action, cp.ReadAddressWritten)
	}
	if strings.Join(res.ReadAddress.Apps, ",") != "api,web" {
		t.Errorf("read address reached %v, want both attached apps named", res.ReadAddress.Apps)
	}
	for _, app := range []string{"api", "web"} {
		got, ok := k.SecretValue(app, "DATABASE_URL_READ")
		if !ok {
			t.Fatalf("%s has no DATABASE_URL_READ; a standby nobody can reach is not a read replica", app)
		}
		// The read address must name the `-ro` endpoint and NOT the one the write URL names: an
		// address pointing at the same service would be the primary wearing a second name, which
		// ADR-0081 §2 rejects by name.
		if want := fake.ReadURLFor(app, cp.DefaultEnvironment); got != want {
			t.Errorf("%s's read address = %q, want %q", app, got, want)
		}
		if write, _ := k.SecretValue(app, "DATABASE_URL"); write == got {
			t.Errorf("%s's read address is identical to its write address, so reads would land on the primary", app)
		}
		if _, restarted := k.RestartedAt(app); !restarted {
			t.Errorf("%s was not restarted, so its pods cannot see the variable that was just written", app)
		}
	}
}

// TestConfigureStandbysToZeroWithdrawsTheReadAddress is ADR-0082 §3. The alternative is a variable
// resolving to nothing, which fails at the app's next read rather than at the operation that caused
// it — and a failure at the moment of the change is one somebody can connect to the change.
func TestConfigureStandbysToZeroWithdrawsTheReadAddress(t *testing.T) {
	ctx := context.Background()
	e, k, _, _ := newConfigEngine(t)
	attachApp(t, e, k, "api")
	if _, err := e.ConfigureAddon(ctx, cp.AddonPostgres, "", cp.AddonSettingStandbys, "1", cp.ConfigureAddonOptions{}); err != nil {
		t.Fatalf("scale up: %v", err)
	}

	res, err := e.ConfigureAddon(ctx, cp.AddonPostgres, "", cp.AddonSettingStandbys, "0", cp.ConfigureAddonOptions{Confirm: true})
	if err != nil {
		t.Fatalf("ConfigureAddon: %v", err)
	}
	if res.ReadAddress.Action != cp.ReadAddressWithdrawn {
		t.Fatalf("read address action = %q, want %q", res.ReadAddress.Action, cp.ReadAddressWithdrawn)
	}
	if _, ok := k.SecretValue("api", "DATABASE_URL_READ"); ok {
		t.Error("api still holds a read address with no standby to serve it, so it resolves to nothing")
	}
	if _, ok := k.SecretValue("api", "DATABASE_URL"); !ok {
		t.Error("api lost its ordinary connection string; a scale-down takes the READ address away and nothing else")
	}
}

// TestConfigureStandbysHoldsAShrinkAndNamesTheApps is ADR-0082 §2's other half. A count is not
// consent: the refusal has to say which apps, for the reason ADR-0064 §2 gives.
func TestConfigureStandbysHoldsAShrinkAndNamesTheApps(t *testing.T) {
	ctx := context.Background()
	e, k, _, _ := newConfigEngine(t)
	attachApp(t, e, k, "api")
	if _, err := e.ConfigureAddon(ctx, cp.AddonPostgres, "", cp.AddonSettingStandbys, "1", cp.ConfigureAddonOptions{}); err != nil {
		t.Fatalf("scale up: %v", err)
	}
	before := len(k.Configures())

	_, err := e.ConfigureAddon(ctx, cp.AddonPostgres, "", cp.AddonSettingStandbys, "0", cp.ConfigureAddonOptions{})
	if err == nil {
		t.Fatal("an unconfirmed scale-down proceeded; taking capacity away is held for confirmation (ADR-0082 §2)")
	}
	if !errors.Is(err, cp.ErrInvalid) {
		t.Errorf("error = %v, want ErrInvalid", err)
	}
	if !strings.Contains(err.Error(), "api") {
		t.Errorf("the refusal does not name the affected app: %v", err)
	}
	if got := len(k.Configures()); got != before {
		t.Errorf("%d configuration call(s) reached the cluster, want none: a held change writes nothing", got-before)
	}
	if _, ok := k.SecretValue("api", "DATABASE_URL_READ"); !ok {
		t.Error("api lost its read address to a change that was refused")
	}
}

// TestConfigureStorageGrows is the ordinary case, and it proceeds without asking for the same reason
// a scale-up does.
func TestConfigureStorageGrows(t *testing.T) {
	e, k, _, _ := newConfigEngine(t)

	res, err := e.ConfigureAddon(context.Background(), cp.AddonPostgres, "", cp.AddonSettingStorage, "50Gi", cp.ConfigureAddonOptions{})
	if err != nil {
		t.Fatalf("ConfigureAddon: %v", err)
	}
	if !res.Changed || res.To != "50Gi" {
		t.Errorf("result = %+v, want the volume grown to 50Gi", res)
	}
	if shape, _ := k.AddonShape(defaultInstance(t)); shape.Storage != "50Gi" {
		t.Errorf("the cluster holds storage %q, want 50Gi", shape.Storage)
	}
}

// TestConfigureStorageRefusesAShrink is ADR-0082 §2's refusal: a volume cannot shrink, so Burrow says
// so at the point of asking instead of writing a smaller size and letting a `Cluster` explain it in a
// status field. Confirming does not open it, because there is nothing achievable to agree to.
func TestConfigureStorageRefusesAShrink(t *testing.T) {
	ctx := context.Background()
	e, k, _, _ := newConfigEngine(t)
	if _, err := e.ConfigureAddon(ctx, cp.AddonPostgres, "", cp.AddonSettingStorage, "50Gi", cp.ConfigureAddonOptions{}); err != nil {
		t.Fatalf("grow: %v", err)
	}
	before := len(k.Configures())

	_, err := e.ConfigureAddon(ctx, cp.AddonPostgres, "", cp.AddonSettingStorage, "20Gi", cp.ConfigureAddonOptions{Confirm: true})
	if err == nil {
		t.Fatal("a volume shrink was accepted; it cannot be performed, so it is refused rather than attempted")
	}
	if !errors.Is(err, cp.ErrInvalid) {
		t.Errorf("error = %v, want ErrInvalid", err)
	}
	// Both sizes, because the interesting failure here is a typo rather than a considered decision.
	if !strings.Contains(err.Error(), "50Gi") || !strings.Contains(err.Error(), "20Gi") {
		t.Errorf("the refusal names neither the current size nor the requested one: %v", err)
	}
	if got := len(k.Configures()); got != before {
		t.Errorf("%d configuration call(s) reached the cluster, want none", got-before)
	}
}

// TestConfigureAddonIsANoOpAtTheShapeItAlreadyHas: a re-run of the same command is ordinary, and
// reporting a change that did not happen would make a scale-down out of nothing.
func TestConfigureAddonIsANoOpAtTheShapeItAlreadyHas(t *testing.T) {
	e, k, _, _ := newConfigEngine(t)

	res, err := e.ConfigureAddon(context.Background(), cp.AddonPostgres, "", cp.AddonSettingStandbys, "0", cp.ConfigureAddonOptions{})
	if err != nil {
		t.Fatalf("ConfigureAddon: %v", err)
	}
	if res.Changed {
		t.Error("a change was reported for an instance already in the shape that was asked for")
	}
	if got := len(k.Configures()); got != 0 {
		t.Errorf("%d configuration call(s) reached the cluster, want none", got)
	}
}

// TestConfigureAddonAuditsWhatChangedFromWhatToWhat is ADR-0082's acceptance criterion on the audit
// row. "The instance was configured" answers none of the questions asked of the row afterwards.
func TestConfigureAddonAuditsWhatChangedFromWhatToWhat(t *testing.T) {
	e, _, d, _ := newConfigEngine(t)

	if _, err := e.ConfigureAddon(context.Background(), cp.AddonPostgres, "", cp.AddonSettingStandbys, "2", cp.ConfigureAddonOptions{}); err != nil {
		t.Fatalf("ConfigureAddon: %v", err)
	}
	var row *cp.AuditEntry
	for i, r := range d.AuditRows() {
		if r.Operation == "addon_config" {
			row = &d.AuditRows()[i]
		}
	}
	if row == nil {
		t.Fatal("no addon_config audit row was written")
	}
	if row.Target != defaultInstance(t) {
		t.Errorf("audit target = %q, want the instance %q", row.Target, defaultInstance(t))
	}
	if row.Args["setting"] != "standbys" || row.Args["from"] != "0" || row.Args["to"] != "2" {
		t.Errorf("audit args = %v, want the setting and both values", row.Args)
	}
}

// TestConfigureAddonRefusesAnAddonWithNoShape and TestConfigureAddonRefusesAnUnknownSetting: the two
// refusals that name what CAN be said, because a caller who reaches either has most of it right.
func TestConfigureAddonRefusesAnAddonWithNoShape(t *testing.T) {
	e, _, _, _ := newConfigEngine(t)

	_, err := e.ConfigureAddon(context.Background(), cp.AddonCache, "", cp.AddonSettingStandbys, "1", cp.ConfigureAddonOptions{})
	if !errors.Is(err, cp.ErrInvalid) {
		t.Fatalf("error = %v, want ErrInvalid: the cache has no configurable shape yet", err)
	}
}

func TestConfigureAddonRefusesAnUnknownSetting(t *testing.T) {
	e, _, _, _ := newConfigEngine(t)

	_, err := e.ConfigureAddon(context.Background(), cp.AddonPostgres, "", cp.AddonSetting("replicas"), "2", cp.ConfigureAddonOptions{})
	if !errors.Is(err, cp.ErrInvalid) {
		t.Fatalf("error = %v, want ErrInvalid", err)
	}
	if !strings.Contains(err.Error(), "standbys") {
		t.Errorf("the refusal does not name a setting that does exist: %v", err)
	}
}

// TestConfigureAddonRefusesAnEnvironmentWithNoInstance: there is nothing to configure, and saying so
// is better than writing a `Cluster` into existence as a side effect of a scale.
func TestConfigureAddonRefusesAnEnvironmentWithNoInstance(t *testing.T) {
	e, _, _, _ := newConfigEngine(t)
	if _, err := e.AddEnvironment(context.Background(), "staging", "burrow-apps-staging"); err != nil {
		t.Fatalf("AddEnvironment: %v", err)
	}

	_, err := e.ConfigureAddon(context.Background(), cp.AddonPostgres, "staging", cp.AddonSettingStandbys, "1", cp.ConfigureAddonOptions{})
	if !errors.Is(err, cp.ErrNotFound) {
		t.Fatalf("error = %v, want ErrNotFound", err)
	}
}

// TestScaleUpStrandsAnAppWithoutFailingTheChange: the standby exists by the time the read address is
// written, so an app that could not be given one is reported rather than raised — failing here would
// send an operator to repeat a scale-up that already happened.
func TestScaleUpStrandsAnAppWithoutFailingTheChange(t *testing.T) {
	e, k, _, prov := newConfigEngine(t)
	attachApp(t, e, k, "api")
	prov.SetReadURLError(errors.New("no credential"))

	res, err := e.ConfigureAddon(context.Background(), cp.AddonPostgres, "", cp.AddonSettingStandbys, "1", cp.ConfigureAddonOptions{})
	if err != nil {
		t.Fatalf("ConfigureAddon: %v", err)
	}
	if !res.Changed {
		t.Error("the standby was not reported as added, though nothing about it failed")
	}
	if len(res.ReadAddress.Stranded) != 1 || res.ReadAddress.Stranded[0].App != "api" {
		t.Fatalf("stranded = %+v, want api reported with a reason", res.ReadAddress.Stranded)
	}
	if !strings.Contains(res.ReadAddress.Stranded[0].Reason, "addon attach") {
		t.Errorf("the stranded reason names no way out: %q", res.ReadAddress.Stranded[0].Reason)
	}
}

// TestAttachWritesTheReadAddressWhenTheInstanceHasAStandby closes ADR-0081 §2 on the other side: an
// app attached AFTER the standby exists gets the address too, so the set of apps holding one is not
// merely whoever happened to be attached at the moment of the scale-up.
func TestAttachWritesTheReadAddressWhenTheInstanceHasAStandby(t *testing.T) {
	ctx := context.Background()
	e, k, _, _ := newConfigEngine(t)
	if _, err := e.ConfigureAddon(ctx, cp.AddonPostgres, "", cp.AddonSettingStandbys, "1", cp.ConfigureAddonOptions{}); err != nil {
		t.Fatalf("scale up: %v", err)
	}

	attachApp(t, e, k, "late")

	if _, ok := k.SecretValue("late", "DATABASE_URL_READ"); !ok {
		t.Error("an app attached to an instance that already has a standby got no read address")
	}
}

// TestAttachWritesNoReadAddressWithoutAStandby is the same rule from the other direction, and it is
// the property ADR-0081 §2 argues for hardest: an address that is always present reads as a thing to
// use, and on a standby-less instance it points at no endpoint at all.
func TestAttachWritesNoReadAddressWithoutAStandby(t *testing.T) {
	e, k, _, _ := newConfigEngine(t)

	attachApp(t, e, k, "api")

	if _, ok := k.SecretValue("api", "DATABASE_URL_READ"); ok {
		t.Error("a standby-less instance handed out a read address, which resolves to nothing")
	}
}

// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package controlplane_test

import (
	"context"
	"errors"
	"testing"
	"time"

	cp "github.com/burrow-cloud/burrow/controlplane"
	"github.com/burrow-cloud/burrow/controlplane/internal/fake"
)

// The property under test is issue #504's: a build that succeeds is never silently discarded. A
// build runs for minutes in the cluster on a Kubernetes object with a life of its own, so the client
// that started it can be gone by the time it finishes — and before this, everything that knew what
// the build was for went with it. What survives now is the build itself, carrying its own intent,
// and the control plane finishes it.

// reconcileClock is when the fake engines below think it is.
var reconcileClock = time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)

// newReconcileEngine wires an engine with a build ledger alongside the standard fakes, plus the
// reconciler bound to it.
func newReconcileEngine(t *testing.T, policy cp.Policy) (*cp.BuildReconciler, *fake.Database, *fake.BuildLedger, *fake.Kubernetes) {
	t.Helper()
	k := fake.NewKubernetes()
	d := fake.NewDatabase()
	d.SetPolicy(policy)
	ledger := fake.NewBuildLedger()
	e, err := cp.New(cp.Deps{
		Kubernetes: k, Database: d, Clock: fake.NewClock(reconcileClock),
		IDs: fake.NewIDs(), Resolver: fake.NewResolver(), Credentials: fake.NewCredentials(),
		DNS: fake.NewDNSFactory(), Builder: fake.NewBuilder(), BuildLedger: ledger,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return e.NewBuildReconciler(cp.BuildReconcilerConfig{}), d, ledger, k
}

// strandedShop is a build of the app "shop" that succeeded long enough ago to be past any settling
// margin, and whose deploy never ran.
func strandedShop() cp.StrandedBuild {
	return cp.StrandedBuild{
		ID:          "burrow-build-5af8e9f4ad07",
		App:         "shop",
		Env:         "prod",
		Image:       "registry.example/shop:build",
		Digest:      "sha256:1111111111111111111111111111111111111111111111111111111111111111",
		CompletedAt: reconcileClock.Add(-time.Hour),
	}
}

// strandedImage is the digest-pinned reference a finished strandedShop deploys.
const strandedImage = "registry.example/shop:build@sha256:1111111111111111111111111111111111111111111111111111111111111111"

// deployedImages returns the images of every release recorded for shop/prod, in order.
func deployedImages(t *testing.T, d *fake.Database) []string {
	t.Helper()
	releases, err := d.Releases(context.Background(), "shop", "prod")
	if err != nil {
		t.Fatalf("Releases: %v", err)
	}
	images := make([]string, 0, len(releases))
	for _, r := range releases {
		images = append(images, r.Image)
	}
	return images
}

// TestABuildWhoseClientDisconnectedStillDeploys is the bug: the Job cloned, ran every Dockerfile
// step, and pushed the image, and no app existed. The build that succeeded is now finished — through
// the ordinary guarded deploy, pinned to the digest the build produced — and the build is reaped
// once it has been used.
func TestABuildWhoseClientDisconnectedStillDeploys(t *testing.T) {
	r, d, ledger, k := newReconcileEngine(t, permissive())
	ledger.Add(strandedShop())

	r.ReconcileOnceForTest(context.Background())

	if got := deployedImages(t, d); len(got) != 1 || got[0] != strandedImage {
		t.Fatalf("releases = %v, want one pinned to the digest the build produced (%s)", got, strandedImage)
	}
	if spec, ok := k.Spec("shop"); !ok || spec.Image != strandedImage {
		t.Errorf("cluster workload = %+v (present=%v), want the recovered build's image", spec, ok)
	}
	if got := ledger.Reaped(); len(got) != 1 || got[0] != strandedShop().ID {
		t.Errorf("reaped = %v, want the build to be discarded once it had been used", got)
	}
}

// TestARecoveredBuildDoesNotDeployTwice is the other half of the property. The build is durable
// cluster state and the sweep is repeated, so "finish what was never finished" must be safe to run
// again — against a second sweep, against a control plane that crashed between deploying and
// reaping, and against the original caller getting there first.
func TestARecoveredBuildDoesNotDeployTwice(t *testing.T) {
	r, d, ledger, _ := newReconcileEngine(t, permissive())
	ledger.Add(strandedShop())

	ctx := context.Background()
	r.ReconcileOnceForTest(ctx)
	// Put it back: the reap is best-effort in production, so a build the reconciler has already
	// finished can be listed again by the next sweep.
	ledger.Add(strandedShop())
	r.ReconcileOnceForTest(ctx)

	if got := deployedImages(t, d); len(got) != 1 {
		t.Fatalf("releases = %v, want exactly one: the same digest must not deploy twice", got)
	}
}

// TestARecoveredBuildIsNotDeployedOverTheCallerWhoGotThereFirst asserts the guard is the release
// history rather than a timer: a build whose image already has a release was never discarded, so
// there is nothing to finish and nothing to record.
func TestARecoveredBuildIsNotDeployedOverTheCallerWhoGotThereFirst(t *testing.T) {
	r, d, ledger, _ := newReconcileEngine(t, permissive())
	ledger.Add(strandedShop())

	// The build's original caller finished it a moment before the sweep.
	if err := d.SaveRelease(context.Background(), cp.Release{
		ID: "rel-first", App: "shop", Environment: "prod", Image: strandedImage,
		Replicas: 1, Status: cp.ReleaseDeployed, CreatedAt: reconcileClock,
	}); err != nil {
		t.Fatalf("SaveRelease: %v", err)
	}

	r.ReconcileOnceForTest(context.Background())

	if got := deployedImages(t, d); len(got) != 1 {
		t.Fatalf("releases = %v, want the one the original caller recorded", got)
	}
	if got := ledger.Reaped(); len(got) != 1 {
		t.Errorf("reaped = %v, want the finished build tidied away", got)
	}
}

// TestAnAlreadySupersededBuildIsNotResurrected asserts the guard looks at the whole history rather
// than at what is running: a build that deployed and has since been rolled past must not be brought
// back over a newer release. Resurrecting an old image would be a far worse bug than losing one.
func TestAnAlreadySupersededBuildIsNotResurrected(t *testing.T) {
	r, d, ledger, _ := newReconcileEngine(t, permissive())
	ledger.Add(strandedShop())

	ctx := context.Background()
	for _, rel := range []cp.Release{
		{ID: "rel-old", App: "shop", Environment: "prod", Image: strandedImage, Replicas: 1, Status: cp.ReleaseSuperseded, CreatedAt: reconcileClock},
		{ID: "rel-new", App: "shop", Environment: "prod", Image: "registry.example/shop:2", Replicas: 1, Status: cp.ReleaseDeployed, CreatedAt: reconcileClock.Add(time.Minute)},
	} {
		if err := d.SaveRelease(ctx, rel); err != nil {
			t.Fatalf("SaveRelease: %v", err)
		}
	}

	r.ReconcileOnceForTest(ctx)

	got := deployedImages(t, d)
	if len(got) != 2 {
		t.Fatalf("releases = %v, want the two that were already there", got)
	}
	if got[len(got)-1] != "registry.example/shop:2" {
		t.Errorf("newest release = %q, want the newer image to still be on top", got[len(got)-1])
	}
}

// TestAnOvertakenBuildDoesNotRollTheAppBack is the guard that keeps the fix from being worse than
// the bug. The build was stranded, the operator moved the app on themselves, and finishing the build
// now would silently roll production back to an older image with nobody watching. A build that has
// been overtaken is set aside instead — the one case where a successful build is not deployed, and a
// deliberate one.
func TestAnOvertakenBuildDoesNotRollTheAppBack(t *testing.T) {
	r, d, ledger, k := newReconcileEngine(t, permissive())
	ledger.Add(strandedShop())

	ctx := context.Background()
	// The operator deployed something else after the build finished.
	if err := d.SaveRelease(ctx, cp.Release{
		ID: "rel-newer", App: "shop", Environment: "prod", Image: "registry.example/shop:2",
		Replicas: 1, Status: cp.ReleaseDeployed, CreatedAt: strandedShop().CompletedAt.Add(time.Minute),
	}); err != nil {
		t.Fatalf("SaveRelease: %v", err)
	}

	r.ReconcileOnceForTest(ctx)

	if got := deployedImages(t, d); len(got) != 1 || got[0] != "registry.example/shop:2" {
		t.Fatalf("releases = %v, want only the newer one the operator deployed", got)
	}
	if _, ok := k.Spec("shop"); ok {
		t.Error("an overtaken build reached the cluster and rolled the app back")
	}
	if reason, held := ledger.Held(strandedShop().ID); !held || reason != "superseded" {
		t.Errorf("hold = (%q, %v), want the overtaken build set aside as superseded", reason, held)
	}
	if got := ledger.Reaped(); len(got) != 0 {
		t.Errorf("reaped = %v, want the build left in place so it can still be re-run deliberately", got)
	}
}

// TestADeletedAppIsNotResurrected asserts the guard that a delete cannot be undone by a build that
// was already in flight. Deleting an app takes its release history with it, so the release guards see
// nothing at all; the append-only audit trail is what still remembers the delete happened.
func TestADeletedAppIsNotResurrected(t *testing.T) {
	r, d, ledger, k := newReconcileEngine(t, permissive())
	ledger.Add(strandedShop())

	ctx := context.Background()
	if err := d.AppendAudit(ctx, cp.AuditEntry{
		Timestamp: strandedShop().CompletedAt.Add(time.Minute),
		Operation: "app_delete", Target: "shop", Outcome: cp.AuditExecuted,
	}); err != nil {
		t.Fatalf("AppendAudit: %v", err)
	}

	r.ReconcileOnceForTest(ctx)

	if got := deployedImages(t, d); len(got) != 0 {
		t.Fatalf("releases = %v, want none: a deleted app must not come back", got)
	}
	if _, ok := k.Spec("shop"); ok {
		t.Error("a deleted app was resurrected by a build that was already in flight")
	}
	if reason, held := ledger.Held(strandedShop().ID); !held || reason != "app deleted" {
		t.Errorf("hold = (%q, %v), want the build set aside because its app was deleted", reason, held)
	}
}

// TestADeleteBeforeTheBuildFinishedDoesNotBlockIt is the control for the test above: an app deleted
// and then rebuilt is a normal thing to do, and the guard must look at WHEN the delete happened.
func TestADeleteBeforeTheBuildFinishedDoesNotBlockIt(t *testing.T) {
	r, d, ledger, _ := newReconcileEngine(t, permissive())
	ledger.Add(strandedShop())

	ctx := context.Background()
	if err := d.AppendAudit(ctx, cp.AuditEntry{
		Timestamp: strandedShop().CompletedAt.Add(-time.Hour),
		Operation: "app_delete", Target: "shop", Outcome: cp.AuditExecuted,
	}); err != nil {
		t.Fatalf("AppendAudit: %v", err)
	}

	r.ReconcileOnceForTest(ctx)

	if got := deployedImages(t, d); len(got) != 1 {
		t.Errorf("releases = %v, want the build finished: the delete predates it", got)
	}
}

// TestARecoveredDeploySaysNobodyWasThere asserts the audit trail does not claim a human was present.
// The trigger stays manual, because the build was manually triggered and only its completion was
// not, and the row carries the fact that the control plane finished it.
func TestARecoveredDeploySaysNobodyWasThere(t *testing.T) {
	r, d, ledger, _ := newReconcileEngine(t, permissive())
	ledger.Add(strandedShop())

	ctx := context.Background()
	r.ReconcileOnceForTest(ctx)

	rows, err := d.Audit(ctx, cp.AuditFilter{App: "shop", Operation: "deploy"})
	if err != nil {
		t.Fatalf("Audit: %v", err)
	}
	if len(rows) == 0 {
		t.Fatal("the recovered deploy recorded nothing")
	}
	for _, row := range rows {
		if row.Args["recovered"] != "build" {
			t.Errorf("audit args = %v, want the row to say the control plane finished a build", row.Args)
		}
	}
}

// TestAMarkerThatCannotBeWrittenIsStillADecision asserts the reconciler does not re-record the same
// guardrail decision every sweep when the ledger cannot be marked at all — an install whose RBAC
// predates the verb the marker needs. One permanently held build must not write an audit row a
// minute for the whole of the build's retention.
func TestAMarkerThatCannotBeWrittenIsStillADecision(t *testing.T) {
	r, d, ledger, _ := newReconcileEngine(t, permissive().With(cp.GuardrailAppDeploy, cp.DispositionConfirm))
	ledger.Add(strandedShop())
	ledger.SetHoldError(errors.New("jobs.batch is forbidden: cannot patch"))

	ctx := context.Background()
	for range 5 {
		r.ReconcileOnceForTest(ctx)
	}

	rows, err := d.Audit(ctx, cp.AuditFilter{Outcome: cp.AuditHeld})
	if err != nil {
		t.Fatalf("Audit: %v", err)
	}
	if len(rows) != 1 {
		t.Errorf("held audit rows across five sweeps = %d, want 1", len(rows))
	}
}

// TestABuildThatKeepsFailingIsGivenUpOn asserts a build that can never be finished stops costing a
// deploy prologue every minute for the whole of its retention.
func TestABuildThatKeepsFailingIsGivenUpOn(t *testing.T) {
	r, d, ledger, _ := newReconcileEngine(t, permissive())
	ledger.Add(strandedShop())
	d.SetError(fake.OpSaveRelease, errors.New("the database is unreachable"))

	ctx := context.Background()
	for range 20 {
		r.ReconcileOnceForTest(ctx)
	}

	rows, err := d.Audit(ctx, cp.AuditFilter{App: "shop", Operation: "deploy"})
	if err != nil {
		t.Fatalf("Audit: %v", err)
	}
	if len(rows) > 6 {
		t.Errorf("deploy attempts across twenty sweeps = %d, want a handful before giving up", len(rows))
	}
	if len(rows) < 2 {
		t.Errorf("deploy attempts = %d, want a transient failure retried at least once", len(rows))
	}
}

// TestAHeldDeployIsHeldWithNoClientAttached is the guardrail question this raises. The deploy after a
// build is guarded, and a held deploy used to surface on the caller's stream. With nobody attached
// there is nobody to confirm, and "run it and hang up" must not become a way around the guardrail:
// the hold is honored, recorded in the audit trail exactly as it would have been for the caller, and
// the build is LEFT IN PLACE so a confirmed re-run reuses the image rather than paying to build it
// again.
func TestAHeldDeployIsHeldWithNoClientAttached(t *testing.T) {
	r, d, ledger, k := newReconcileEngine(t, permissive().With(cp.GuardrailAppDeploy, cp.DispositionConfirm))
	ledger.Add(strandedShop())

	ctx := context.Background()
	r.ReconcileOnceForTest(ctx)

	if got := deployedImages(t, d); len(got) != 0 {
		t.Fatalf("releases = %v, want none: an unattended deploy must not waive a guardrail", got)
	}
	if _, ok := k.Spec("shop"); ok {
		t.Error("a held deploy reached the cluster")
	}
	if got := ledger.Reaped(); len(got) != 0 {
		t.Errorf("reaped = %v, want the held build left in place so a confirmed re-run reuses it", got)
	}
	reason, held := ledger.Held(strandedShop().ID)
	if !held {
		t.Fatal("the held build was not marked, so the same decision would be recorded on every sweep")
	}
	if reason != string(cp.GuardrailAppDeploy) {
		t.Errorf("hold reason = %q, want the guardrail code", reason)
	}

	rows, err := d.Audit(ctx, cp.AuditFilter{})
	if err != nil {
		t.Fatalf("AuditEntries: %v", err)
	}
	var heldRows int
	for _, row := range rows {
		if row.Outcome == cp.AuditHeld && row.GuardrailCode == string(cp.GuardrailAppDeploy) {
			heldRows++
		}
	}
	if heldRows != 1 {
		t.Errorf("held audit rows = %d, want the hold recorded once so the tenant can see what happened", heldRows)
	}

	// A second sweep records nothing further: the marker is what keeps a permanent hold from becoming
	// a permanent stream of audit rows.
	r.ReconcileOnceForTest(ctx)
	rows, err = d.Audit(ctx, cp.AuditFilter{})
	if err != nil {
		t.Fatalf("AuditEntries: %v", err)
	}
	heldRows = 0
	for _, row := range rows {
		if row.Outcome == cp.AuditHeld {
			heldRows++
		}
	}
	if heldRows != 1 {
		t.Errorf("held audit rows after a second sweep = %d, want 1", heldRows)
	}
}

// TestABuildWithNoRecordedIntentIsLeftAlone asserts the known limit rather than leaving it
// ambiguous: a build started by a control plane that predates attribution carries nothing that says
// what it was for, and is not guessed at. Its own retention reaps it.
func TestABuildWithNoRecordedIntentIsLeftAlone(t *testing.T) {
	r, d, ledger, _ := newReconcileEngine(t, permissive())
	b := strandedShop()
	b.App = ""
	ledger.Add(b)

	r.ReconcileOnceForTest(context.Background())

	if got := deployedImages(t, d); len(got) != 0 {
		t.Fatalf("releases = %v, want none", got)
	}
	if got := ledger.Reaped(); len(got) != 0 {
		t.Errorf("reaped = %v, want a build nothing can be done with left for its own retention", got)
	}
}

// TestTheSweepAsksForBuildsPastTheSettlingMargin asserts the reconciler does not race the build's
// original caller: it asks only for builds that finished before now minus the margin, and it reads
// "now" from the injected clock rather than ambient time.
func TestTheSweepAsksForBuildsPastTheSettlingMargin(t *testing.T) {
	r, _, ledger, _ := newReconcileEngine(t, permissive())

	r.ReconcileOnceForTest(context.Background())

	cutoffs := ledger.Cutoffs()
	if len(cutoffs) != 1 {
		t.Fatalf("cutoffs = %v, want one per sweep", cutoffs)
	}
	if want := reconcileClock.Add(-cp.DefaultBuildSettleMargin); !cutoffs[0].Equal(want) {
		t.Errorf("cutoff = %s, want %s (now minus the settling margin)", cutoffs[0], want)
	}
}

// TestASweepThatCannotListIsContained asserts the sweep is fail-soft in the way every Burrow sweep
// is: an unreadable cluster costs this pass and nothing else.
func TestASweepThatCannotListIsContained(t *testing.T) {
	r, d, ledger, _ := newReconcileEngine(t, permissive())
	ledger.Add(strandedShop())
	ledger.SetListError(errors.New("the apiserver is unreachable"))

	ctx := context.Background()
	r.ReconcileOnceForTest(ctx)
	if got := deployedImages(t, d); len(got) != 0 {
		t.Fatalf("releases = %v, want none while the cluster cannot be read", got)
	}

	ledger.SetListError(nil)
	r.ReconcileOnceForTest(ctx)
	if got := deployedImages(t, d); len(got) != 1 {
		t.Errorf("releases = %v, want the build finished on the next sweep", got)
	}
}

// TestTheReconcilerIsOptional asserts an install with no build ledger wired behaves exactly as it did
// before recovery existed: Run returns rather than spinning.
func TestTheReconcilerIsOptional(t *testing.T) {
	e, _, _, _ := newBuildEngine(t, permissive())
	r := e.NewBuildReconciler(cp.BuildReconcilerConfig{})

	done := make(chan struct{})
	go func() {
		r.Run(context.Background())
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return with no build ledger wired")
	}
}

// TestTheEngineRecordsWhatEachBuildIsFor asserts the engine hands the builder the intent that makes a
// build recoverable — including the environment resolved to a concrete name, so a build recovered
// later needs no context to interpret, and the PUBLIC deploy reference rather than the internal push
// target (ADR-0054), because that is the one a release must carry.
func TestTheEngineRecordsWhatEachBuildIsFor(t *testing.T) {
	k := fake.NewKubernetes()
	d := fake.NewDatabase()
	d.SetPolicy(permissive())
	b := fake.NewBuilder()
	e, err := cp.New(cp.Deps{
		Kubernetes: k, Database: d, Clock: fake.NewClock(reconcileClock),
		IDs: fake.NewIDs(), Resolver: fake.NewResolver(), Credentials: fake.NewCredentials(),
		DNS: fake.NewDNSFactory(), Builder: b,
		BuildRegistry:       "burrow-registry.burrow.svc.cluster.local:5000",
		BuildPublicRegistry: "registry.example.com",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if _, err := e.Build(context.Background(), cp.BuildRequest{
		App:    "shop",
		Source: cp.SourceRef{Repo: "https://github.com/acme/shop", Ref: "v1.0.0"},
	}); err != nil {
		t.Fatalf("Build: %v", err)
	}

	got := b.LastIntent()
	want := cp.BuildIntent{App: "shop", Env: "prod", Image: "registry.example.com/shop:build"}
	if got != want {
		t.Errorf("recorded intent = %+v, want %+v", got, want)
	}
}

// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package controlplane_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	cp "github.com/burrow-cloud/burrow/controlplane"
	"github.com/burrow-cloud/burrow/controlplane/internal/fake"
)

// The lock (cloud ADR-0060): destroying something takes two deliberate acts, and everything that is
// not destruction is untouched. These tests assert both halves, because a mechanism that only ever
// gets the first half right is one an operator turns off.

// mustLocked asserts err is a lock refusal and returns it, and asserts it is NOT a guardrail refusal.
// The second half is the point: a guardrail refusal tells a caller that confirming, or calling as
// somebody else, may work. Neither is true of a lock, and a client that read one as the other would
// send an agent into a retry loop against a refusal that never changes.
func mustLocked(t *testing.T, err error) *cp.LockedError {
	t.Helper()
	l, ok := cp.AsLocked(err)
	if !ok {
		t.Fatalf("err = %v, want a LockedError", err)
	}
	if _, isGuardrail := cp.AsGuardrail(err); isGuardrail {
		t.Fatalf("a lock refusal must not also be a guardrail refusal: %v", err)
	}
	return l
}

// TestLockedAppRefusesDeleteAndUnlockingLetsItThrough is the central claim: locked, the delete
// refuses; unlocked, the same command works. The two acts are the whole mechanism.
func TestLockedAppRefusesDeleteAndUnlockingLetsItThrough(t *testing.T) {
	ctx := context.Background()
	e, k, _, _ := newEngine(t, permissive().With(cp.GuardrailAppDelete, cp.DispositionConfirm))
	if err := k.ApplyWorkload(ctx, cp.WorkloadSpec{App: "web", Kind: cp.WorkloadDeployment, Image: "img:1", Replicas: 1}); err != nil {
		t.Fatalf("seed workload: %v", err)
	}

	res, err := e.LockApp(ctx, "web", "")
	if err != nil {
		t.Fatalf("LockApp: %v", err)
	}
	if !res.Locked || !res.Changed {
		t.Fatalf("LockApp result = %+v, want locked and changed", res)
	}

	err = e.DeleteApp(ctx, "web", "", true)
	l := mustLocked(t, err)
	if l.Subject != cp.LockSubjectApp || l.Name != "web" {
		t.Errorf("refusal names %s %q, want the app web", l.Subject, l.Name)
	}
	// The app is still there: a refusal that had already torn part of it down would be worse than
	// no refusal at all.
	if _, err := k.WorkloadStatus(ctx, "web"); err != nil {
		t.Fatalf("the workload was touched by a refused delete: %v", err)
	}

	if _, err := e.UnlockApp(ctx, "web", ""); err != nil {
		t.Fatalf("UnlockApp: %v", err)
	}
	if err := e.DeleteApp(ctx, "web", "", true); err != nil {
		t.Fatalf("DeleteApp after unlock: %v", err)
	}
	if _, err := k.WorkloadStatus(ctx, "web"); !errors.Is(err, cp.ErrNotFound) {
		t.Errorf("workload after the unlocked delete: err = %v, want ErrNotFound", err)
	}
}

// TestLockedRefusalNamesTheUnlockAndIsNotAPermissionsError asserts the wording, which is part of the
// mechanism rather than decoration. It has to name the command that removes the lock — a refusal
// that does not is a support question — and it must not read like a permissions error, or a person
// goes looking for access they already have while the true answer is one command they can run.
func TestLockedRefusalNamesTheUnlockAndIsNotAPermissionsError(t *testing.T) {
	ctx := context.Background()
	e, k, _, _ := newEngine(t, permissive().With(cp.GuardrailAppDelete, cp.DispositionConfirm))
	if err := k.ApplyWorkload(ctx, cp.WorkloadSpec{App: "web", Kind: cp.WorkloadDeployment, Image: "img:1", Replicas: 1}); err != nil {
		t.Fatalf("seed workload: %v", err)
	}
	if _, err := e.LockApp(ctx, "web", ""); err != nil {
		t.Fatalf("LockApp: %v", err)
	}

	msg := mustLocked(t, e.DeleteApp(ctx, "web", "", true)).Error()
	for _, want := range []string{"locked", "burrow unlock web --env prod"} {
		if !strings.Contains(msg, want) {
			t.Errorf("refusal %q does not contain %q", msg, want)
		}
	}
	// The words a permissions refusal uses. A lock says nothing about the caller — it holds against
	// the person who set it — so borrowing this vocabulary would describe the wrong mechanism.
	for _, unwanted := range []string{"denied", "permission", "not permitted", "forbidden", "unauthorized", "guardrail", "disposition", "--confirm"} {
		if strings.Contains(strings.ToLower(msg), unwanted) {
			t.Errorf("refusal %q reads like a permissions/guardrail error: it contains %q", msg, unwanted)
		}
	}
}

// TestLockedAppStillDeploysScalesAndRollsBack is the other half of the design, and the half that
// keeps the mechanism switched on. A lock that interrupted ordinary work would be removed and never
// restored, so everything reversible must proceed exactly as before.
func TestLockedAppStillDeploysScalesAndRollsBack(t *testing.T) {
	ctx := context.Background()
	e, _, _, _ := newEngine(t, permissive())

	if _, err := e.Deploy(ctx, cp.DeployRequest{App: "web", Image: "img:1", Replicas: 1, Confirm: true}); err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	if _, err := e.LockApp(ctx, "web", ""); err != nil {
		t.Fatalf("LockApp: %v", err)
	}

	if _, err := e.Deploy(ctx, cp.DeployRequest{App: "web", Image: "img:2", Replicas: 1, Confirm: true}); err != nil {
		t.Errorf("a locked app refused a deploy: %v", err)
	}
	if _, err := e.Scale(ctx, "web", "", 3, true); err != nil {
		t.Errorf("a locked app refused a scale: %v", err)
	}
	if _, err := e.Rollback(ctx, "web", "", cp.RollbackOptions{Confirm: true}); err != nil {
		t.Errorf("a locked app refused a rollback: %v", err)
	}
	// Confirmed, because a config write is held by app.config since ADR-0098 — the hold is the
	// guardrail's, and what this asserts is that the LOCK adds nothing to it.
	if err := e.SetConfig(ctx, "web", "", "K", "V", false, true); err != nil {
		t.Errorf("a locked app refused a config change: %v", err)
	}
	// And the lock survived all of it: it persists across deploys, restarts and rollbacks, which is
	// what makes it worth setting.
	st, err := e.Status(ctx, "web", "")
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if !st.Locked {
		t.Error("the lock did not survive a deploy, a scale and a rollback")
	}
}

// TestLockedAddonInstanceRefusesRemoveAndDeleteDataDetachButNotAnOrdinaryOne: the instance holds the
// data, so the operations that destroy it refuse — and the detach that KEEPS the data does not,
// because re-attaching gets it back.
func TestLockedAddonInstanceRefusesRemoveAndDeleteDataDetachButNotAnOrdinaryOne(t *testing.T) {
	ctx := context.Background()
	e, k, _, prov := newPostgresEngine(t)
	if err := k.ApplyWorkload(ctx, cp.WorkloadSpec{App: "web", Kind: cp.WorkloadDeployment, Image: "busybox", Replicas: 1}); err != nil {
		t.Fatalf("seed workload: %v", err)
	}
	if _, err := e.AttachAddon(ctx, cp.AddonPostgres, "web", "", cp.AttachAddonOptions{Confirm: true}); err != nil {
		t.Fatalf("AttachAddon: %v", err)
	}

	instance := mustInstance(t, cp.AddonPostgres, cp.DefaultEnvironment)
	if _, err := e.LockAddonInstance(ctx, instance, ""); err != nil {
		t.Fatalf("LockAddonInstance: %v", err)
	}

	// Removing the instance refuses, whether or not it was asked to destroy the data.
	if _, err := e.RemoveAddon(ctx, instance, cp.RemoveAddonOptions{Confirm: true}); mustLocked(t, err).Subject != cp.LockSubjectAddonInstance {
		t.Errorf("remove refusal names the wrong subject: %v", err)
	}
	if _, err := e.RemoveAddon(ctx, instance, cp.RemoveAddonOptions{DeleteData: true, Confirm: true}); err == nil {
		t.Error("a data-deleting removal of a locked instance succeeded")
	}

	// Detaching with --delete-data refuses: that is the form that cannot be undone.
	err := e.DetachAddon(ctx, cp.AddonPostgres, "web", "", cp.DetachAddonOptions{DeleteData: true, Confirm: true})
	if l := mustLocked(t, err); !strings.Contains(l.Error(), "burrow unlock addon "+instance) {
		t.Errorf("the detach refusal does not name the add-on unlock command: %v", err)
	}
	if got := prov.Dropped(); len(got) != 0 {
		t.Errorf("a refused --delete-data detach still dropped a database: %v", got)
	}

	// The ordinary detach proceeds. It keeps the database, so it undoes by re-attaching, and a lock
	// that blocked it would be a lock somebody switched off.
	if err := e.DetachAddon(ctx, cp.AddonPostgres, "web", "", cp.DetachAddonOptions{Confirm: true}); err != nil {
		t.Fatalf("a locked instance refused an ordinary detach: %v", err)
	}
	if got := prov.Databases(cp.DefaultEnvironment); len(got) != 1 || got[0] != "web" {
		t.Errorf("databases after the ordinary detach = %v, want web's kept", got)
	}
}

// TestLockedAddonInstanceStillAttachesAndBacksUp: attaching and backing up are not destruction, so a
// locked instance serves them unchanged.
func TestLockedAddonInstanceStillAttaches(t *testing.T) {
	ctx := context.Background()
	e, k, _, _ := newPostgresEngine(t)
	if err := k.ApplyWorkload(ctx, cp.WorkloadSpec{App: "web", Kind: cp.WorkloadDeployment, Image: "busybox", Replicas: 1}); err != nil {
		t.Fatalf("seed workload: %v", err)
	}
	if _, err := e.LockAddonInstance(ctx, mustInstance(t, cp.AddonPostgres, cp.DefaultEnvironment), ""); err != nil {
		t.Fatalf("LockAddonInstance: %v", err)
	}
	if _, err := e.AttachAddon(ctx, cp.AddonPostgres, "web", "", cp.AttachAddonOptions{Confirm: true}); err != nil {
		t.Errorf("a locked instance refused an attach: %v", err)
	}
}

// TestUnlockIsAudited: both acts are recorded and the unlock is the line worth reading — an unlock
// with no deletion after it is a lock somebody removed and forgot to restore.
func TestUnlockIsAudited(t *testing.T) {
	ctx := context.Background()
	e, k, d, _ := newEngine(t, permissive())
	if err := k.ApplyWorkload(ctx, cp.WorkloadSpec{App: "web", Kind: cp.WorkloadDeployment, Image: "img:1", Replicas: 1}); err != nil {
		t.Fatalf("seed workload: %v", err)
	}
	if _, err := e.LockApp(ctx, "web", ""); err != nil {
		t.Fatalf("LockApp: %v", err)
	}
	if _, err := e.UnlockApp(ctx, "web", ""); err != nil {
		t.Fatalf("UnlockApp: %v", err)
	}

	locks := auditRows(t, d, "lock")
	if len(locks) != 1 || locks[0].Target != "web" || locks[0].Outcome != cp.AuditExecuted {
		t.Errorf("lock audit rows = %+v, want one executed row for web", locks)
	}
	unlocks := auditRows(t, d, "unlock")
	if len(unlocks) != 1 {
		t.Fatalf("unlock audit rows = %+v, want exactly one", unlocks)
	}
	row := unlocks[0]
	if row.Target != "web" || row.Outcome != cp.AuditExecuted {
		t.Errorf("unlock row = %+v, want an executed row for web", row)
	}
	if row.Args["subject"] != string(cp.LockSubjectApp) || row.Args["env"] != cp.DefaultEnvironment {
		t.Errorf("unlock row args = %v, want the subject and environment recorded", row.Args)
	}
	// It carries no guardrail decision, because the verb has no guardrail: a lock is not policy
	// about a caller, so there is no disposition to record.
	if row.GuardrailCode != "" {
		t.Errorf("unlock row names guardrail %q; the verb has none", row.GuardrailCode)
	}
}

// TestLockIsIdempotentAndKeepsItsTimestamp: locking something already locked reports it locked and
// unchanged, and does not move the timestamp. The row answers "since when has this been protected",
// and a second person asserting the same protection is not a new answer to that question.
func TestLockIsIdempotentAndKeepsItsTimestamp(t *testing.T) {
	ctx := context.Background()
	e, k, _, clock := newEngine(t, permissive())
	if err := k.ApplyWorkload(ctx, cp.WorkloadSpec{App: "web", Kind: cp.WorkloadDeployment, Image: "img:1", Replicas: 1}); err != nil {
		t.Fatalf("seed workload: %v", err)
	}
	first, err := e.LockApp(ctx, "web", "")
	if err != nil {
		t.Fatalf("LockApp: %v", err)
	}
	clock.Advance(72 * time.Hour) // three days later
	second, err := e.LockApp(ctx, "web", "")
	if err != nil {
		t.Fatalf("LockApp (again): %v", err)
	}
	if !second.Locked || second.Changed {
		t.Errorf("second lock = %+v, want locked and unchanged", second)
	}
	if !second.LockedAt.Equal(first.LockedAt) {
		t.Errorf("second lock moved the timestamp from %s to %s", first.LockedAt, second.LockedAt)
	}
	// Unlocking something that is not locked is likewise no error: it is the state the caller asked
	// for, and a failure here would read as "something is wrong" to somebody about to type a
	// destructive command.
	if _, err := e.UnlockApp(ctx, "web", ""); err != nil {
		t.Fatalf("UnlockApp: %v", err)
	}
	again, err := e.UnlockApp(ctx, "web", "")
	if err != nil {
		t.Fatalf("UnlockApp (already unlocked): %v", err)
	}
	if again.Locked || again.Changed {
		t.Errorf("second unlock = %+v, want unlocked and unchanged", again)
	}
}

// TestLockRequiresTheAppToExist: a lock on a name nobody deployed protects nothing while reading
// exactly like protection, and a mistyped name is the same class of mistake the lock exists for.
func TestLockRequiresTheAppToExist(t *testing.T) {
	e, _, _, _ := newEngine(t, permissive())
	if _, err := e.LockApp(context.Background(), "webiste", ""); !errors.Is(err, cp.ErrNotFound) {
		t.Errorf("LockApp on an unknown app err = %v, want ErrNotFound", err)
	}
}

// TestAnUnreadableLockRefusesTheDelete: "I could not read whether this is locked" and "it is not
// locked" are different facts, and reading the first as the second destroys the one thing somebody
// asked Burrow to protect. The destructive path fails closed.
func TestAnUnreadableLockRefusesTheDelete(t *testing.T) {
	ctx := context.Background()
	e, k, d, _ := newEngine(t, permissive().With(cp.GuardrailAppDelete, cp.DispositionAllow))
	if err := k.ApplyWorkload(ctx, cp.WorkloadSpec{App: "web", Kind: cp.WorkloadDeployment, Image: "img:1", Replicas: 1}); err != nil {
		t.Fatalf("seed workload: %v", err)
	}
	d.SetError(fake.OpLock, errBoom)
	if err := e.DeleteApp(ctx, "web", "", true); err == nil {
		t.Fatal("a delete proceeded while the lock could not be read")
	}
	if _, err := k.WorkloadStatus(ctx, "web"); err != nil {
		t.Errorf("the workload was torn down despite the unreadable lock: %v", err)
	}
}

// TestLockStateIsVisibleInTheListings: a lock nobody can see is a lock somebody removes and forgets
// to restore, so it rides on the listings and on status rather than being discoverable only by
// attempting to destroy something.
func TestLockStateIsVisibleInTheListings(t *testing.T) {
	ctx := context.Background()
	e, k, _, _ := newEngine(t, permissive())
	for _, app := range []string{"web", "api"} {
		if err := k.ApplyWorkload(ctx, cp.WorkloadSpec{App: app, Kind: cp.WorkloadDeployment, Image: "img:1", Replicas: 1}); err != nil {
			t.Fatalf("seed workload %s: %v", app, err)
		}
	}
	if _, err := e.LockApp(ctx, "web", ""); err != nil {
		t.Fatalf("LockApp: %v", err)
	}

	apps, err := e.ListApps(ctx, "")
	if err != nil {
		t.Fatalf("ListApps: %v", err)
	}
	for _, a := range apps {
		if want := a.App == "web"; a.Locked != want {
			t.Errorf("listing reports %s locked=%t, want %t", a.App, a.Locked, want)
		}
	}
	st, err := e.Status(ctx, "api", "")
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if st.Locked {
		t.Error("status reports an unlocked app as locked")
	}
}

// TestAddonListingReportsTheLock: the same visibility for the instance, keyed by its label in its
// own environment.
func TestAddonListingReportsTheLock(t *testing.T) {
	ctx := context.Background()
	e, _, _, _ := newPostgresEngine(t)
	instance := mustInstance(t, cp.AddonPostgres, cp.DefaultEnvironment)
	if _, err := e.LockAddonInstance(ctx, instance, ""); err != nil {
		t.Fatalf("LockAddonInstance: %v", err)
	}
	addons, err := e.ListAddons(ctx)
	if err != nil {
		t.Fatalf("ListAddons: %v", err)
	}
	var seen bool
	for _, a := range addons {
		if a.Name != instance {
			continue
		}
		seen = true
		if !a.Locked {
			t.Error("the add-ons listing does not report the instance as locked")
		}
	}
	if !seen {
		t.Fatalf("the instance %q is not in the listing", instance)
	}
}

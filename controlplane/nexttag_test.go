// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package controlplane_test

import (
	"context"
	"errors"
	"testing"

	cp "github.com/burrow-cloud/burrow/controlplane"
)

// TestNextTagSuggestsTrio proves NextTag reads the app's current running tag and returns the next
// patch/minor/major (ADR-0052 §8), turning "use semver" into concrete numbers the agent applies.
func TestNextTagSuggestsTrio(t *testing.T) {
	ctx := context.Background()
	e, _, d, _ := newEngine(t, permissive())
	seedRunningRelease(t, d, "web", "ghcr.io/u/web:1.4.2")

	res, err := e.NextTag(ctx, "web", "")
	if err != nil {
		t.Fatalf("NextTag: %v", err)
	}
	if res.Current != "1.4.2" || res.Next == nil {
		t.Fatalf("NextTag = current %q next %+v, want 1.4.2 with a suggestion", res.Current, res.Next)
	}
	if res.Next.Patch != "1.4.3" || res.Next.Minor != "1.5.0" || res.Next.Major != "2.0.0" {
		t.Errorf("Next = %q/%q/%q, want 1.4.3 / 1.5.0 / 2.0.0", res.Next.Patch, res.Next.Minor, res.Next.Major)
	}
	if res.Note != "" {
		t.Errorf("Note = %q, want empty for a semver current tag", res.Note)
	}
}

// TestNextTagNonSemverNotes proves a non-semver current tag degrades to a structured note with no
// suggestion and no error (ADR-0040): the guidance is best-effort, never a failure.
func TestNextTagNonSemverNotes(t *testing.T) {
	ctx := context.Background()
	e, _, d, _ := newEngine(t, permissive())
	seedRunningRelease(t, d, "web", "ghcr.io/u/web:sha-abc")

	res, err := e.NextTag(ctx, "web", "")
	if err != nil {
		t.Fatalf("NextTag returned an error on a non-semver tag, want graceful degrade: %v", err)
	}
	if res.Current != "sha-abc" || res.Next != nil {
		t.Errorf("NextTag = current %q next %+v, want current sha-abc with no suggestion", res.Current, res.Next)
	}
	if res.Note == "" {
		t.Errorf("Note is empty, want a reason semver could not be suggested")
	}
}

// TestNextTagNoReleaseNotes proves an app with no running release degrades to a note rather than
// erroring (ADR-0040).
func TestNextTagNoReleaseNotes(t *testing.T) {
	ctx := context.Background()
	e, _, _, _ := newEngine(t, permissive())

	res, err := e.NextTag(ctx, "web", "")
	if err != nil {
		t.Fatalf("NextTag: %v", err)
	}
	if res.Current != "" || res.Next != nil || res.Note == "" {
		t.Errorf("NextTag = current %q next %+v note %q, want no current, no suggestion, a note", res.Current, res.Next, res.Note)
	}
}

// TestNextTagRejectsInvalidApp proves the read verb still validates its app name.
func TestNextTagRejectsInvalidApp(t *testing.T) {
	ctx := context.Background()
	e, _, _, _ := newEngine(t, permissive())
	if _, err := e.NextTag(ctx, "Bad Name", ""); !errors.Is(err, cp.ErrInvalid) {
		t.Errorf("NextTag(bad name) err = %v, want ErrInvalid", err)
	}
}

// TestDeploySemverNoHint proves a semver tag deploys with no nudge hint (ADR-0052 §8): it can be
// classified for auto-update, so there is nothing to note.
func TestDeploySemverNoHint(t *testing.T) {
	ctx := context.Background()
	e, _, _, _ := newEngine(t, permissive())

	res, err := e.Deploy(ctx, cp.DeployRequest{App: "web", Image: "ghcr.io/u/web:1.2.3", Replicas: 1})
	if err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	if len(res.Hints) != 0 {
		t.Errorf("Hints = %v, want none for a semver tag", res.Hints)
	}
}

// TestDeployNonSemverHint proves a non-semver tag deploys successfully but carries a non-blocking
// hint nudging toward semver (ADR-0052 §8, ADR-0007): the deploy lands, the hint is advisory.
func TestDeployNonSemverHint(t *testing.T) {
	ctx := context.Background()
	e, _, _, _ := newEngine(t, permissive())

	res, err := e.Deploy(ctx, cp.DeployRequest{App: "web", Image: "ghcr.io/u/web:latest", Replicas: 1})
	if err != nil {
		t.Fatalf("Deploy of a non-semver tag must still succeed (ADR-0007): %v", err)
	}
	if res.Release.Status != cp.ReleaseDeployed {
		t.Errorf("release status = %q, want deployed (the hint does not gate the deploy)", res.Release.Status)
	}
	if len(res.Hints) == 0 {
		t.Fatalf("Hints is empty, want a semver nudge for a non-semver tag")
	}
}

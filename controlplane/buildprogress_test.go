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

// A build reports its stages so a caller blocked on it for minutes hears something before the end
// (issue #503). The property carrying these tests is that IT IS ONE SEQUENCE: a build ends where
// deploy begins, and the stages say so.

// TestBuildReportsItsStagesThenTheDeploys is the whole feature at the engine: the build's own stages
// run straight into the deploy stages of the deploy it hands off to, in one unbroken sequence, from
// one reporter.
func TestBuildReportsItsStagesThenTheDeploys(t *testing.T) {
	e, k, _, _ := newBuildEngine(t, permissive())

	var got []string
	res, err := e.Build(context.Background(), cp.BuildRequest{
		App:         "web",
		Source:      cp.SourceRef{Repo: "https://github.com/acme/web", Ref: "v1.0.0"},
		TargetImage: "ghcr.io/acme/web:1.0.0",
		Progress:    recordStages(&got),
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	wantStages(t, got, []string{
		"clone:started", "clone:done",
		"build:started", "build:done",
		"apply:started", "apply:done",
		"settle:started", "settle:done",
	})
	if res.Digest != fake.DefaultDigest {
		t.Errorf("digest = %q, want %q", res.Digest, fake.DefaultDigest)
	}
	if _, ok := k.Spec("web"); !ok {
		t.Error("the build did not deploy the image it built")
	}
}

// TestAFailingBuildReportsAFailedStageAndNoDeployStages pins both halves of a build that does not
// build: the stage that failed is marked failed rather than left open, and NO deploy stage follows —
// a failed build must not touch the deploy path (ADR-0053 §4), and the report must not suggest it did.
func TestAFailingBuildReportsAFailedStageAndNoDeployStages(t *testing.T) {
	e, k, _, b := newBuildEngine(t, permissive())
	b.SetError(errors.New("step 7/12 returned a non-zero code"))

	var got []string
	_, err := e.Build(context.Background(), cp.BuildRequest{
		App:         "web",
		Source:      cp.SourceRef{Repo: "https://github.com/acme/web", Ref: "v1.0.0"},
		TargetImage: "ghcr.io/acme/web:1.0.0",
		Progress:    recordStages(&got),
	})
	if err == nil {
		t.Fatal("Build succeeded with a failing builder")
	}
	wantStages(t, got, []string{
		"clone:started", "clone:done",
		"build:started", "build:failed",
	})
	if _, ok := k.Spec("web"); ok {
		t.Error("a failed build deployed a workload")
	}
}

// TestNothingIsReportedBeforeTheBuildStarts is what keeps a refusal deliverable as a status-coded
// error: the API writes the stream's header on the first event, so everything decided before the
// builder is invoked — validation, a missing target with no in-cluster registry — must report none.
func TestNothingIsReportedBeforeTheBuildStarts(t *testing.T) {
	cases := []struct {
		name string
		req  cp.BuildRequest
	}{
		{
			name: "an invalid app name",
			req:  cp.BuildRequest{App: "", Source: cp.SourceRef{Repo: "https://github.com/acme/web", Ref: "v1"}, TargetImage: "ghcr.io/acme/web:1"},
		},
		{
			name: "an invalid source",
			req:  cp.BuildRequest{App: "web", Source: cp.SourceRef{Repo: "", Ref: ""}, TargetImage: "ghcr.io/acme/web:1"},
		},
		{
			name: "no target and no in-cluster registry to default to",
			req:  cp.BuildRequest{App: "web", Source: cp.SourceRef{Repo: "https://github.com/acme/web", Ref: "v1"}},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e, _, _, _ := newBuildEngine(t, permissive())
			var got []string
			c.req.Progress = recordStages(&got)
			if _, err := e.Build(context.Background(), c.req); err == nil {
				t.Fatal("Build succeeded on a request that should be refused")
			}
			if len(got) != 0 {
				t.Fatalf("stages = %v, want none before the build starts", got)
			}
		})
	}
}

// TestABuildThatAskedForNoProgressIsUnchanged is the default every existing caller takes, including
// `burrow-agent`: no reporter, no events, and the same result. The nil is normalised once at the top
// of Build, so no emit site below it carries a nil check.
func TestABuildThatAskedForNoProgressIsUnchanged(t *testing.T) {
	e, k, _, b := newBuildEngine(t, permissive())
	res, err := e.Build(context.Background(), cp.BuildRequest{
		App:         "web",
		Source:      cp.SourceRef{Repo: "https://github.com/acme/web", Ref: "v1.0.0"},
		TargetImage: "ghcr.io/acme/web:1.0.0",
	})
	if err != nil {
		t.Fatalf("Build with no Progress: %v", err)
	}
	if res.Digest != fake.DefaultDigest {
		t.Errorf("digest = %q, want %q", res.Digest, fake.DefaultDigest)
	}
	if _, ok := k.Spec("web"); !ok {
		t.Error("the build did not deploy the image it built")
	}
	// And it takes the plain seam call. Observing a build costs the cluster a read per interval, so a
	// build nobody is watching must not pay for a report that goes nowhere.
	if b.ProgressCalls() != 0 {
		t.Errorf("the reporting seam was taken %d time(s) for a build that asked for no progress", b.ProgressCalls())
	}
}

// silentBuilder is a Builder that cannot report its own stages — the shape of any implementation
// behind the seam that never opted into ProgressBuilder, including a hardened executor supplied by
// something other than this repo.
type silentBuilder struct {
	digest string
	err    error
}

func (b silentBuilder) Build(context.Context, cp.SourceRef, string, bool, cp.SourceCredential) (string, error) {
	return b.digest, b.err
}

// TestABuilderThatCannotReportIsBracketedAsOneStage pins the fallback. Granularity is the only thing
// a caller loses: the build is still reported as a stage that starts and then finishes, so the first
// event still arrives BEFORE the minutes-long wait rather than after it — which is the whole reason
// the stream survives a proxy read timeout.
func TestABuilderThatCannotReportIsBracketedAsOneStage(t *testing.T) {
	for _, c := range []struct {
		name    string
		builder silentBuilder
		want    []string
	}{
		{
			name:    "a build that succeeds",
			builder: silentBuilder{digest: "sha256:abc"},
			want:    []string{"build:started", "build:done", "apply:started", "apply:done", "settle:started", "settle:done"},
		},
		{
			name:    "a build that fails",
			builder: silentBuilder{err: errors.New("the builder gave up")},
			want:    []string{"build:started", "build:failed"},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			k, d := fake.NewKubernetes(), fake.NewDatabase()
			d.SetPolicy(permissive())
			e, err := cp.New(cp.Deps{
				Kubernetes: k, Database: d, Clock: fake.NewClock(time.Date(2026, 6, 23, 12, 0, 0, 0, time.UTC)),
				IDs: fake.NewIDs(), Resolver: fake.NewResolver(), Credentials: fake.NewCredentials(),
				DNS: fake.NewDNSFactory(), Builder: c.builder,
			})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			var got []string
			_, buildErr := e.Build(context.Background(), cp.BuildRequest{
				App:         "web",
				Source:      cp.SourceRef{Repo: "https://github.com/acme/web", Ref: "v1.0.0"},
				TargetImage: "ghcr.io/acme/web:1.0.0",
				Progress:    recordStages(&got),
			})
			if (buildErr != nil) != (c.builder.err != nil) {
				t.Fatalf("Build err = %v", buildErr)
			}
			wantStages(t, got, c.want)
		})
	}
}

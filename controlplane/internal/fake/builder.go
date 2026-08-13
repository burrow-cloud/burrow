// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package fake

import (
	"context"
	"sync"

	"github.com/burrow-cloud/burrow/controlplane"
)

var _ controlplane.Builder = (*Builder)(nil)

// The fake reports stages like the real adapter does, so the build's progress stream can be exercised
// end to end without a cluster (issue #503).
var _ controlplane.ProgressBuilder = (*Builder)(nil)

// The fake records what each build was for like the real adapter does, so a test can assert the
// intent the engine hands down without a cluster (issue #504).
var _ controlplane.AttributedBuilder = (*Builder)(nil)

// The fake carries a push credential like the real adapter does, so a test can assert what the engine
// resolved for the push target without a cluster (issue #584).
var _ controlplane.PushCredentialBuilder = (*Builder)(nil)

// Builder is an in-memory controlplane.Builder. Tests seed the digest it returns with SetDigest,
// inject a build failure with SetError, and read back the source ref and target image it was called
// with (LastSource / LastTarget) plus the call count, so the in-cluster build orchestration can be
// exercised — build success feeding the guarded deploy path, and build failure NOT touching it —
// without standing up a real Kubernetes build Job (ADR-0053 §6).
type Builder struct {
	mu           sync.Mutex
	digest       string
	err          error
	lastSource   controlplane.SourceRef
	lastTarget   string
	lastInsecure bool
	lastCred     controlplane.SourceCredential
	lastPush     controlplane.PushCredential
	calls        int
	// progressCalls counts the calls that came in through the reporting seam, so a test can tell the
	// two entry points apart.
	progressCalls int
	// lastIntent is what the engine said the last build was FOR (issue #504), so a test can assert
	// the app, environment, and deploy reference recorded against the build itself.
	lastIntent controlplane.BuildIntent
}

// DefaultDigest is the digest NewBuilder returns until SetDigest overrides it, so a test can assert
// the built reference the engine deployed without first seeding a value.
const DefaultDigest = "sha256:1111111111111111111111111111111111111111111111111111111111111111"

// NewBuilder returns a fake builder that returns DefaultDigest on a successful build.
func NewBuilder() *Builder { return &Builder{digest: DefaultDigest} }

// SetDigest seeds the digest a successful Build returns.
func (b *Builder) SetDigest(digest string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.digest = digest
}

// SetError makes Build return err (nil clears it), exercising the build-failure path where the deploy
// path must not be touched.
func (b *Builder) SetError(err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.err = err
}

// LastSource returns the source ref Build was last called with.
func (b *Builder) LastSource() controlplane.SourceRef {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.lastSource
}

// LastTarget returns the target image reference Build was last called with.
func (b *Builder) LastTarget() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.lastTarget
}

// LastInsecure returns the insecure flag Build was last called with — true when the engine pushed to
// the plain-HTTP in-cluster registry (ADR-0054 §5).
func (b *Builder) LastInsecure() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.lastInsecure
}

// LastCredential returns the source-provider credential Build was last called with, so a test can
// assert the engine resolved a configured private-source token and handed it to the builder — or
// handed the zero credential for a public source (ADR-0057).
func (b *Builder) LastCredential() controlplane.SourceCredential {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.lastCred
}

// LastPushCredential returns the push credential Build was last called with, so a test can assert the
// engine resolved one for the push target and handed it to the builder — or handed the zero
// credential, the anonymous push a self-hosted install makes to its in-cluster registry (issue #584).
func (b *Builder) LastPushCredential() controlplane.PushCredential {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.lastPush
}

// Calls returns how many times Build has been called.
func (b *Builder) Calls() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.calls
}

// ProgressCalls returns how many times the engine took the reporting seam rather than the plain one,
// so a test can assert that a build nobody asked to observe is not observed.
func (b *Builder) ProgressCalls() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.progressCalls
}

// LastIntent returns what the engine said the last build was FOR — the app, environment, and deploy
// reference recorded against the build so it can be finished after its caller has gone (issue #504).
func (b *Builder) LastIntent() controlplane.BuildIntent {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.lastIntent
}

func (b *Builder) Build(_ context.Context, source controlplane.SourceRef, targetImage string, insecure bool, cred controlplane.SourceCredential) (string, error) {
	return b.build(controlplane.BuildIntent{}, source, targetImage, insecure, cred, controlplane.PushCredential{}, func(string, string) {})
}

// BuildAttributed records the intent alongside everything Build records, then behaves exactly as the
// entry point matching the caller's reporter would: a nil reporter is a build nobody asked to
// observe, and it is NOT counted as a progress call — the real adapter takes the same single entry
// point either way, and what a test is asserting with ProgressCalls is whether anyone asked for a
// report, not which method was called.
func (b *Builder) BuildAttributed(ctx context.Context, intent controlplane.BuildIntent, source controlplane.SourceRef, targetImage string, insecure bool, cred controlplane.SourceCredential, progress func(controlplane.DeployEvent)) (string, error) {
	return b.BuildWithPushCredential(ctx, intent, source, targetImage, insecure, cred, controlplane.PushCredential{}, progress)
}

// BuildWithPushCredential records the push credential alongside everything BuildAttributed records,
// and otherwise behaves identically — so a test can assert both that a resolved credential reached
// the builder and that a build resolving none is unchanged (issue #584).
func (b *Builder) BuildWithPushCredential(_ context.Context, intent controlplane.BuildIntent, source controlplane.SourceRef, targetImage string, insecure bool, cred controlplane.SourceCredential, push controlplane.PushCredential, progress func(controlplane.DeployEvent)) (string, error) {
	if progress == nil {
		return b.build(intent, source, targetImage, insecure, cred, push, func(string, string) {})
	}
	b.mu.Lock()
	b.progressCalls++
	b.mu.Unlock()
	return b.build(intent, source, targetImage, insecure, cred, push, func(stage, status string) {
		progress(controlplane.DeployEvent{Stage: stage, Status: status})
	})
}

// BuildWithProgress records the call exactly as Build does and reports the stage sequence the real
// adapter reports: the clone, then the build. A seeded error fails the BUILD stage, which is where a
// build fails in the case worth modelling — a Dockerfile step that exited non-zero, after the source
// was already on disk. The heartbeat the adapter emits across a long build has nothing to stand for
// here — a fake build takes no time — so it is not reported.
func (b *Builder) BuildWithProgress(_ context.Context, source controlplane.SourceRef, targetImage string, insecure bool, cred controlplane.SourceCredential, progress func(controlplane.DeployEvent)) (string, error) {
	b.mu.Lock()
	b.progressCalls++
	b.mu.Unlock()
	return b.build(controlplane.BuildIntent{}, source, targetImage, insecure, cred, controlplane.PushCredential{}, func(stage, status string) {
		progress(controlplane.DeployEvent{Stage: stage, Status: status})
	})
}

// build is what both entry points do: record the call and walk the stages, reporting each to event.
//
// THE LOCK IS RELEASED BEFORE ANY EVENT IS REPORTED. The reporter is the caller's own code, and a
// reporter that reads back what the builder recorded — which is exactly what a test asserting the
// call would do — would deadlock on a mutex still held over the callback.
func (b *Builder) build(intent controlplane.BuildIntent, source controlplane.SourceRef, targetImage string, insecure bool, cred controlplane.SourceCredential, push controlplane.PushCredential, event func(stage, status string)) (string, error) {
	b.mu.Lock()
	b.calls++
	b.lastIntent = intent
	b.lastSource = source
	b.lastTarget = targetImage
	b.lastInsecure = insecure
	b.lastCred = cred
	b.lastPush = push
	digest, err := b.digest, b.err
	b.mu.Unlock()

	event(controlplane.StageClone, controlplane.DeployStarted)
	event(controlplane.StageClone, controlplane.DeployDone)
	event(controlplane.StageBuild, controlplane.DeployStarted)
	if err != nil {
		event(controlplane.StageBuild, controlplane.DeployFailed)
		return "", err
	}
	event(controlplane.StageBuild, controlplane.DeployDone)
	return digest, nil
}

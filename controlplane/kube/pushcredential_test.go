// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package kube

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/burrow-cloud/burrow/controlplane"
)

// TestBuildWithPushCredentialMountsRegistryAuthOnly is the case the source-provider credential cannot
// express at all (issue #584): a PUBLIC repository built and pushed to a registry that is nobody's
// source provider. The push credential is consumed by MOUNTING, exactly as the source credential is —
// a docker config.json keyed by the push target's host, in a Secret owned by the Job — and the
// password appears nowhere in the Job spec.
//
// The gitconfig is deliberately ABSENT: there is no clone credential, and a Secret volume item naming
// a key the Secret does not hold leaves the pod unable to start, which would turn a public build into
// a permanent Pending.
func TestBuildWithPushCredentialMountsRegistryAuthOnly(t *testing.T) {
	ctx := context.Background()
	source := controlplane.SourceRef{Repo: "https://github.com/acme/public", Ref: "v1.2.3"}
	const target = "reg.tenants.example:5000/tenant-42/web:build"
	const password = "registry_push_password"
	client, created := buildFakeSucceeding(t, source, target, validDigest)

	push := controlplane.PushCredential{Registry: "reg.tenants.example:5000", Username: "tenant-42", Password: password}
	if _, err := NewBuilder(client).BuildWithPushCredential(ctx, controlplane.BuildIntent{}, source, target, false, controlplane.SourceCredential{}, push, nil); err != nil {
		t.Fatalf("BuildWithPushCredential: %v", err)
	}
	job := (*created)[0]

	secret, err := client.CoreV1().Secrets(buildNamespace).Get(ctx, credSecretName(job.Name), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("credential secret not created: %v", err)
	}
	if _, ok := secret.Data[gitConfigFile]; ok {
		t.Error("a build with no source credential wrote a gitconfig; there is no clone to authenticate")
	}
	dockercfg := string(secret.Data[registryAuthFile])
	wantAuth := base64.StdEncoding.EncodeToString([]byte("tenant-42:" + password))
	if !strings.Contains(dockercfg, wantAuth) {
		t.Error("docker config.json does not carry the base64 push auth")
	}
	if !strings.Contains(dockercfg, "reg.tenants.example:5000") {
		t.Error("docker config.json is not keyed by the push target's registry host")
	}

	// The Secret is owned by the Job, so it is garbage-collected when the Job is reaped: the password
	// never outlives the build.
	if len(secret.OwnerReferences) != 1 || secret.OwnerReferences[0].Kind != "Job" || secret.OwnerReferences[0].Name != job.Name {
		t.Errorf("credential secret ownerReferences = %+v, want a single Job owner %q", secret.OwnerReferences, job.Name)
	}

	// The build container reads it through REGISTRY_AUTH_FILE; the clone mounts nothing.
	build := job.Spec.Template.Spec.Containers[0]
	if got := envValue(build.Env, "REGISTRY_AUTH_FILE"); got != registryAuthPath+"/"+registryAuthFile {
		t.Errorf("build REGISTRY_AUTH_FILE = %q, want the mounted docker config", got)
	}
	if !mountsVolume(build.VolumeMounts, "registry-auth") {
		t.Error("build does not mount the registry-auth volume")
	}
	clone := job.Spec.Template.Spec.InitContainers[0]
	if mountsVolume(clone.VolumeMounts, "git-creds") {
		t.Error("clone mounts a git-creds volume for a public source; the Secret holds no gitconfig")
	}
	for _, v := range job.Spec.Template.Spec.Volumes {
		if v.Name == "git-creds" {
			t.Error("the Job declares a git-creds volume whose Secret key does not exist; the pod could not start")
		}
	}

	// The invariant that matters: the password is NOT in the Job spec — not an env value, not a
	// command, not a volume. It lives only in the separate Secret object.
	raw, err := json.Marshal(job)
	if err != nil {
		t.Fatalf("marshal job: %v", err)
	}
	if strings.Contains(string(raw), password) {
		t.Error("the push password leaked into the build Job spec; it must live only in the mounted Secret")
	}
	assertNoSourceBytes(t, job)
}

// TestBuildWithBothCredentialsWritesBothRegistries is the two-party case: a private GitHub source
// pushed to a registry GitHub knows nothing about. A docker config.json is a MAP from host to
// credential, so two registries are two entries — one mechanism, not two.
func TestBuildWithBothCredentialsWritesBothRegistries(t *testing.T) {
	ctx := context.Background()
	source := controlplane.SourceRef{Repo: "https://github.com/acme/private", Ref: "v1.2.3"}
	const target = "reg.tenants.example:5000/tenant-42/web:build"
	const sourceToken = "ghp_source_token"
	const pushPassword = "registry_push_password"
	client, created := buildFakeSucceeding(t, source, target, validDigest)

	cred := controlplane.SourceCredential{Provider: controlplane.ProviderGitHub, Token: sourceToken}
	push := controlplane.PushCredential{Registry: "reg.tenants.example:5000", Username: "tenant-42", Password: pushPassword}
	if _, err := NewBuilder(client).BuildWithPushCredential(ctx, controlplane.BuildIntent{}, source, target, false, cred, push, nil); err != nil {
		t.Fatalf("BuildWithPushCredential: %v", err)
	}
	job := (*created)[0]
	secret, err := client.CoreV1().Secrets(buildNamespace).Get(ctx, credSecretName(job.Name), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("credential secret not created: %v", err)
	}

	// The clone still authenticates with the source token.
	if gitcfg := string(secret.Data[gitConfigFile]); !strings.Contains(gitcfg, sourceToken) {
		t.Error("gitconfig does not carry the source token for the private clone")
	}

	var cfg struct {
		Auths map[string]struct {
			Auth string `json:"auth"`
		} `json:"auths"`
	}
	if err := json.Unmarshal(secret.Data[registryAuthFile], &cfg); err != nil {
		t.Fatalf("docker config.json is not valid JSON: %v", err)
	}
	if len(cfg.Auths) != 2 {
		t.Fatalf("docker config.json has %d registry entries, want 2 (the provider's and the push target's)", len(cfg.Auths))
	}
	// The provider entry authenticates a private base-image pull from ghcr.io; the push entry
	// authenticates the write to a registry the provider token has no standing at.
	if got, want := cfg.Auths["ghcr.io"].Auth, base64.StdEncoding.EncodeToString([]byte("x-access-token:"+sourceToken)); got != want {
		t.Error("the ghcr.io entry does not carry the source provider's auth")
	}
	if got, want := cfg.Auths["reg.tenants.example:5000"].Auth, base64.StdEncoding.EncodeToString([]byte("tenant-42:"+pushPassword)); got != want {
		t.Error("the push target entry does not carry the push credential's auth")
	}

	raw, err := json.Marshal(job)
	if err != nil {
		t.Fatalf("marshal job: %v", err)
	}
	if strings.Contains(string(raw), pushPassword) || strings.Contains(string(raw), sourceToken) {
		t.Error("a credential leaked into the build Job spec; both must live only in the mounted Secret")
	}
}

// TestBuildWithoutPushCredentialIsUnchanged pins the behaviour every self-hosted install has: no push
// credential, no Secret, no mounted volume, an anonymous push. Adding the seam must not add a
// credential to a build that supplies none.
func TestBuildWithoutPushCredentialIsUnchanged(t *testing.T) {
	ctx := context.Background()
	source := controlplane.SourceRef{Repo: "https://github.com/acme/public", Ref: "v1"}
	const target = "burrow-registry.burrow.svc.cluster.local:5000/web:build"
	client, created := buildFakeSucceeding(t, source, target, validDigest)

	if _, err := NewBuilder(client).BuildWithPushCredential(ctx, controlplane.BuildIntent{}, source, target, true, controlplane.SourceCredential{}, controlplane.PushCredential{}, nil); err != nil {
		t.Fatalf("BuildWithPushCredential: %v", err)
	}
	job := (*created)[0]
	if _, err := client.CoreV1().Secrets(buildNamespace).Get(ctx, credSecretName(job.Name), metav1.GetOptions{}); err == nil {
		t.Error("a build with neither credential created a Secret; it must not")
	}
	for _, v := range job.Spec.Template.Spec.Volumes {
		if v.Secret != nil {
			t.Errorf("build mounts a Secret volume %q; want none", v.Name)
		}
	}
	if got := envValue(job.Spec.Template.Spec.Containers[0].Env, "REGISTRY_AUTH_FILE"); got != "" {
		t.Errorf("build REGISTRY_AUTH_FILE = %q, want unset for an anonymous push", got)
	}
	// The rest of the build is untouched: the insecure push to the in-cluster registry still applies.
	if got := envValue(job.Spec.Template.Spec.Containers[0].Env, "TARGET_INSECURE"); got != "true" {
		t.Errorf("build TARGET_INSECURE = %q, want %q", got, "true")
	}
}

// TestBuildRefusesAPushCredentialWithNoRegistry catches the credential that cannot be written down:
// a docker config.json entry is keyed by host, so a password with no host is a secret with nowhere to
// be presented. The adapter refuses before any Job or Secret exists, and the error names the missing
// field rather than the value.
func TestBuildRefusesAPushCredentialWithNoRegistry(t *testing.T) {
	ctx := context.Background()
	source := controlplane.SourceRef{Repo: "https://github.com/acme/public", Ref: "v1"}
	const target = "reg.tenants.example:5000/tenant-42/web:build"
	const password = "registry_push_password"
	client, created := buildFakeSucceeding(t, source, target, validDigest)

	push := controlplane.PushCredential{Username: "tenant-42", Password: password}
	_, err := NewBuilder(client).BuildWithPushCredential(ctx, controlplane.BuildIntent{}, source, target, false, controlplane.SourceCredential{}, push, nil)
	if err == nil {
		t.Fatal("BuildWithPushCredential with a hostless credential: want an error, got nil")
	}
	if strings.Contains(err.Error(), password) {
		t.Error("the error carries the push password")
	}
	if len(*created) != 0 {
		t.Error("a build Job was created for a credential that could not be written down")
	}
}

// TestPushCredentialOverridesTheProviderEntryForOneHost fixes the collision rule. When the push target
// IS the source provider's registry and both credentials name it, the push credential wins: it was
// resolved for this specific push, while the provider entry is a side effect of who happens to host
// the source.
func TestPushCredentialOverridesTheProviderEntryForOneHost(t *testing.T) {
	cred := controlplane.SourceCredential{Provider: controlplane.ProviderGitHub, Token: "ghp_source_token"}
	push := controlplane.PushCredential{Registry: "ghcr.io", Username: "tenant-42", Password: "push_password"}

	raw, err := registryAuthConfig(cred, push)
	if err != nil {
		t.Fatalf("registryAuthConfig: %v", err)
	}
	var cfg struct {
		Auths map[string]struct {
			Auth string `json:"auth"`
		} `json:"auths"`
	}
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		t.Fatalf("docker config.json is not valid JSON: %v", err)
	}
	if len(cfg.Auths) != 1 {
		t.Fatalf("docker config.json has %d entries, want 1 — both credentials name ghcr.io", len(cfg.Auths))
	}
	if got, want := cfg.Auths["ghcr.io"].Auth, base64.StdEncoding.EncodeToString([]byte("tenant-42:push_password")); got != want {
		t.Error("the ghcr.io entry is not the push credential's; the credential resolved for this push wins")
	}
}

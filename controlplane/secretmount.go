// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package controlplane

import (
	"context"
	"fmt"
	"path"
	"slices"
	"strings"
	"time"
)

// A secret key projected into a FILE (ADR-0089 §1-§3, §5).
//
// A secret reached a running app one way: as an environment variable, sourced wholesale from the
// per-app Secret through envFrom. That is a worse container for a credential than a file in ways
// that are concrete rather than stylistic — it is readable at /proc/<pid>/environ, it is INHERITED
// BY EVERY CHILD PROCESS, and it lands in a crash dump — and a kubeconfig, a PEM private key or a
// service-account JSON is structurally file-shaped, so the variable form means base64 plus a decode
// step in every app that holds one.
//
// A mount changes the PROJECTION and nothing else. The value does not move: same Secret, same
// storage, same writer, so ADR-0029 holds unchanged and nothing in this file ever sees a value. What
// is recorded here is a KEY NAME and a FILENAME.
//
// TWO RULES CARRY THE DESIGN, and both come from Burrow owning the directory rather than the caller
// naming a path:
//
//  1. ONLY MOUNTED KEYS ARE ON DISK. The volume projects Items, one per mounted key, so mounting one
//     key does not put every other credential the app holds into the filesystem — which is what
//     widens the reach of a path-traversal bug or a stray `tar`.
//
//  2. THE DIRECTORY IS PER APP, NEVER PER KEY (§2). A per-key path forces a subPath mount, which is
//     the one form of Secret volume the kubelet DOES NOT UPDATE IN PLACE, so it silently gives up
//     rotation without a rollout; and it can shadow a file in the app's own image, where the failure
//     looks like a broken image rather than a mount. `--dir` moves the whole directory for one app,
//     which loses neither property.
//
// A mount is APP CONFIGURATION, NOT A RELEASE PROPERTY (§5). It is stored per app per environment
// and re-read by deploy, env reapply and rollback, exactly as `cfg` is. If it rode the release,
// rolling back to a release cut before the mount existed would remove the file the running app
// needs — a rollback, the incident escape hatch, would take the credential with it.
//
// MOUNTING ADDS A FILE; IT DOES NOT REMOVE THE VARIABLE (§4). The code that reads the file has to be
// deployed before the variable it replaces disappears, and Burrow does not know when that happened,
// so removing it on mount would break an app mid-rollout. NoEnv is how a caller says otherwise, and
// it is the half of this record that actually takes a credential out of /proc/self/environ.
//
// Its cost is stated rather than discovered. envFrom sources the Secret wholesale and there is no
// way to exclude one key from it, so the first file-only key on an app switches its pod template to
// an ENUMERATED secretKeyRef per remaining key. That changes a behaviour for that app: a `secret
// set` of a NEW key used to reach the pod on a restart, because envFrom picks up whatever the Secret
// holds, and an enumerated template has to be re-applied instead. An app with no file-only key keeps
// envFrom and is bit-for-bit what it was.

const (
	// DefaultSecretsDir is where an app's mounted secret keys land unless `--dir` moves them
	// (ADR-0089 §2). It is a tmpfs path no base image populates, so a mount shadows nothing and the
	// failure mode "my app broke and the mount is why" cannot occur.
	DefaultSecretsDir = "/run/secrets"

	// SecretsDirEnvVar carries the directory to the app, and never a value (ADR-0089 §3). It is the
	// shape the build Job already uses for its own credentials: a path in the environment is not a
	// secret in the environment. Nearly every tool that reads a credential file accepts a path
	// variable (GOOGLE_APPLICATION_CREDENTIALS, KUBECONFIG, REGISTRY_AUTH_FILE), and pointing one at
	// this directory is `burrow app config set`.
	SecretsDirEnvVar = "BURROW_SECRETS_DIR"

	// SecretsVolumeName is the name of the one Secret volume an app's pod template carries.
	SecretsVolumeName = "app-secrets"
)

// SecretMount is one secret KEY projected into a file, for one app in one environment (ADR-0089 §1).
// It carries a key name and a filename and NEVER a value: the value lives only in the per-app
// Kubernetes Secret, and a mount is a KeyToPath reference to it.
type SecretMount struct {
	App         string
	Environment string
	// Key is the key in the app's per-app Secret. It must already exist when the mount is made.
	Key string
	// Filename is the name the key lands under in the app's secrets directory. It defaults to the
	// key and is validated as a single path segment.
	Filename string
	// NoEnv marks the key FILE-ONLY (ADR-0089 §4): projected into the file above, and kept out of the
	// container's environment. It is the answer for a credential whose whole reason for being on disk
	// is to stay out of /proc/self/environ, where every child process inherits it.
	//
	// It is a field of the MOUNT, and that placement is the rule "a key can be file-only only if it
	// is mounted" expressed in the type: there is no row here without a projection, so there is
	// nowhere to record a key that reaches the app by no route at all. Unmounting takes the marking
	// with the file and puts the variable back.
	NoEnv     bool
	UpdatedAt time.Time
}

// SecretMounts is an app's whole file projection in one environment: the directory its mounted keys
// land in, and the mounts themselves, sorted by key so every render of the pod template is
// byte-identical.
//
// The directory is a field of the SET rather than of a mount, which is ADR-0089 §2's per-app rule
// expressed in the type: there is nowhere to put a per-key path, so no code path can arrive at the
// subPath mount the kubelet will not update in place.
type SecretMounts struct {
	// Dir is the operator's override, empty when the app uses the default. Read it through
	// Directory rather than directly.
	Dir    string
	Mounts []SecretMount
}

// Directory is where this app's mounted keys land: the override, or DefaultSecretsDir.
func (m SecretMounts) Directory() string {
	if m.Dir == "" {
		return DefaultSecretsDir
	}
	return m.Dir
}

// Any reports whether the app projects anything as a file. An app with no mount gets no volume, no
// volume mount and no BURROW_SECRETS_DIR, so its pod template is bit-for-bit what it was before this
// existed.
func (m SecretMounts) Any() bool { return len(m.Mounts) > 0 }

// Keys returns the mounted key names, sorted — the audit- and log-safe view of a projection, since a
// key name is the most this whole path ever holds.
func (m SecretMounts) Keys() []string {
	keys := make([]string, 0, len(m.Mounts))
	for _, mount := range m.Mounts {
		keys = append(keys, mount.Key)
	}
	return keys
}

// FileOnly returns the mounted keys that are kept out of the app's environment, sorted (ADR-0089
// §4). It is the set the enumerated pod template subtracts from the app's Secret keys, and — because
// the mounts are sorted — the same projection always yields the same list and so the same template.
func (m SecretMounts) FileOnly() []string {
	var keys []string
	for _, mount := range m.Mounts {
		if mount.NoEnv {
			keys = append(keys, mount.Key)
		}
	}
	return keys
}

// AnyFileOnly reports whether this app has left the envFrom fast path (ADR-0089 §4). It is the one
// switch: false means the pod template sources the Secret wholesale, exactly as it did before any of
// this existed, and true means it enumerates a secretKeyRef per key that is still in the
// environment. Nearly every app answers false and must not pay for the ones that do not.
func (m SecretMounts) AnyFileOnly() bool {
	for _, mount := range m.Mounts {
		if mount.NoEnv {
			return true
		}
	}
	return false
}

// Sort orders the mounts by key. The store returns them sorted; this is for the callers that build a
// set themselves.
func (m *SecretMounts) Sort() {
	slices.SortFunc(m.Mounts, func(a, b SecretMount) int { return strings.Compare(a.Key, b.Key) })
}

// ValidateSecretFilename checks that name is a single path segment (ADR-0089 §1). A filename is the
// last thing between a mount and the app's own filesystem, so it may not contain a separator and may
// not be a directory traversal: `..` in a projected path is how a mount escapes the directory Burrow
// owns, which is the property everything else here rests on.
//
// Secret keys already match ^[A-Za-z_][A-Za-z0-9_]*$ (ADR-0028), so the DEFAULT filename is always
// legal and this only ever refuses something the caller typed at `--filename`.
func ValidateSecretFilename(name string) error {
	switch {
	case name == "":
		return fmt.Errorf("filename is empty")
	case strings.ContainsRune(name, '/'):
		return fmt.Errorf("filename %q contains a path separator: a mounted secret is one file in the app's secrets directory, not a path under it", name)
	case name == "." || name == "..":
		return fmt.Errorf("filename %q is a directory reference, not a file name", name)
	}
	return nil
}

// ValidateSecretsDir checks a `--dir` override (ADR-0089 §2). It must be an absolute, already-clean
// path and it may not be the container's root: Burrow mounts a VOLUME OVER this directory, so
// whatever the image had there is hidden, and mounting over `/` hides the image entirely.
func ValidateSecretsDir(dir string) error {
	switch {
	case dir == "":
		return nil // no override; the app uses DefaultSecretsDir
	case !strings.HasPrefix(dir, "/"):
		return fmt.Errorf("secrets directory %q is not absolute", dir)
	case dir == "/":
		return fmt.Errorf("secrets directory may not be %q: Burrow mounts a volume over this directory, which would hide the whole image", dir)
	case path.Clean(dir) != dir:
		return fmt.Errorf("secrets directory %q is not a clean path (write it as %q)", dir, path.Clean(dir))
	}
	return nil
}

// SecretMounts returns the keys an app projects as files in one environment, and the directory they
// land in (ADR-0089 §1). It is a read of the control plane's own record: keys and filenames, never a
// value.
func (e *Engine) SecretMounts(ctx context.Context, app, env string) (SecretMounts, error) {
	if err := (App{Name: app}).Validate(); err != nil {
		return SecretMounts{}, fmt.Errorf("secret mounts: %w: %w", ErrInvalid, err)
	}
	if _, err := e.resolveNamespace(ctx, env); err != nil {
		return SecretMounts{}, fmt.Errorf("secret mounts %s: %w", app, err)
	}
	mounts, err := e.db.SecretMounts(ctx, app, envName(env))
	if err != nil {
		return SecretMounts{}, fmt.Errorf("secret mounts %s: %w", app, err)
	}
	return mounts, nil
}

// MountSecret projects one of an app's secret keys into a file and re-applies the running workload
// so the file reaches its pods (ADR-0089 §1). filename defaults to the key; dir, when non-empty,
// moves the whole app's secrets directory (§2) — it is per app on purpose and there is no per-key
// form of it.
//
// noEnv marks the key FILE-ONLY (§4), and it is a POINTER because "leave it as it is" is a different
// answer from "put the variable back". A caller re-mounting a key to rename its file, or an agent
// re-running a mount it is not sure took, must not silently return a credential to an environment
// somebody deliberately took it out of. nil keeps whatever the key already has, true takes the
// variable away, false gives it back.
//
// THE KEY MUST ALREADY EXIST, or this refuses. Mounting a key that was never set produces an app
// that starts, finds no file, and fails at the moment it needs the credential — which is the failure
// this whole record exists to avoid making easy. The check reads KEY NAMES from the Secret and never
// a value.
//
// It is ungated (§7). Config and secret mutation carry no guardrail today, and the wrong verb to
// invent one for is the single verb that makes a credential SAFER: mounting a key the app already
// holds as an environment variable moves a credential it can already read from one place it can read
// it to another.
func (e *Engine) MountSecret(ctx context.Context, app, env, key, filename, dir string, noEnv *bool) (SecretMounts, error) {
	if err := (App{Name: app}).Validate(); err != nil {
		return SecretMounts{}, fmt.Errorf("mount secret: %w: %w", ErrInvalid, err)
	}
	if err := validateEnvKey(key); err != nil {
		return SecretMounts{}, fmt.Errorf("mount secret %s: %w: %w", app, ErrInvalid, err)
	}
	if filename == "" {
		filename = key
	}
	if err := ValidateSecretFilename(filename); err != nil {
		return SecretMounts{}, fmt.Errorf("mount secret %s: %w: %w", app, ErrInvalid, err)
	}
	if err := ValidateSecretsDir(dir); err != nil {
		return SecretMounts{}, fmt.Errorf("mount secret %s: %w: %w", app, ErrInvalid, err)
	}
	ns, err := e.resolveMutatingNamespace(ctx, env)
	if err != nil {
		return SecretMounts{}, fmt.Errorf("mount secret %s: %w", app, err)
	}
	k := e.k8s.WithNamespace(ns)
	keys, err := k.SecretKeys(ctx, app)
	if err != nil {
		return SecretMounts{}, fmt.Errorf("mount secret %s: reading the app's secret keys: %w", app, err)
	}
	if !slices.Contains(keys, key) {
		return SecretMounts{}, fmt.Errorf("mount secret %s: %w: %s is not set on %s; set it with `burrow app secret set %s %s` first, or the app would start and only fail when it opened the file",
			app, ErrInvalid, key, app, app, key)
	}
	// Resolve the file-only marking against what the key already has, so a mount that says nothing
	// about the environment changes nothing about it.
	fileOnly, err := e.fileOnly(ctx, app, env, key, noEnv)
	if err != nil {
		return SecretMounts{}, fmt.Errorf("mount secret %s: %w", app, err)
	}
	if dir != "" {
		if err := e.db.SetSecretsDir(ctx, app, envName(env), dir, e.clock.Now()); err != nil {
			return SecretMounts{}, fmt.Errorf("mount secret %s: recording the secrets directory: %w", app, err)
		}
	}
	m := SecretMount{App: app, Environment: envName(env), Key: key, Filename: filename, NoEnv: fileOnly, UpdatedAt: e.clock.Now()}
	if err := e.db.SetSecretMount(ctx, m); err != nil {
		return SecretMounts{}, fmt.Errorf("mount secret %s: recording the mount of %s: %w", app, key, err)
	}
	return e.applySecretMounts(ctx, ns, "mount secret", app, env)
}

// fileOnly answers whether key ends up marked file-only, given what the caller asked for and what
// the key already is (ADR-0089 §4). An explicit request wins; nil is "leave it alone", which for a
// key that was never mounted is false — the variable stays, which is the default the record states.
func (e *Engine) fileOnly(ctx context.Context, app, env, key string, noEnv *bool) (bool, error) {
	if noEnv != nil {
		return *noEnv, nil
	}
	mounts, err := e.secretMountsFor(ctx, app, envName(env))
	if err != nil {
		return false, err
	}
	for _, m := range mounts.Mounts {
		if m.Key == key {
			return m.NoEnv, nil
		}
	}
	return false, nil
}

// UnmountSecret stops projecting one key as a file and re-applies the running workload so the file
// leaves its pods (ADR-0089 §1). The VALUE is untouched: the key stays in the Secret and, because a
// file-only marking is a field of the mount, it goes back into the app's environment along with the
// file leaving — so unmounting is not a way to lose a credential, even one that was file-only.
// Unmounting a key that was not mounted is not an error — it is the state the app is already in.
func (e *Engine) UnmountSecret(ctx context.Context, app, env, key string) (SecretMounts, error) {
	if err := (App{Name: app}).Validate(); err != nil {
		return SecretMounts{}, fmt.Errorf("unmount secret: %w: %w", ErrInvalid, err)
	}
	if err := validateEnvKey(key); err != nil {
		return SecretMounts{}, fmt.Errorf("unmount secret %s: %w: %w", app, ErrInvalid, err)
	}
	ns, err := e.resolveMutatingNamespace(ctx, env)
	if err != nil {
		return SecretMounts{}, fmt.Errorf("unmount secret %s: %w", app, err)
	}
	if err := e.db.UnsetSecretMount(ctx, app, envName(env), key); err != nil {
		return SecretMounts{}, fmt.Errorf("unmount secret %s: removing the mount of %s: %w", app, key, err)
	}
	return e.applySecretMounts(ctx, ns, "unmount secret", app, env)
}

// applySecretMounts rolls the running workload after a mount write and returns the resulting set.
// A failure IS returned rather than logged: the caller asked for this directly, so "it is recorded
// but did not reach your pods" is the answer they need. An app with no running release simply
// persists — the mount lands on the next deploy, which is what reapplyWorkload's false return means.
//
// A mount changes the POD TEMPLATE (a volume, a volume mount, one env var), so unlike `secret set`
// there is no annotation to bump: the apply itself is what rolls the workload.
func (e *Engine) applySecretMounts(ctx context.Context, ns, op, app, env string) (SecretMounts, error) {
	if _, err := e.reapplyWorkload(ctx, e.k8s.WithNamespace(ns), op, app, envName(env)); err != nil {
		return SecretMounts{}, err
	}
	mounts, err := e.db.SecretMounts(ctx, app, envName(env))
	if err != nil {
		return SecretMounts{}, fmt.Errorf("%s %s: %w", op, app, err)
	}
	return mounts, nil
}

// secretMountsFor is the read every path that authors a workload makes — deploy, env reapply,
// rollback — so the files on a rolled-back app are the files a redeploy would have given it. It is
// the same shape as the readiness read next to it, and for the same reason (ADR-0089 §5).
func (e *Engine) secretMountsFor(ctx context.Context, app, env string) (SecretMounts, error) {
	mounts, err := e.db.SecretMounts(ctx, app, env)
	if err != nil {
		return SecretMounts{}, fmt.Errorf("reading the app's secret mounts: %w", err)
	}
	return mounts, nil
}

// secretProjectionFor is the whole answer to "how does this app's Secret reach its pods" for one
// apply: which keys are FILES, and which keys are still ENVIRONMENT VARIABLES. Every path that
// authors a workload asks it, so a rolled-back app gets the files and the variables a redeploy would
// have given it (ADR-0089 §5).
//
// THE SECOND RETURN IS EMPTY FOR NEARLY EVERY APP, and that is the point. Without a file-only key
// the pod template sources the Secret wholesale through envFrom, so there is nothing to enumerate
// and this makes no extra call — an app that never asked for any of this pays neither a template
// change nor a read.
//
// The keys come from the CLUSTER because the Secret is where they live: Postgres records which keys
// are projected as files and never what the Secret holds (ADR-0029). A key that is set and not
// file-only is an environment variable, which is exactly what envFrom would have made it.
func (e *Engine) secretProjectionFor(ctx context.Context, k Kubernetes, app, env string) (SecretMounts, []string, error) {
	mounts, err := e.secretMountsFor(ctx, app, env)
	if err != nil {
		return SecretMounts{}, nil, err
	}
	if !mounts.AnyFileOnly() {
		return mounts, nil, nil
	}
	keys, err := k.SecretKeys(ctx, app)
	if err != nil {
		return SecretMounts{}, nil, fmt.Errorf("reading the app's secret keys: %w", err)
	}
	fileOnly := mounts.FileOnly()
	envKeys := make([]string, 0, len(keys))
	for _, key := range keys {
		if !slices.Contains(fileOnly, key) {
			envKeys = append(envKeys, key)
		}
	}
	return mounts, envKeys, nil
}

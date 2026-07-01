// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package fake

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/burrow-cloud/burrow/controlplane"
)

var _ controlplane.Kubernetes = (*Kubernetes)(nil)

// Kubernetes is an in-memory controlplane.Kubernetes. Applied workloads are stored and
// inspectable; by default a workload is healthy (ready == desired) immediately, and
// tests can override readiness (SetReady) and seed logs (SetLogs) to model partial or
// failed rollouts. Errors can be injected per operation with SetError.
//
// Per-app resources (workloads, exposures, per-app Secrets) are keyed by namespace so the fake can
// model namespace-per-environment (ADR-0035 phase 2): WithNamespace returns a view whose app
// operations land under a different namespace, sharing the same backing maps and lock. The
// introspection helpers (Spec, SecretValue, …) read the receiver view's namespace; the
// namespace-qualified variants (SpecInNamespace, SecretValueInNamespace) read a named one.
type Kubernetes struct {
	mu           *sync.Mutex
	ns           string // the namespace this view's per-app operations act in
	base         string // the namespace treated as the default (unprefixed) one
	deploys      map[string]*deployState
	exposed      map[string]controlplane.ExposeSpec
	addresses    map[string]string // app -> ingress external address (controller-assigned)
	certReady    map[string]bool   // app -> whether the requested TLS certificate has been issued
	addons       map[string]controlplane.AddonInfo
	secrets      map[string]map[string]string          // app -> per-app Secret (key -> value)
	autoscalers  map[string]controlplane.AutoscaleSpec // app -> applied HPA spec (namespace-keyed)
	backups      *[]backupCall                         // RunBackupJob calls, in order
	restores     *[]backupCall                         // RunRestoreJob calls, in order
	backupSiz    *int64                                // size RunBackupJob reports
	metricsAvail *bool                                 // whether metrics-server is reported present
	errs         map[Op]error
}

// fakeBaseNamespace is the namespace the fake treats as the default: app resources in it are keyed
// by the bare app name, so a fake driven through the default environment behaves exactly as it did
// before namespace-per-environment, and existing tests that introspect by app name keep working. It
// matches the engine and adapter default app namespace ("default").
const fakeBaseNamespace = "default"

// key namespace-qualifies app for this view: the base (default) namespace keys by the bare app name,
// any other namespace prefixes it, so a named environment's resources are stored separately.
func (k *Kubernetes) key(app string) string { return nsKey(k.ns, k.base, app) }

func nsKey(ns, base, app string) string {
	if ns == "" || ns == base {
		return app
	}
	return ns + "/" + app
}

// appInNamespace reports whether the stored key nk belongs to this view's namespace and, if so, the
// bare app name. It is the inverse of key, used by the per-namespace ListWorkloads.
func (k *Kubernetes) appInNamespace(nk string) (string, bool) {
	if k.ns == "" || k.ns == k.base {
		// The default namespace keys by the bare app name (no "/"); a "ns/app" key is elsewhere.
		if strings.Contains(nk, "/") {
			return "", false
		}
		return nk, true
	}
	prefix := k.ns + "/"
	if !strings.HasPrefix(nk, prefix) {
		return "", false
	}
	return nk[len(prefix):], true
}

// WithNamespace returns a view of the fake whose per-app operations act in ns, sharing the same
// backing state and lock (ADR-0035 phase 2). Add-on operations are unaffected.
func (k *Kubernetes) WithNamespace(ns string) controlplane.Kubernetes {
	v := *k
	v.ns = ns
	return &v
}

// backupCall records one RunBackupJob/RunRestoreJob invocation so a test can assert the engine
// drove the in-cluster Job with the right app and backup id.
type backupCall struct {
	App      string
	BackupID string
}

type deployState struct {
	spec        controlplane.WorkloadSpec
	ready       int32
	logs        []controlplane.LogLine
	restartedAt time.Time // last RestartWorkload timestamp; zero until rolled
}

// NewKubernetes returns an empty fake cluster. metrics-server is reported present by default (the
// common case, where an applied HPA scales); a test models a cluster without it via
// SetMetricsAvailable(false).
func NewKubernetes() *Kubernetes {
	metricsAvail := true
	return &Kubernetes{
		mu:           &sync.Mutex{},
		ns:           fakeBaseNamespace,
		base:         fakeBaseNamespace,
		deploys:      make(map[string]*deployState),
		exposed:      make(map[string]controlplane.ExposeSpec),
		addresses:    make(map[string]string),
		certReady:    make(map[string]bool),
		addons:       make(map[string]controlplane.AddonInfo),
		secrets:      make(map[string]map[string]string),
		autoscalers:  make(map[string]controlplane.AutoscaleSpec),
		backups:      &[]backupCall{},
		restores:     &[]backupCall{},
		backupSiz:    new(int64),
		metricsAvail: &metricsAvail,
		errs:         make(map[Op]error),
	}
}

// SetSecret seeds app's per-app Secret with key=value, modelling a `secret set` done over the
// kubeconfig path (which never goes through this engine seam). Tests use it to set up list/unset.
func (k *Kubernetes) SetSecret(app, key, value string) {
	k.mu.Lock()
	defer k.mu.Unlock()
	nk := k.key(app)
	if k.secrets[nk] == nil {
		k.secrets[nk] = map[string]string{}
	}
	k.secrets[nk][key] = value
}

// SecretValue returns the stored value under key for app in this view's namespace and whether it is
// present — test-only introspection (the real seam never exposes values).
func (k *Kubernetes) SecretValue(app, key string) (string, bool) {
	k.mu.Lock()
	defer k.mu.Unlock()
	v, ok := k.secrets[k.key(app)][key]
	return v, ok
}

// SecretValueInNamespace is SecretValue scoped to a named namespace, so a test can assert a per-env
// secret landed in the environment's namespace (ADR-0035 phase 2).
func (k *Kubernetes) SecretValueInNamespace(ns, app, key string) (string, bool) {
	k.mu.Lock()
	defer k.mu.Unlock()
	v, ok := k.secrets[nsKey(ns, k.base, app)][key]
	return v, ok
}

// RestartedAt returns the last RestartWorkload timestamp for app and whether the workload was
// ever rolled by a restart bump.
func (k *Kubernetes) RestartedAt(app string) (time.Time, bool) {
	k.mu.Lock()
	defer k.mu.Unlock()
	d := k.deploys[k.key(app)]
	if d == nil || d.restartedAt.IsZero() {
		return time.Time{}, false
	}
	return d.restartedAt, true
}

func (k *Kubernetes) DeployAddon(ctx context.Context, spec controlplane.AddonSpec) (controlplane.AddonInfo, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	name := "burrow-" + string(spec.Type)
	info := controlplane.AddonInfo{
		Name:         name,
		Type:         spec.Type,
		Mode:         "installed",
		Backend:      spec.Backend,
		Image:        spec.Image,
		Endpoint:     fmt.Sprintf("%s.default.svc:%d", name, spec.Port),
		Capabilities: spec.Capabilities,
		Ready:        true,
	}
	k.addons[name] = info
	return info, nil
}

// AddonReady reports whether the named add-on was deployed (and thus ready) in this fake
// cluster. A deployed add-on is ready by default; an unknown one is not ready.
func (k *Kubernetes) AddonReady(ctx context.Context, name string) (bool, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if err := k.errs[OpAddonReady]; err != nil {
		return false, err
	}
	_, ok := k.addons[name]
	return ok, nil
}

func (k *Kubernetes) DeleteAddon(ctx context.Context, name string) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	if _, ok := k.addons[name]; !ok {
		return fmt.Errorf("fake: addon %q: %w", name, controlplane.ErrNotFound)
	}
	delete(k.addons, name)
	return nil
}

// Exposure returns the recorded exposure for app and whether one exists.
func (k *Kubernetes) Exposure(app string) (controlplane.ExposeSpec, bool) {
	k.mu.Lock()
	defer k.mu.Unlock()
	s, ok := k.exposed[k.key(app)]
	return s, ok
}

// SetIngressAddress sets the controller-assigned external address reported for app's
// exposure, modelling the ingress controller having processed the Ingress.
func (k *Kubernetes) SetIngressAddress(app, addr string) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.addresses[k.key(app)] = addr
}

// SetCertReady sets whether the requested TLS certificate reported for app's exposure has been
// issued, modelling cert-manager having populated the certificate Secret.
func (k *Kubernetes) SetCertReady(app string, ready bool) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.certReady[k.key(app)] = ready
}

func (k *Kubernetes) ExposureStatus(ctx context.Context, app string) (controlplane.ExposureStatus, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if err := k.errs[OpExposureStatus]; err != nil {
		return controlplane.ExposureStatus{}, err
	}
	nk := k.key(app)
	spec, ok := k.exposed[nk]
	if !ok {
		return controlplane.ExposureStatus{}, nil
	}
	return controlplane.ExposureStatus{Exposed: true, Host: spec.Host, Address: k.addresses[nk], TLS: spec.TLS, CertReady: k.certReady[nk]}, nil
}

// SetError makes op return err until cleared with SetError(op, nil).
func (k *Kubernetes) SetError(op Op, err error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if err == nil {
		delete(k.errs, op)
		return
	}
	k.errs[op] = err
}

// SetReady overrides the ready replica count for app, modelling a partial rollout. It
// is a no-op if app has no workload.
func (k *Kubernetes) SetReady(app string, ready int32) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if d := k.deploys[k.key(app)]; d != nil {
		d.ready = ready
	}
}

// SetLogs replaces the stored log lines for app.
func (k *Kubernetes) SetLogs(app string, lines []controlplane.LogLine) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if d := k.deploys[k.key(app)]; d != nil {
		d.logs = append([]controlplane.LogLine(nil), lines...)
	}
}

// Spec returns the currently applied spec for app in this view's namespace and whether a workload
// exists.
func (k *Kubernetes) Spec(app string) (controlplane.WorkloadSpec, bool) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if d := k.deploys[k.key(app)]; d != nil {
		return d.spec, true
	}
	return controlplane.WorkloadSpec{}, false
}

// SpecInNamespace is Spec scoped to a named namespace, so a test can assert a deploy routed to an
// environment's namespace (ADR-0035 phase 2).
func (k *Kubernetes) SpecInNamespace(ns, app string) (controlplane.WorkloadSpec, bool) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if d := k.deploys[nsKey(ns, k.base, app)]; d != nil {
		return d.spec, true
	}
	return controlplane.WorkloadSpec{}, false
}

func (k *Kubernetes) ApplyWorkload(ctx context.Context, spec controlplane.WorkloadSpec) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	if err := k.errs[OpApply]; err != nil {
		return err
	}
	nk := k.key(spec.App)
	d := k.deploys[nk]
	if d == nil {
		d = &deployState{}
		k.deploys[nk] = d
	}
	d.spec = spec
	d.ready = spec.Replicas // healthy by default
	return nil
}

func (k *Kubernetes) WorkloadStatus(ctx context.Context, app string) (controlplane.WorkloadStatus, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if err := k.errs[OpStatus]; err != nil {
		return controlplane.WorkloadStatus{}, err
	}
	d := k.deploys[k.key(app)]
	if d == nil {
		return controlplane.WorkloadStatus{}, fmt.Errorf("kubernetes: workload %q: %w", app, controlplane.ErrNotFound)
	}
	return controlplane.WorkloadStatus{
		App:             app,
		Kind:            d.spec.Kind,
		Image:           d.spec.Image,
		DesiredReplicas: d.spec.Replicas,
		ReadyReplicas:   d.ready,
		UpdatedReplicas: d.ready,
		Available:       d.spec.Replicas > 0 && d.ready >= d.spec.Replicas,
	}, nil
}

func (k *Kubernetes) ListWorkloads(ctx context.Context) ([]controlplane.WorkloadStatus, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if err := k.errs[OpStatus]; err != nil {
		return nil, err
	}
	out := make([]controlplane.WorkloadStatus, 0, len(k.deploys))
	for nk, d := range k.deploys {
		app, ok := k.appInNamespace(nk)
		if !ok {
			continue // a workload in a different namespace; listing is per-namespace
		}
		out = append(out, controlplane.WorkloadStatus{
			App:             app,
			Kind:            d.spec.Kind,
			Image:           d.spec.Image,
			DesiredReplicas: d.spec.Replicas,
			ReadyReplicas:   d.ready,
			UpdatedReplicas: d.ready,
			Available:       d.spec.Replicas > 0 && d.ready >= d.spec.Replicas,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].App < out[j].App })
	return out, nil
}

func (k *Kubernetes) ScaleWorkload(ctx context.Context, app string, replicas int32) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	if err := k.errs[OpScale]; err != nil {
		return err
	}
	d := k.deploys[k.key(app)]
	if d == nil {
		return fmt.Errorf("kubernetes: workload %q: %w", app, controlplane.ErrNotFound)
	}
	d.spec.Replicas = replicas
	d.ready = replicas
	return nil
}

// SetMetricsAvailable sets whether MetricsAPIAvailable reports metrics-server present, modelling a
// cluster with or without it. It shares the flag across namespace views (like the other pointer
// state), so a test sets it once on the base fake.
func (k *Kubernetes) SetMetricsAvailable(available bool) {
	k.mu.Lock()
	defer k.mu.Unlock()
	*k.metricsAvail = available
}

// Autoscaler returns the applied HPA spec for app in this view's namespace and whether one exists —
// test-only introspection of ApplyAutoscaler/DeleteAutoscaler.
func (k *Kubernetes) Autoscaler(app string) (controlplane.AutoscaleSpec, bool) {
	k.mu.Lock()
	defer k.mu.Unlock()
	spec, ok := k.autoscalers[k.key(app)]
	return spec, ok
}

func (k *Kubernetes) ApplyAutoscaler(ctx context.Context, app string, spec controlplane.AutoscaleSpec) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	if err := k.errs[OpApplyAutoscaler]; err != nil {
		return err
	}
	k.autoscalers[k.key(app)] = spec
	return nil
}

func (k *Kubernetes) DeleteAutoscaler(ctx context.Context, app string) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	if err := k.errs[OpDeleteAutoscaler]; err != nil {
		return err
	}
	delete(k.autoscalers, k.key(app)) // missing HPA is a no-op: idempotent
	return nil
}

func (k *Kubernetes) MetricsAPIAvailable(ctx context.Context) (bool, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if err := k.errs[OpMetricsAPIAvailable]; err != nil {
		return false, err
	}
	return *k.metricsAvail, nil
}

func (k *Kubernetes) Logs(ctx context.Context, app string, opts controlplane.LogOptions) ([]controlplane.LogLine, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if err := k.errs[OpLogs]; err != nil {
		return nil, err
	}
	d := k.deploys[k.key(app)]
	if d == nil {
		return nil, fmt.Errorf("kubernetes: workload %q: %w", app, controlplane.ErrNotFound)
	}
	lines := d.logs
	if opts.TailLines > 0 && len(lines) > opts.TailLines {
		lines = lines[len(lines)-opts.TailLines:]
	}
	return append([]controlplane.LogLine(nil), lines...), nil
}

func (k *Kubernetes) DeleteWorkload(ctx context.Context, app string) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	if err := k.errs[OpDelete]; err != nil {
		return err
	}
	nk := k.key(app)
	if _, ok := k.deploys[nk]; !ok {
		return fmt.Errorf("kubernetes: workload %q: %w", app, controlplane.ErrNotFound)
	}
	delete(k.deploys, nk)
	return nil
}

func (k *Kubernetes) Expose(ctx context.Context, spec controlplane.ExposeSpec) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	if err := k.errs[OpExpose]; err != nil {
		return err
	}
	k.exposed[k.key(spec.App)] = spec
	return nil
}

func (k *Kubernetes) Unexpose(ctx context.Context, app string) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	if err := k.errs[OpUnexpose]; err != nil {
		return err
	}
	nk := k.key(app)
	if _, ok := k.exposed[nk]; !ok {
		return fmt.Errorf("kubernetes: exposure %q: %w", app, controlplane.ErrNotFound)
	}
	delete(k.exposed, nk)
	return nil
}

// SetSecretValue upserts key=value into app's per-app Secret map (ADR-0029), modelling burrowd
// writing the value it received over the control-plane API. SecretKeys/SecretValue read the same
// map. An OpSetSecretValue error can be injected to exercise the failure path.
func (k *Kubernetes) SetSecretValue(ctx context.Context, app, key, value string) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	if err := k.errs[OpSetSecretValue]; err != nil {
		return err
	}
	nk := k.key(app)
	if k.secrets[nk] == nil {
		k.secrets[nk] = map[string]string{}
	}
	k.secrets[nk][key] = value
	return nil
}

func (k *Kubernetes) SecretKeys(ctx context.Context, app string) ([]string, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if err := k.errs[OpSecretKeys]; err != nil {
		return nil, err
	}
	sec := k.secrets[k.key(app)]
	keys := make([]string, 0, len(sec))
	for key := range sec {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys, nil
}

func (k *Kubernetes) UnsetSecretKey(ctx context.Context, app, key string) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	if err := k.errs[OpUnsetSecretKey]; err != nil {
		return err
	}
	delete(k.secrets[k.key(app)], key) // missing Secret/key is a no-op
	return nil
}

func (k *Kubernetes) RestartWorkload(ctx context.Context, app string, at time.Time) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	if err := k.errs[OpRestartWorkload]; err != nil {
		return err
	}
	d := k.deploys[k.key(app)]
	if d == nil {
		return fmt.Errorf("kubernetes: workload %q: %w", app, controlplane.ErrNotFound)
	}
	d.restartedAt = at
	return nil
}

// SetBackupSize sets the byte size RunBackupJob reports, modelling the dump container reporting it.
func (k *Kubernetes) SetBackupSize(n int64) {
	k.mu.Lock()
	defer k.mu.Unlock()
	*k.backupSiz = n
}

// BackupJobs returns the (app, backupID) pairs RunBackupJob was called with, in order.
func (k *Kubernetes) BackupJobs() []backupCall {
	k.mu.Lock()
	defer k.mu.Unlock()
	return append([]backupCall(nil), *k.backups...)
}

// RestoreJobs returns the (app, backupID) pairs RunRestoreJob was called with, in order.
func (k *Kubernetes) RestoreJobs() []backupCall {
	k.mu.Lock()
	defer k.mu.Unlock()
	return append([]backupCall(nil), *k.restores...)
}

func (k *Kubernetes) RunBackupJob(ctx context.Context, app, backupID string) (int64, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if err := k.errs[OpRunBackupJob]; err != nil {
		return 0, err
	}
	*k.backups = append(*k.backups, backupCall{App: app, BackupID: backupID})
	return *k.backupSiz, nil
}

func (k *Kubernetes) RunRestoreJob(ctx context.Context, app, backupID string) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	if err := k.errs[OpRunRestoreJob]; err != nil {
		return err
	}
	*k.restores = append(*k.restores, backupCall{App: app, BackupID: backupID})
	return nil
}

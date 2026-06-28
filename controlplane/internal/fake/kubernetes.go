// SPDX-License-Identifier: FSL-1.1-ALv2
// Copyright 2026 Nicholas Phillips

package fake

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/burrow-cloud/burrow/controlplane"
)

var _ controlplane.Kubernetes = (*Kubernetes)(nil)

// Kubernetes is an in-memory controlplane.Kubernetes. Applied workloads are stored and
// inspectable; by default a workload is healthy (ready == desired) immediately, and
// tests can override readiness (SetReady) and seed logs (SetLogs) to model partial or
// failed rollouts. Errors can be injected per operation with SetError.
type Kubernetes struct {
	mu        sync.Mutex
	deploys   map[string]*deployState
	exposed   map[string]controlplane.ExposeSpec
	addresses map[string]string // app -> ingress external address (controller-assigned)
	addons    map[string]controlplane.AddonInfo
	secrets   map[string]map[string]string // app -> per-app Secret (key -> value)
	errs      map[Op]error
}

type deployState struct {
	spec        controlplane.WorkloadSpec
	ready       int32
	logs        []controlplane.LogLine
	restartedAt time.Time // last RestartWorkload timestamp; zero until rolled
}

// NewKubernetes returns an empty fake cluster.
func NewKubernetes() *Kubernetes {
	return &Kubernetes{
		deploys:   make(map[string]*deployState),
		exposed:   make(map[string]controlplane.ExposeSpec),
		addresses: make(map[string]string),
		addons:    make(map[string]controlplane.AddonInfo),
		secrets:   make(map[string]map[string]string),
		errs:      make(map[Op]error),
	}
}

// SetSecret seeds app's per-app Secret with key=value, modelling a `secret set` done over the
// kubeconfig path (which never goes through this engine seam). Tests use it to set up list/unset.
func (k *Kubernetes) SetSecret(app, key, value string) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.secrets[app] == nil {
		k.secrets[app] = map[string]string{}
	}
	k.secrets[app][key] = value
}

// SecretValue returns the stored value under key for app and whether it is present — test-only
// introspection (the real seam never exposes values).
func (k *Kubernetes) SecretValue(app, key string) (string, bool) {
	k.mu.Lock()
	defer k.mu.Unlock()
	v, ok := k.secrets[app][key]
	return v, ok
}

// RestartedAt returns the last RestartWorkload timestamp for app and whether the workload was
// ever rolled by a restart bump.
func (k *Kubernetes) RestartedAt(app string) (time.Time, bool) {
	k.mu.Lock()
	defer k.mu.Unlock()
	d := k.deploys[app]
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
	s, ok := k.exposed[app]
	return s, ok
}

// SetIngressAddress sets the controller-assigned external address reported for app's
// exposure, modelling the ingress controller having processed the Ingress.
func (k *Kubernetes) SetIngressAddress(app, addr string) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.addresses[app] = addr
}

func (k *Kubernetes) ExposureStatus(ctx context.Context, app string) (controlplane.ExposureStatus, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if err := k.errs[OpExposureStatus]; err != nil {
		return controlplane.ExposureStatus{}, err
	}
	spec, ok := k.exposed[app]
	if !ok {
		return controlplane.ExposureStatus{}, nil
	}
	return controlplane.ExposureStatus{Exposed: true, Host: spec.Host, Address: k.addresses[app], TLS: spec.TLS}, nil
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
	if d := k.deploys[app]; d != nil {
		d.ready = ready
	}
}

// SetLogs replaces the stored log lines for app.
func (k *Kubernetes) SetLogs(app string, lines []controlplane.LogLine) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if d := k.deploys[app]; d != nil {
		d.logs = append([]controlplane.LogLine(nil), lines...)
	}
}

// Spec returns the currently applied spec for app and whether a workload exists.
func (k *Kubernetes) Spec(app string) (controlplane.WorkloadSpec, bool) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if d := k.deploys[app]; d != nil {
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
	d := k.deploys[spec.App]
	if d == nil {
		d = &deployState{}
		k.deploys[spec.App] = d
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
	d := k.deploys[app]
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
	for app, d := range k.deploys {
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
	d := k.deploys[app]
	if d == nil {
		return fmt.Errorf("kubernetes: workload %q: %w", app, controlplane.ErrNotFound)
	}
	d.spec.Replicas = replicas
	d.ready = replicas
	return nil
}

func (k *Kubernetes) Logs(ctx context.Context, app string, opts controlplane.LogOptions) ([]controlplane.LogLine, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if err := k.errs[OpLogs]; err != nil {
		return nil, err
	}
	d := k.deploys[app]
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
	if _, ok := k.deploys[app]; !ok {
		return fmt.Errorf("kubernetes: workload %q: %w", app, controlplane.ErrNotFound)
	}
	delete(k.deploys, app)
	return nil
}

func (k *Kubernetes) Expose(ctx context.Context, spec controlplane.ExposeSpec) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	if err := k.errs[OpExpose]; err != nil {
		return err
	}
	k.exposed[spec.App] = spec
	return nil
}

func (k *Kubernetes) Unexpose(ctx context.Context, app string) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	if err := k.errs[OpUnexpose]; err != nil {
		return err
	}
	if _, ok := k.exposed[app]; !ok {
		return fmt.Errorf("kubernetes: exposure %q: %w", app, controlplane.ErrNotFound)
	}
	delete(k.exposed, app)
	return nil
}

func (k *Kubernetes) SecretKeys(ctx context.Context, app string) ([]string, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if err := k.errs[OpSecretKeys]; err != nil {
		return nil, err
	}
	keys := make([]string, 0, len(k.secrets[app]))
	for key := range k.secrets[app] {
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
	delete(k.secrets[app], key) // missing Secret/key is a no-op
	return nil
}

func (k *Kubernetes) RestartWorkload(ctx context.Context, app string, at time.Time) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	if err := k.errs[OpRestartWorkload]; err != nil {
		return err
	}
	d := k.deploys[app]
	if d == nil {
		return fmt.Errorf("kubernetes: workload %q: %w", app, controlplane.ErrNotFound)
	}
	d.restartedAt = at
	return nil
}

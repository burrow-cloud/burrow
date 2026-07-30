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
	volumes      map[string]fakeVolume                 // claim name -> the add-on volume, present while the claim exists
	secrets      map[string]map[string]string          // app -> per-app Secret (key -> value)
	autoscalers  map[string]controlplane.AutoscaleSpec // app -> applied HPA spec (namespace-keyed)
	backups      *[]backupCall                         // RunBackupJob calls, in order
	restores     *[]backupCall                         // RunRestoreJob calls, in order
	runs         *[]runCall                            // RunJob calls, in order
	runResult    *controlplane.RunResult               // canned result RunJob returns
	backupSiz    *int64                                // size RunBackupJob reports
	backupReason *controlplane.BackupJobOutcome        // reason/detail RunBackupJob reports on failure
	metricsAvail *bool                                 // whether metrics-server is reported present
	errs         map[Op]error
}

// fakeBaseNamespace is the namespace the fake treats as the default: app resources in it are keyed
// by the bare app name, so a fake driven through the default environment behaves exactly as it did
// before namespace-per-environment, and existing tests that introspect by app name keep working. It
// matches the engine and adapter default app namespace ("default").
const fakeBaseNamespace = "default"

// fakeAddonNamespace is the namespace the fake reports add-on resources (and retained volumes) live
// in, mirroring the adapter's default add-on namespace.
const fakeAddonNamespace = "burrow-addons"

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

// fakeVolume is one PersistentVolumeClaim in the fake's add-on namespace. It carries what the real
// adapter reads off a claim's labels and capacity, so a test asserting on retained volumes asserts
// on the same shape production produces (ADR-0064 §6).
type fakeVolume struct {
	Addon controlplane.AddonType
	Role  string
	Size  string
}

// backupCall records one RunBackupJob/RunRestoreJob invocation so a test can assert the engine
// drove the in-cluster Job with the right app, ENVIRONMENT, and backup id. The environment is
// recorded because it selects the instance the dump is read from or written into (ADR-0067 §1) — a
// backup that named only the app could not be told apart from one taken against another
// environment's server.
type backupCall struct {
	App      string
	Env      string
	BackupID string
	// Dest is the object-storage destination the engine resolved for this backup, nil when none is
	// registered. A test asserts on it because "which destination did this backup go to" is the
	// question a Backup row must answer honestly (ADR-0063 §7), and the engine deciding it is where
	// that answer is settled.
	Dest *controlplane.BackupDestination
}

// runCall records one RunJob invocation so a test can assert the engine drove the one-off command
// Job with the right app, image, command, TTL, and namespace (ADR-0048).
type runCall struct {
	App        string
	Image      string
	Command    []string
	TTLSeconds int32
	Namespace  string
}

type deployState struct {
	spec        controlplane.WorkloadSpec
	ready       int32
	logs        []controlplane.LogLine
	restartedAt time.Time // last RestartWorkload timestamp; zero until rolled
	// issue is the injected blocking pod condition, in the same evidence shape the real adapter
	// reads off a pod (SetImagePullFailure/SetWedgedRollout/SetIssue); nil when healthy.
	issue *controlplane.IssueEvidence
}

// status builds the observed WorkloadStatus for this deploy state, mirroring the real adapter:
// availability comes from the ready/desired replicas, and an injected blocking pod condition
// (SetImagePullFailure/SetWedgedRollout/SetIssue) becomes the same actionable Issue the real
// adapter attaches — the SAME message, because both sides render it through
// controlplane.IssueEvidence.Message rather than each writing its own prose. It centralizes the
// shape so WorkloadStatus and ListWorkloads agree.
//
// A blocking condition forces the workload not-available and surfaces the Issue even when ready
// still meets desired: that is the wedged rolling update of issue #307, where the NEW release
// cannot pull its image while the PREVIOUS release's pods keep serving, so a naive ready>=desired
// check would wrongly read healthy. The real adapter reaches the same verdict by inspecting the
// updated pods; the fake models it directly.
func (d *deployState) status(app string) controlplane.WorkloadStatus {
	st := controlplane.WorkloadStatus{
		App:             app,
		Kind:            d.spec.Kind,
		Image:           d.spec.Image,
		DesiredReplicas: d.spec.Replicas,
		ReadyReplicas:   d.ready,
		UpdatedReplicas: d.ready,
		Available:       d.spec.Replicas > 0 && d.ready >= d.spec.Replicas,
	}
	if d.issue != nil {
		ev := *d.issue
		if ev.Image == "" {
			// An image-pull Issue names the CURRENTLY deployed image, so a redeploy after the
			// injection is reflected — the same thing the real adapter does by reading the image
			// off the pod each time it is asked.
			ev.Image = d.spec.Image
		}
		st.Available = false
		st.Issue = ev.Message()
		st.IssueReason = ev.Reason
	}
	return st
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
		volumes:      make(map[string]fakeVolume),
		secrets:      make(map[string]map[string]string),
		autoscalers:  make(map[string]controlplane.AutoscaleSpec),
		backups:      &[]backupCall{},
		restores:     &[]backupCall{},
		runs:         &[]runCall{},
		runResult:    &controlplane.RunResult{},
		backupSiz:    new(int64),
		backupReason: new(controlplane.BackupJobOutcome),
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

// DeployAddon models installing one add-on instance FOR ONE ENVIRONMENT: the instance is named by
// controlplane.AddonInstanceName, so the default environment lands on the unqualified name an
// existing install already has and any other environment gets a separate instance beside it
// (ADR-0067 §1). Two environments therefore occupy two entries in this fake cluster, never one.
func (k *Kubernetes) DeployAddon(ctx context.Context, spec controlplane.AddonSpec, env string) (controlplane.AddonInfo, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	name, err := controlplane.AddonInstanceName(spec.Type, env)
	if err != nil {
		return controlplane.AddonInfo{}, err
	}
	info := controlplane.AddonInfo{
		Name:         name,
		Type:         spec.Type,
		Environment:  env,
		Mode:         "installed",
		Backend:      spec.Backend,
		Image:        spec.Image,
		Endpoint:     fmt.Sprintf("%s.default.svc:%d", name, spec.Port),
		Capabilities: spec.Capabilities,
		Ready:        true,
	}
	k.addons[name] = info
	// A stateful add-on gets a data volume named after it, like the adapter's PVC. Whether that
	// volume survives a removal is the thing tests assert on, so the fake has to hold it.
	if spec.StorageGi > 0 {
		k.volumes[name] = fakeVolume{
			Addon: spec.Type,
			Role:  controlplane.AddonVolumeData,
			Size:  fmt.Sprintf("%dGi", spec.StorageGi),
		}
	}
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

// AddonVolume reports the data volume the named add-on still has in this fake cluster. It is how a
// test asserts the load-bearing property of removal: the volume survives a plain remove and only
// disappears when the caller explicitly asked for it (ADR-0025/0031).
func (k *Kubernetes) AddonVolume(name string) (string, bool) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if _, ok := k.volumes[name]; !ok {
		return "", false
	}
	return name, true
}

// AddonVolumes returns every claim this fake cluster still holds, exactly as the adapter reads them
// off the cluster: the ones whose add-on is gone are in here too, which is what makes a retained
// volume findable at all (ADR-0064 §6).
func (k *Kubernetes) AddonVolumes(ctx context.Context) ([]controlplane.AddonVolume, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if err := k.errs[OpAddonVolumes]; err != nil {
		return nil, err
	}
	out := make([]controlplane.AddonVolume, 0, len(k.volumes))
	for name, v := range k.volumes {
		out = append(out, controlplane.AddonVolume{
			Name:            name,
			Namespace:       fakeAddonNamespace,
			Addon:           v.Addon,
			Role:            v.Role,
			Size:            v.Size,
			ReinstallAdopts: v.Role == controlplane.AddonVolumeData,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// DeleteAddon models the real teardown: the add-on always goes, its data volume only when deleteData
// is set. The retained-volume names it reports mirror the adapter's, so an engine test asserts on the
// same shape production produces.
func (k *Kubernetes) DeleteAddon(ctx context.Context, name string, deleteData bool) (controlplane.AddonRemoval, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	removal := controlplane.AddonRemoval{Namespace: fakeAddonNamespace}
	info, ok := k.addons[name]
	if !ok {
		return removal, fmt.Errorf("fake: addon %q: %w", name, controlplane.ErrNotFound)
	}
	delete(k.addons, name)
	if _, hasVol := k.volumes[name]; hasVol {
		if deleteData {
			delete(k.volumes, name)
			removal.DataDeleted = true
		} else {
			removal.RetainedDataVolume = name
		}
	}
	// The backup volume outlives the database either way (ADR-0032). It exists in this fake only
	// once a backup has been taken, exactly as the adapter creates it on first backup.
	if info.Type == controlplane.AddonPostgres && len(*k.backups) > 0 {
		removal.RetainedBackupVolume = controlplane.PostgresBackupVolume
	}
	return removal, nil
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

// SetImagePullFailure models a workload whose pod cannot pull its image: it drops ready to 0 (so
// the workload is not Available) and records reason as the blocking pod waiting reason, so
// WorkloadStatus/ListWorkloads report the same actionable Issue the real adapter attaches on a
// genuine ImagePullBackOff. Pass a non-image-pull reason (or "") to clear it. It is a no-op if
// app has no workload.
func (k *Kubernetes) SetImagePullFailure(app, reason string) {
	k.setImagePullFailure(app, reason, "")
}

// SetImagePullFailureMessage is SetImagePullFailure with the kubelet's waiting message attached,
// so a test can drive the not-found vs unauthorized Issue distinction (ADR-0040).
func (k *Kubernetes) SetImagePullFailureMessage(app, reason, message string) {
	k.setImagePullFailure(app, reason, message)
}

func (k *Kubernetes) setImagePullFailure(app, reason, message string) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if d := k.deploys[k.key(app)]; d != nil {
		d.ready = 0
		d.issue = imagePullEvidence(reason, message)
	}
}

// imagePullEvidence builds the injected condition for the image-pull setters, or nil to clear it.
// They stay image-pull-ONLY now that the vocabulary is wider (ADR-0074 §2): passing any other
// reason has always meant "clear the failure" here, and a setter that silently started injecting a
// crash loop because a test passed "CrashLoopBackOff" to it would be a trap. SetIssue is the way to
// inject the other classes.
func imagePullEvidence(reason, message string) *controlplane.IssueEvidence {
	if !controlplane.IsImagePullReason(reason) {
		return nil
	}
	return &controlplane.IssueEvidence{Reason: reason, Detail: message}
}

// SetIssue injects an arbitrary blocking pod condition for app, in the same evidence shape the real
// adapter reads off a pod: the workload reports not-available carrying the message
// controlplane.IssueEvidence.Message renders for it — byte for byte the message production
// produces, since there is only one renderer (ADR-0074 §2). It is how a test drives an
// unschedulable pod, a crash loop, a missing config key, an OOM kill or an unavailable volume
// without a cluster.
//
// A zero IssueEvidence (or one whose Reason is outside the closed set) clears the condition. Unlike
// SetImagePullFailure it leaves the ready count alone, so a test can model either a wholly failed
// workload or a wedged rollout whose previous release still serves. It is a no-op if app has no
// workload.
func (k *Kubernetes) SetIssue(app string, ev controlplane.IssueEvidence) {
	k.mu.Lock()
	defer k.mu.Unlock()
	d := k.deploys[k.key(app)]
	if d == nil {
		return
	}
	if !controlplane.IsIssueReason(ev.Reason) {
		d.issue = nil
		return
	}
	d.issue = &ev
}

// SetWedgedRollout models issue #307: a NEW release whose image cannot be pulled while the
// PREVIOUS release's pods keep serving. Unlike SetImagePullFailure it leaves ready at its current
// (old-release) count, so a naive ready>=desired check still reads healthy; the blocking waiting
// reason is what marks the current release not-serving, exactly as a wedged rolling update looks in
// a real cluster. Pass a non-image-pull reason (or "") to clear it. It is a no-op if app has no
// workload.
func (k *Kubernetes) SetWedgedRollout(app, reason string) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if d := k.deploys[k.key(app)]; d != nil {
		d.issue = imagePullEvidence(reason, "")
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
	return d.status(app), nil
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
		out = append(out, d.status(app))
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

// SetAutoscalerActive seeds (or clears) an active HorizontalPodAutoscaler for app in this view's
// namespace, so a test can model an app the autoscaler owns without applying a full AutoscaleSpec
// through the engine. It shares the backing store with ApplyAutoscaler/DeleteAutoscaler, so both
// paths agree on what AutoscalerActive reports.
func (k *Kubernetes) SetAutoscalerActive(app string, active bool) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if active {
		k.autoscalers[k.key(app)] = controlplane.AutoscaleSpec{}
		return
	}
	delete(k.autoscalers, k.key(app))
}

// AutoscalerActive reports whether app has an applied HPA in this view's namespace — an entry set
// by ApplyAutoscaler or SetAutoscalerActive and not since deleted. A missing HPA is inactive
// (false, nil), mirroring the real adapter.
func (k *Kubernetes) AutoscalerActive(ctx context.Context, app string) (bool, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if err := k.errs[OpAutoscalerActive]; err != nil {
		return false, err
	}
	_, ok := k.autoscalers[k.key(app)]
	return ok, nil
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

// BackupJobs returns the (app, environment, backupID) triples RunBackupJob was called with, in order.
func (k *Kubernetes) BackupJobs() []backupCall {
	k.mu.Lock()
	defer k.mu.Unlock()
	return append([]backupCall(nil), *k.backups...)
}

// RestoreJobs returns the (app, environment, backupID) triples RunRestoreJob was called with, in order.
func (k *Kubernetes) RestoreJobs() []backupCall {
	k.mu.Lock()
	defer k.mu.Unlock()
	return append([]backupCall(nil), *k.restores...)
}

// SetBackupFailure sets the closed reason and detail RunBackupJob reports alongside its injected
// error, modelling the Job telling burrowd WHY a backup did not reach its destination (ADR-0063 §7).
func (k *Kubernetes) SetBackupFailure(reason, detail string) {
	k.mu.Lock()
	defer k.mu.Unlock()
	*k.backupReason = controlplane.BackupJobOutcome{Reason: reason, Detail: detail}
}

func (k *Kubernetes) RunBackupJob(ctx context.Context, app, env, backupID string, dest *controlplane.BackupDestination) (controlplane.BackupJobOutcome, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	*k.backups = append(*k.backups, backupCall{App: app, Env: env, BackupID: backupID, Dest: dest})
	if err := k.errs[OpRunBackupJob]; err != nil {
		// The call is recorded BEFORE the injected failure so a test can assert what the engine asked
		// for even on the path where the Job fails — which is the path this issue is about.
		return *k.backupReason, err
	}
	// The backup claim is created on first backup, exactly as the adapter creates it (ADR-0032), and
	// from then on it is a claim in the add-on namespace like any other — including after the add-on
	// that filled it is gone.
	k.volumes[controlplane.PostgresBackupVolume] = fakeVolume{
		Addon: controlplane.AddonPostgres,
		Role:  controlplane.AddonVolumeBackup,
		Size:  "10Gi",
	}
	return controlplane.BackupJobOutcome{SizeBytes: *k.backupSiz}, nil
}

func (k *Kubernetes) RunRestoreJob(ctx context.Context, app, env, backupID string) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	if err := k.errs[OpRunRestoreJob]; err != nil {
		return err
	}
	*k.restores = append(*k.restores, backupCall{App: app, Env: env, BackupID: backupID})
	return nil
}

// SetRunResult sets the canned RunResult the next RunJob calls return, modelling a command's captured
// output and exit code (ADR-0048). A test seeds a non-zero ExitCode to assert it surfaces as a
// structured result rather than a transport error.
func (k *Kubernetes) SetRunResult(res controlplane.RunResult) {
	k.mu.Lock()
	defer k.mu.Unlock()
	*k.runResult = res
}

// RunJobs returns the recorded RunJob invocations, in order, so a test can assert the engine drove
// the one-off command Job with the app's current image, command, TTL, and target namespace.
func (k *Kubernetes) RunJobs() []runCall {
	k.mu.Lock()
	defer k.mu.Unlock()
	return append([]runCall(nil), *k.runs...)
}

func (k *Kubernetes) RunJob(ctx context.Context, spec controlplane.RunSpec) (controlplane.RunResult, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if err := k.errs[OpRunJob]; err != nil {
		return controlplane.RunResult{}, err
	}
	*k.runs = append(*k.runs, runCall{App: spec.App, Image: spec.Image, Command: append([]string(nil), spec.Command...), TTLSeconds: spec.TTLSeconds, Namespace: k.ns})
	return *k.runResult, nil
}

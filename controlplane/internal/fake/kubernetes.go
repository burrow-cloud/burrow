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
	mu          *sync.Mutex
	ns          string // the namespace this view's per-app operations act in
	base        string // the namespace treated as the default (unprefixed) one
	deploys     map[string]*deployState
	exposed     map[string]controlplane.ExposeSpec
	addresses   map[string]string // app -> ingress external address (controller-assigned)
	certReady   map[string]bool   // app -> whether the requested TLS certificate has been issued
	addons      map[string]controlplane.AddonInfo
	volumes     map[string]fakeVolume                  // claim name -> the add-on volume, present while the claim exists
	secrets     map[string]map[string]string           // app -> per-app Secret (key -> value)
	autoscalers map[string]controlplane.AutoscaleSpec  // app -> applied HPA spec (namespace-keyed)
	backups     *[]backupCall                          // RunBackupJob calls, in order
	restores    *[]backupCall                          // RunRestoreJob calls, in order
	runs        *[]runCall                             // RunJob calls, in order
	rollouts    *[]rolloutCall                         // AwaitRollout calls, in order
	rolloutOut  map[string]controlplane.RolloutOutcome // app -> canned AwaitRollout answer, when overridden
	runResult   *controlplane.RunResult                // canned result RunJob returns
	// runJobHook, when set, is called by RunJob OUTSIDE the fake's lock, so a test can observe how
	// long a Job is in flight and whether two are ever in flight at once — the only way to assert
	// that the engine serializes lifecycle hooks (ADR-0072 §9) rather than the fake's own mutex
	// serializing them.
	runJobHook     *func()
	backupSiz      *int64                              // size RunBackupJob reports
	backupReason   *controlplane.BackupJobOutcome      // reason/detail RunBackupJob reports on failure
	physicals      *[]backupCall                       // RunPhysicalBackup calls, in order
	physicalLabel  *string                             // pgBackRest label RunPhysicalBackup reports
	physicalReason *controlplane.PhysicalBackupOutcome // reason/detail RunPhysicalBackup reports
	// instanceRestores records RestoreInstance calls, in order (ADR-0066 §4).
	instanceRestores *[]RestoreInstanceCall
	metricsAvail     *bool // whether metrics-server is reported present
	// backupJobs records which backups still have a Job in the fake add-on namespace. The real
	// adapter reads this off the cluster; here a test sets it, because the interesting state is the
	// one no code path produces on purpose — a Job that is GONE while its row is still pending
	// (ADR-0074 §6).
	backupJobs map[string]bool
	// physicalBackups is backupJobs' sibling for the `Backup` objects a physical backup creates, and
	// archiving records which instances have a pgBackRest repository behind them.
	physicalBackups map[string]bool
	archiving       map[string]string
	// watchers are the workload watches WatchWorkloads has established, by namespace (ADR-0079 §1).
	// A mutator that changes what a workload looks like reports it to the watch over its namespace,
	// which is what a real cluster does when a pod changes underneath one — so a test arranges the
	// cluster exactly as it always has and the events follow.
	watchers map[string]*workloadWatcher
	errs     map[Op]error
}

// workloadWatcher is one established watch: where to deliver, and the context that ends it. The
// context is held rather than watched by a goroutine so a cancelled watch stops delivering at the
// next emit, with no scheduling for a test to race.
type workloadWatcher struct {
	ctx    context.Context
	events chan<- controlplane.WorkloadEvent
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
	Env   string
	Role  string
	Size  string
}

// fakeEnvName resolves an unnamed environment to the default one, mirroring the engine's envName so
// a fake driven by a test that omits the environment lands where the real adapter would.
func fakeEnvName(env string) string {
	if env == "" {
		return controlplane.DefaultEnvironment
	}
	return env
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
	// Env is the container environment the Job was built with: the app's config, plus the BURROW_*
	// variables the phase tells the hook (ADR-0072 §4). A test asserts on it because "what is a post
	// hook actually told" is the whole question that phase exists to answer, and because a dependency
	// check must be given the app's own environment (ADR-0076 §4). No secret VALUE travels this way —
	// the Secret is projected by the real adapter, which is why no value is here.
	Env map[string]string
	// SecretFiles and SecretEnvKeys are the app's secret projection the Job was built with: which
	// keys it reads as files and which are still environment variables (ADR-0089 §4). They are key
	// NAMES and filenames, never a value. A test asserts on them because a Job that sourced the
	// Secret wholesale would put a key the app marked file-only back into an environment every child
	// process of the command inherits.
	SecretFiles   controlplane.SecretMounts
	SecretEnvKeys []string
	// Probe is the ADR-0076 §4 request to make Burrow's binary executable inside the app's image,
	// nil for an ordinary run. A test asserts the deploy-time dependency check asked for it and that
	// the plan it carried names key NAMES and addresses rather than values.
	Probe *controlplane.ProbeSpec
}

// rolloutCall records one AwaitRollout invocation so a test can assert a deploy waited for the
// rollout to settle before running a post hook, and waited with the bound the operational
// configuration resolved (ADR-0072 §5).
type rolloutCall struct {
	App       string
	Timeout   time.Duration
	Namespace string
}

type deployState struct {
	spec        controlplane.WorkloadSpec
	ready       int32
	logs        []controlplane.LogLine
	restartedAt time.Time // last RestartWorkload timestamp; zero until rolled
	// issue is the injected blocking pod condition, in the same evidence shape the real adapter
	// reads off a pod (SetImagePullFailure/SetWedgedRollout/SetIssue); nil when healthy.
	issue *controlplane.IssueEvidence
	// issueServing marks an injected condition that does NOT stop the workload serving — a container
	// that was killed and came back (issue #416). The real adapter attaches that Issue on the
	// ROLLED-OUT path and leaves availability exactly as the replica counts report it, so a fake that
	// forced not-available for every injected condition could not model the case at all. Set by
	// SetServingIssue and cleared by every other injector.
	issueServing bool
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
//
// A condition injected with SetServingIssue is the ONE exception, and it is the other half of the
// same principle: a container killed and restarted in place leaves a workload that really is serving
// (issue #416), so it carries the Issue with availability untouched. Both cases exist so that a bare
// "available" is never read as "nothing is happening" — they differ only in whether the app is up.
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
		if !d.issueServing {
			st.Available = false
		}
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
		mu:               &sync.Mutex{},
		ns:               fakeBaseNamespace,
		base:             fakeBaseNamespace,
		deploys:          make(map[string]*deployState),
		exposed:          make(map[string]controlplane.ExposeSpec),
		addresses:        make(map[string]string),
		certReady:        make(map[string]bool),
		addons:           make(map[string]controlplane.AddonInfo),
		volumes:          make(map[string]fakeVolume),
		secrets:          make(map[string]map[string]string),
		autoscalers:      make(map[string]controlplane.AutoscaleSpec),
		backups:          &[]backupCall{},
		restores:         &[]backupCall{},
		runs:             &[]runCall{},
		rollouts:         &[]rolloutCall{},
		rolloutOut:       make(map[string]controlplane.RolloutOutcome),
		runResult:        &controlplane.RunResult{},
		runJobHook:       new(func()),
		backupSiz:        new(int64),
		backupReason:     new(controlplane.BackupJobOutcome),
		metricsAvail:     &metricsAvail,
		backupJobs:       make(map[string]bool),
		physicalBackups:  make(map[string]bool),
		archiving:        make(map[string]string),
		physicals:        new([]backupCall),
		physicalLabel:    new(string),
		physicalReason:   new(controlplane.PhysicalBackupOutcome),
		instanceRestores: new([]RestoreInstanceCall),
		watchers:         make(map[string]*workloadWatcher),
		errs:             make(map[Op]error),
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
func (k *Kubernetes) DeployAddon(ctx context.Context, spec controlplane.AddonSpec, env string, archive *controlplane.ArchiveDestination) (controlplane.AddonInfo, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	// Whether the instance archives is remembered, because it is what decides whether a physical
	// backup of it is possible at all (ADR-0066 §3) — the fake cluster has to be able to be in both
	// states, since an install with no object-storage provider registered is the ordinary one.
	if spec.Type == controlplane.AddonPostgres {
		if n, err := controlplane.AddonInstanceName(spec.Type, env); err == nil {
			if archive != nil {
				k.archiving[n] = archive.Provider
			} else if _, ok := k.archiving[n]; !ok {
				k.archiving[n] = ""
			}
		}
	}
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
	// A stateful add-on gets a data volume, like the adapter's PVC. Whether that volume survives a
	// removal is the thing tests assert on, so the fake has to hold it — and it has to hold it under
	// the NAME the add-on's type gives it, because that is the name a removal has to find. Burrow's
	// own claim is named after the instance; a Postgres instance is a CloudNativePG `Cluster`, which
	// composes its claim and names it `<instance>-1`.
	if spec.StorageGi > 0 {
		k.volumes[controlplane.AddonDataVolumeName(spec.Type, name)] = fakeVolume{
			Addon: spec.Type,
			Env:   fakeEnvName(env),
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
			Environment:     v.Env,
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
//
// The add-on's TYPE decides which claim that is, and modelling it here is what makes an engine test
// of a Postgres removal mean something: the retained claim is the operator's `<instance>-1`, not
// `<instance>`, and a fake that used the Deployment name would agree with a removal that looked for
// the wrong volume (ADR-0066 §1, ADR-0064 §1).
func (k *Kubernetes) DeleteAddon(ctx context.Context, name string, t controlplane.AddonType, deleteData bool) (controlplane.AddonRemoval, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	removal := controlplane.AddonRemoval{Namespace: fakeAddonNamespace}
	info, ok := k.addons[name]
	if !ok {
		return removal, fmt.Errorf("fake: addon %q: %w", name, controlplane.ErrNotFound)
	}
	delete(k.addons, name)
	dataVolume := controlplane.AddonDataVolumeName(t, name)
	if _, hasVol := k.volumes[dataVolume]; hasVol {
		if deleteData {
			delete(k.volumes, dataVolume)
			removal.DataDeleted = true
		} else {
			removal.RetainedDataVolume = dataVolume
		}
	}
	// The backup volume outlives the database either way (ADR-0032), and it is THIS environment's
	// claim (ADR-0067 §1). It exists in this fake only once a backup has been taken in that
	// environment, exactly as the adapter creates it on first backup.
	if info.Type == controlplane.AddonPostgres {
		if claim, err := controlplane.BackupVolumeName(controlplane.AddonPostgres, fakeEnvName(info.Environment)); err == nil {
			if _, ok := k.volumes[claim]; ok {
				removal.RetainedBackupVolume = claim
			}
		}
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

// SetBackupJob marks whether the Job for a backup id exists in this fake cluster. A backup whose
// Job was reaped, or whose burrowd restarted mid-backup, is the case ADR-0074 §6 is about: the row
// stays pending and nothing is left to finish it.
func (k *Kubernetes) SetBackupJob(backupID string, present bool) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if present {
		k.backupJobs[backupID] = true
		return
	}
	delete(k.backupJobs, backupID)
}

// SetPhysicalBackup marks whether the `Backup` object for a backup id exists in this fake cluster —
// PhysicalBackupPresent's half of the ADR-0074 §6 sweep.
func (k *Kubernetes) SetPhysicalBackup(backupID string, present bool) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if present {
		k.physicalBackups[backupID] = true
		return
	}
	delete(k.physicalBackups, backupID)
}

// PhysicalBackupPresent reports whether the `Backup` object for a backup id still exists.
func (k *Kubernetes) PhysicalBackupPresent(ctx context.Context, backupID string) (bool, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if err := k.errs[OpPhysicalBackupPresent]; err != nil {
		return false, err
	}
	return k.physicalBackups[backupID], nil
}

// SetPhysicalBackupLabel sets pgBackRest's own backup label RunPhysicalBackup reports on success.
func (k *Kubernetes) SetPhysicalBackupLabel(label string) {
	k.mu.Lock()
	defer k.mu.Unlock()
	*k.physicalLabel = label
}

// SetPhysicalBackupFailure sets the closed reason and detail RunPhysicalBackup reports alongside its
// injected error, modelling a `Backup` object reaching a terminal phase (ADR-0066 §2).
func (k *Kubernetes) SetPhysicalBackupFailure(reason, detail string) {
	k.mu.Lock()
	defer k.mu.Unlock()
	*k.physicalReason = controlplane.PhysicalBackupOutcome{Reason: reason, Detail: detail}
}

// PhysicalBackups returns the (environment, backupID) pairs RunPhysicalBackup was called with.
func (k *Kubernetes) PhysicalBackups() []backupCall {
	k.mu.Lock()
	defer k.mu.Unlock()
	return append([]backupCall(nil), *k.physicals...)
}

// RunPhysicalBackup models asking CloudNativePG for a base backup of one environment's instance. It
// refuses an instance that does not archive, exactly as the adapter does: a `Backup` object against a
// `Cluster` with no plugin has nowhere to write.
func (k *Kubernetes) RunPhysicalBackup(ctx context.Context, env, backupID string, archive *controlplane.ArchiveDestination) (controlplane.PhysicalBackupOutcome, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	*k.physicals = append(*k.physicals, backupCall{Env: env, BackupID: backupID})
	if err := k.errs[OpRunPhysicalBackup]; err != nil {
		return *k.physicalReason, err
	}
	name, err := controlplane.AddonInstanceName(controlplane.AddonPostgres, fakeEnvName(env))
	if err != nil {
		return controlplane.PhysicalBackupOutcome{}, err
	}
	provider, installed := k.archiving[name]
	if !installed {
		return controlplane.PhysicalBackupOutcome{}, fmt.Errorf("fake: environment %q has no postgres instance to back up: %w", env, controlplane.ErrNotFound)
	}
	if provider == "" {
		return controlplane.PhysicalBackupOutcome{}, fmt.Errorf("fake: the postgres instance %q archives nowhere: %w", name, controlplane.ErrInvalid)
	}
	// The key comes from the destination the instance archives to, as the adapter derives it from the
	// instance's own Stanza.
	return controlplane.PhysicalBackupOutcome{
		Label:     *k.physicalLabel,
		ObjectKey: controlplane.PgBackRestManifestKey(archive.RepoPath, name, *k.physicalLabel),
	}, nil
}

// RestoreInstanceCall records one RestoreInstance invocation, so a test can assert what the engine
// asked the cluster to recover — the environment and the point — without reaching into the adapter.
// It deliberately does NOT record the ArchiveDestination: that value carries a credential, and a
// fake that stored one would be a fixture holding a secret (ADR-0063 §1).
type RestoreInstanceCall struct {
	Env         string
	BackupLabel string
	TargetTime  string
	Provider    string
}

// RestoreInstances returns the RestoreInstance calls, in order.
func (k *Kubernetes) RestoreInstances() []RestoreInstanceCall {
	k.mu.Lock()
	defer k.mu.Unlock()
	return append([]RestoreInstanceCall(nil), *k.instanceRestores...)
}

// RestoreInstance models rewinding an environment's whole Postgres instance from its repository
// (ADR-0066 §4). Like the adapter it refuses an instance that has no repository behind it, because
// there is nothing to recover from — and unlike a backup it does NOT refuse a missing instance: "the
// instance is gone" is the case a physical restore exists for.
func (k *Kubernetes) RestoreInstance(ctx context.Context, req controlplane.RestoreInstanceRequest) (controlplane.RestoreInstanceOutcome, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	provider := ""
	if req.Archive != nil {
		provider = req.Archive.Provider
	}
	*k.instanceRestores = append(*k.instanceRestores, RestoreInstanceCall{
		Env: req.Environment, BackupLabel: req.BackupLabel, TargetTime: req.TargetTime, Provider: provider,
	})
	if err := k.errs[OpRestoreInstance]; err != nil {
		return controlplane.RestoreInstanceOutcome{}, err
	}
	name, err := controlplane.AddonInstanceName(controlplane.AddonPostgres, fakeEnvName(req.Environment))
	if err != nil {
		return controlplane.RestoreInstanceOutcome{}, err
	}
	if k.archiving[name] == "" {
		return controlplane.RestoreInstanceOutcome{}, fmt.Errorf("fake: the postgres instance %q has no pgBackRest repository to recover from: %w", name, controlplane.ErrNotFound)
	}
	// The recovered instance comes up under the instance's OWN name, and the data claim of the one
	// that was there is destroyed to make room for it — the adapter's behaviour, modelled so a test
	// can assert that the environment still resolves to the same instance afterwards.
	claim := controlplane.AddonDataVolumeName(controlplane.AddonPostgres, name)
	destroyed := []string{}
	if _, ok := k.volumes[claim]; ok {
		delete(k.volumes, claim)
		destroyed = append(destroyed, claim)
	}
	return controlplane.RestoreInstanceOutcome{Instance: name, VolumesDestroyed: destroyed}, nil
}

// BackupJobPresent reports whether the Job for a backup id still exists. A missing Job is absent
// (false, nil), not an error, matching the adapter.
func (k *Kubernetes) BackupJobPresent(ctx context.Context, backupID string) (bool, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if err := k.errs[OpBackupJobPresent]; err != nil {
		return false, err
	}
	return k.backupJobs[backupID], nil
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

// WatchWorkloads establishes the watch over this view's namespace (ADR-0079 §1). The fake's re-list
// is instantaneous: every workload already there is reported, then the watch says it has a complete
// picture. From then on every mutator that changes what a workload looks like delivers an event, and
// a test drives the whole path by arranging the cluster exactly as it always has.
func (k *Kubernetes) WatchWorkloads(ctx context.Context, events chan<- controlplane.WorkloadEvent) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	if err := k.errs[OpWatchWorkloads]; err != nil {
		return err
	}
	k.watchers[k.ns] = &workloadWatcher{ctx: ctx, events: events}
	apps := make([]string, 0, len(k.deploys))
	for nk := range k.deploys {
		if app, ok := k.appInNamespace(nk); ok {
			apps = append(apps, app)
		}
	}
	sort.Strings(apps)
	for _, app := range apps {
		k.notify(app)
	}
	k.emit(controlplane.WorkloadEvent{Kind: controlplane.WorkloadSynced, Namespace: k.ns})
	return nil
}

// DropWorkloadWatch models this namespace's watch losing its place: the consumer is told, and
// nothing is delivered until SyncWorkloadWatch says the re-list completed. It is how a test drives
// ADR-0079 §4 — a gap that shows rather than an observer pretending continuity across it.
func (k *Kubernetes) DropWorkloadWatch(detail string) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.emit(controlplane.WorkloadEvent{Kind: controlplane.WorkloadDropped, Namespace: k.ns, Detail: detail})
}

// SyncWorkloadWatch models a completed re-list after a drop: current state for every workload in the
// namespace, then the signal that the picture is complete again. It reports current state and not
// what happened while the watch was away, which is exactly why the gap has to stay visible.
func (k *Kubernetes) SyncWorkloadWatch() {
	k.mu.Lock()
	defer k.mu.Unlock()
	apps := make([]string, 0, len(k.deploys))
	for nk := range k.deploys {
		if app, ok := k.appInNamespace(nk); ok {
			apps = append(apps, app)
		}
	}
	sort.Strings(apps)
	for _, app := range apps {
		k.notify(app)
	}
	k.emit(controlplane.WorkloadEvent{Kind: controlplane.WorkloadSynced, Namespace: k.ns})
}

// notify reports app's current derived state to this view's watch. It is called with the lock held
// by every mutator that changes what a workload looks like.
func (k *Kubernetes) notify(app string) {
	d := k.deploys[k.key(app)]
	if d == nil {
		k.emit(controlplane.WorkloadEvent{
			Kind:      controlplane.WorkloadGone,
			Namespace: k.ns,
			Status:    controlplane.WorkloadStatus{App: app},
		})
		return
	}
	k.emit(controlplane.WorkloadEvent{Kind: controlplane.WorkloadChanged, Namespace: k.ns, Status: d.status(app)})
}

// emit delivers one event to the watch over ev.Namespace, if one is established and its context is
// still live. A full channel becomes a DROPPED event rather than a discarded one: a silently lost
// event would leave the consumer's latch believing a condition that has cleared, and the test would
// then be debugging the fake instead of the observer.
func (k *Kubernetes) emit(ev controlplane.WorkloadEvent) {
	w := k.watchers[ev.Namespace]
	if w == nil || w.ctx.Err() != nil {
		return
	}
	select {
	case w.events <- ev:
		return
	default:
	}
	select {
	case w.events <- controlplane.WorkloadEvent{
		Kind:      controlplane.WorkloadDropped,
		Namespace: ev.Namespace,
		Detail:    "the watch fell behind its consumer and lost its place",
	}:
	default:
	}
}

// SetReady overrides the ready replica count for app, modelling a partial rollout. It
// is a no-op if app has no workload.
func (k *Kubernetes) SetReady(app string, ready int32) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if d := k.deploys[k.key(app)]; d != nil {
		d.ready = ready
		k.notify(app)
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
		d.issue, d.issueServing = imagePullEvidence(reason, message), false
		k.notify(app)
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
		d.issue, d.issueServing = nil, false
		k.notify(app)
		return
	}
	d.issue, d.issueServing = &ev, false
	k.notify(app)
}

// SetServingIssue injects a condition observed on a workload that is STILL SERVING: the shape a
// container killed and restarted in place leaves behind (issue #416). The workload reports the
// Issue and its reason, and its availability stays exactly what the replica counts say — which is
// what the real adapter does on the rolled-out path, where a kill the app survived attaches an
// Issue and never downgrades availability.
//
// It is a separate setter rather than a flag on SetIssue because the two model opposite facts about
// the same app, and a test that reaches for the wrong one should not compile into a plausible-looking
// assertion. Pass a reason outside the closed set to clear the condition.
func (k *Kubernetes) SetServingIssue(app string, ev controlplane.IssueEvidence) {
	k.mu.Lock()
	defer k.mu.Unlock()
	d := k.deploys[k.key(app)]
	if d == nil {
		return
	}
	if !controlplane.IsIssueReason(ev.Reason) {
		d.issue, d.issueServing = nil, false
		k.notify(app)
		return
	}
	d.issue, d.issueServing = &ev, true
	k.notify(app)
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
		d.issue, d.issueServing = imagePullEvidence(reason, ""), false
		k.notify(app)
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
	k.notify(spec.App)
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

// AwaitRollout answers from the state the test seeded, WITHOUT WAITING. The real adapter polls
// because a cluster changes underneath it; the fake's cluster changes only when a test says so, so a
// loop here would either spin forever or make every test sleep. The verdict is the adapter's,
// reached through the same WorkloadStatus both sides render:
//
//   - a blocking condition injected with SetIssue/SetImagePullFailure/SetWedgedRollout is a failure
//     carrying that reason, exactly as the status surface reports it;
//   - a workload that is not there is ReasonWorkloadMissing (ADR-0074 §6);
//   - ready below desired, with nothing blocking, is the deadline BACKSTOP — the shape of a rollout
//     that never finished and never said why (ADR-0072 §5), which is the case a test seeds with
//     SetReady(app, n) below desired.
//
// The bound is recorded rather than honoured, so a test can still assert the engine resolved it from
// the operational configuration (ADR-0072 §5).
func (k *Kubernetes) AwaitRollout(ctx context.Context, app string, timeout time.Duration) (controlplane.RolloutOutcome, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	*k.rollouts = append(*k.rollouts, rolloutCall{App: app, Timeout: timeout, Namespace: k.ns})
	if err := k.errs[OpAwaitRollout]; err != nil {
		return controlplane.RolloutOutcome{}, err
	}
	if out, ok := k.rolloutOut[k.key(app)]; ok {
		return out, nil
	}
	d := k.deploys[k.key(app)]
	if d == nil {
		return controlplane.RolloutOutcome{
			Reason: controlplane.ReasonWorkloadMissing,
			Detail: "the app's Deployment is not in the cluster, so there is no rollout to wait for",
		}, nil
	}
	st := d.status(app)
	if st.IssueReason != "" {
		return controlplane.RolloutOutcome{
			Reason: st.IssueReason,
			Detail: fmt.Sprintf("%d of %d replicas updated, %d ready", st.UpdatedReplicas, st.DesiredReplicas, st.ReadyReplicas),
		}, nil
	}
	if st.Available {
		return controlplane.RolloutOutcome{Settled: true}, nil
	}
	return controlplane.RolloutOutcome{
		Reason: controlplane.ReasonDeadlineExceeded,
		Detail: fmt.Sprintf("waited %s; %d of %d replicas updated, %d ready", timeout, st.UpdatedReplicas, st.DesiredReplicas, st.ReadyReplicas),
	}, nil
}

// SetRolloutOutcome overrides what AwaitRollout answers for app, for the one case seeded cluster
// state cannot express: ApplyWorkload marks a fake workload ready on apply, so a rollout that is
// still SHORT OF ITS REPLICAS after the deploy that started it — the shape behind an expired
// settle-wait (ADR-0072 §5) — has no state a test could seed for it. The derived answer above stays
// the default, so a test that seeds a real blocking condition still exercises the real mapping.
func (k *Kubernetes) SetRolloutOutcome(app string, out controlplane.RolloutOutcome) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.rolloutOut[k.key(app)] = out
}

// Rollouts returns the recorded AwaitRollout invocations, in order, so a test can assert a settle
// wait happened at all (and with which bound) rather than inferring it from the hook that followed.
func (k *Kubernetes) Rollouts() []rolloutCall {
	k.mu.Lock()
	defer k.mu.Unlock()
	return append([]rolloutCall(nil), *k.rollouts...)
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
	k.notify(app)
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
	k.notify(app)
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
	// The backup claim is created on first backup, exactly as the adapter creates it (ADR-0032) —
	// one per ENVIRONMENT (ADR-0067 §1), so a backup taken in staging creates staging's claim and
	// leaves production's alone. From then on it is a claim in the add-on namespace like any other,
	// including after the add-on that filled it is gone.
	claim, err := controlplane.BackupVolumeName(controlplane.AddonPostgres, fakeEnvName(env))
	if err != nil {
		return controlplane.BackupJobOutcome{}, err
	}
	k.volumes[claim] = fakeVolume{
		Addon: controlplane.AddonPostgres,
		Env:   fakeEnvName(env),
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

// SetRunJobHook installs a function RunJob calls while a Job is "in flight", OUTSIDE the fake's own
// lock. It exists so a test can assert the ENGINE serializes lifecycle hooks per app and environment
// (ADR-0072 §9): with the call inside the lock, every RunJob would appear serialized no matter what
// the engine did, and the assertion would pass vacuously. Passing nil clears it.
func (k *Kubernetes) SetRunJobHook(fn func()) {
	k.mu.Lock()
	defer k.mu.Unlock()
	*k.runJobHook = fn
}

func (k *Kubernetes) RunJob(ctx context.Context, spec controlplane.RunSpec) (controlplane.RunResult, error) {
	k.mu.Lock()
	if err := k.errs[OpRunJob]; err != nil {
		k.mu.Unlock()
		return controlplane.RunResult{}, err
	}
	env := make(map[string]string, len(spec.Env))
	for key, v := range spec.Env {
		env[key] = v
	}
	*k.runs = append(*k.runs, runCall{App: spec.App, Image: spec.Image, Command: append([]string(nil), spec.Command...), TTLSeconds: spec.TTLSeconds, Namespace: k.ns, Env: env, Probe: spec.Probe,
		SecretFiles: spec.SecretFiles, SecretEnvKeys: append([]string(nil), spec.SecretEnvKeys...)})
	inFlight, res := *k.runJobHook, *k.runResult
	k.mu.Unlock()
	if inFlight != nil {
		inFlight()
	}
	return res, nil
}

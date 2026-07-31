// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

// Package fake provides in-memory implementations of the control-plane seams
// (controlplane.Clock, Kubernetes, Registry, Database) for tests. They are
// deterministic and controllable: time is explicit, state is inspectable, and errors
// can be injected per operation so engine logic can be exercised against absence and
// failure without a real cluster, registry, or database (ADR-0010).
//
// The fakes are concurrency-safe so later fault-injection tests can drive them from
// multiple goroutines under the race detector. This package is licensed
// Apache-2.0 (see LICENSING.md and ADR-0033); it lives under controlplane/internal
// so it is importable only by the control plane it supports.
package fake

import "github.com/burrow-cloud/burrow/controlplane"

// Op names a seam method for error injection. Pass one to a fake's SetError to make
// that method return the given error until it is cleared (SetError(op, nil)).
type Op string

const (
	OpApply                Op = "ApplyWorkload"
	OpStatus               Op = "WorkloadStatus"
	OpScale                Op = "ScaleWorkload"
	OpLogs                 Op = "Logs"
	OpDelete               Op = "DeleteWorkload"
	OpExpose               Op = "Expose"
	OpUnexpose             Op = "Unexpose"
	OpExposureStatus       Op = "ExposureStatus"
	OpSecretKeys           Op = "SecretKeys"
	OpSetSecretValue       Op = "SetSecretValue"
	OpUnsetSecretKey       Op = "UnsetSecretKey"
	OpRestartWorkload      Op = "RestartWorkload"
	OpApplyAutoscaler      Op = "ApplyAutoscaler"
	OpDeleteAutoscaler     Op = "DeleteAutoscaler"
	OpAutoscalerActive     Op = "AutoscalerActive"
	OpMetricsAPIAvailable  Op = "MetricsAPIAvailable"
	OpAddonReady           Op = "AddonReady"
	OpAddonVolumes         Op = "AddonVolumes"
	OpSaveAddon            Op = "SaveAddon"
	OpAddon                Op = "Addon"
	OpAddons               Op = "Addons"
	OpDeleteAddon          Op = "DeleteAddon"
	OpSaveRelease          Op = "SaveRelease"
	OpRelease              Op = "Release"
	OpLatestRelease        Op = "LatestRelease"
	OpReleases             Op = "Releases"
	OpListReleases         Op = "ListReleases"
	OpDeleteReleases       Op = "DeleteReleases"
	OpAppEnv               Op = "AppEnv"
	OpSetAppEnv            Op = "SetAppEnv"
	OpUnsetAppEnv          Op = "UnsetAppEnv"
	OpAppHook              Op = "AppHook"
	OpAppHooks             Op = "AppHooks"
	OpSetAppHook           Op = "SetAppHook"
	OpUnsetAppHook         Op = "UnsetAppHook"
	OpDeleteAppHooks       Op = "DeleteAppHooks"
	OpPolicy               Op = "Policy"
	OpSetGuardrail         Op = "SetGuardrail"
	OpOperationalConfig    Op = "OperationalConfig"
	OpSetLimit             Op = "SetLimit"
	OpAutoDeployLevel      Op = "AutoDeployLevel"
	OpSetAutoDeployLevel   Op = "SetAutoDeployLevel"
	OpDisableAutoDeploy    Op = "DisableAutoDeploy"
	OpAutoDeployReason     Op = "AutoDeployReason"
	OpAutoDeployCandidates Op = "AutoDeployCandidates"
	OpSaveProvider         Op = "SaveProvider"
	OpProvider             Op = "Provider"
	OpProviders            Op = "Providers"
	OpToken                Op = "Token"
	OpSetToken             Op = "SetToken"
	OpDNS                  Op = "DNS"
	OpVerifyAccess         Op = "VerifyAccess"
	OpAppendAudit          Op = "AppendAudit"
	OpAudit                Op = "Audit"
	OpRecordBackup         Op = "RecordBackup"
	OpSetBackupStatus      Op = "SetBackupStatus"
	OpFailBackup           Op = "FailBackup"
	OpListBackups          Op = "ListBackups"
	OpGetBackup            Op = "GetBackup"
	OpRunBackupJob         Op = "RunBackupJob"
	OpRunRestoreJob        Op = "RunRestoreJob"
	OpRunJob               Op = "RunJob"
	OpBackupJobPresent     Op = "BackupJobPresent"

	OpCreateEnvironment Op = "CreateEnvironment"
	OpListEnvironments  Op = "ListEnvironments"
	OpGetEnvironment    Op = "GetEnvironment"
	OpDeleteEnvironment Op = "DeleteEnvironment"

	// The failure ledger and what the observer reads to fill it (ADR-0074 §3–§6).
	OpRecordFailure           Op = "RecordFailure"
	OpResolveFailures         Op = "ResolveFailures"
	OpFailures                Op = "Failures"
	OpStartObservationWindow  Op = "StartObservationWindow"
	OpExtendObservationWindow Op = "ExtendObservationWindow"
	OpObservationWindows      Op = "ObservationWindows"
	OpPruneLedger             Op = "PruneLedger"
	OpManagedApps             Op = "ManagedApps"
	OpPendingBackups          Op = "PendingBackups"
	OpRecordExposure          Op = "RecordExposure"
	OpDeleteExposure          Op = "DeleteExposure"
	OpExposures               Op = "Exposures"
)

// cloneRelease returns a deep copy of r so a fake never aliases a caller's Env or
// Command slices/maps — matching a real database, which serializes its records.
func cloneRelease(r controlplane.Release) controlplane.Release {
	if r.Env != nil {
		m := make(map[string]string, len(r.Env))
		for k, v := range r.Env {
			m[k] = v
		}
		r.Env = m
	}
	if r.Command != nil {
		c := make([]string, len(r.Command))
		copy(c, r.Command)
		r.Command = c
	}
	return r
}

// cloneProvider returns a deep copy of p so a fake never aliases a caller's Capabilities
// slice — matching a real database, which serializes its records.
func cloneProvider(p controlplane.Provider) controlplane.Provider {
	if p.Capabilities != nil {
		c := make([]controlplane.Capability, len(p.Capabilities))
		copy(c, p.Capabilities)
		p.Capabilities = c
	}
	return p
}

// cloneAddon returns a deep copy of a so a fake never aliases a caller's Capabilities slice —
// matching a real database, which serializes its records.
func cloneAddon(a controlplane.AddonInfo) controlplane.AddonInfo {
	if a.Capabilities != nil {
		c := make([]string, len(a.Capabilities))
		copy(c, a.Capabilities)
		a.Capabilities = c
	}
	return a
}

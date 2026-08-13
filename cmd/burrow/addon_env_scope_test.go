// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/burrow-cloud/burrow/controlplane"
	"github.com/burrow-cloud/burrow/localconfig"
)

// The scope of the add-on backup commands (issue #570). An omitted --env means the environment the
// invocation is targeting, exactly as it does on every other command; --all-environments is the one
// thing that asks a listing to span them all, and the verbs that ACT do not carry it.

// backupRequest is what a fake burrowd recorded about the call that reached it: the path (which
// carries the environment for the calls whose narrowing rides the route) and the query.
type backupRequest struct {
	path    string
	query   string
	bodyEnv string
}

// env reports the environment the recorded call named, from each of the three places a client puts
// it: the `/env/<name>` route segment used by the calls that aim a write or feed a restore, the
// `env` query parameter used by the plain reads, and the request body used by the physical backup.
// An empty string is a call that named none, which on the read path is the server's "every
// environment" encoding.
func (r backupRequest) env() string {
	if _, after, found := strings.Cut(r.path, "/env/"); found {
		return strings.TrimSuffix(after, "/")
	}
	for _, pair := range strings.Split(r.query, "&") {
		if name, value, found := strings.Cut(pair, "="); found && name == "env" {
			return value
		}
	}
	return r.bodyEnv
}

// fakeBackupCluster stands in for one cluster's burrowd: it serves the install token Secret, then
// answers the backup listing, the health report and the backup itself, recording what each call
// carried so a test can assert the environment the CLI resolved.
func fakeBackupCluster(got *backupRequest) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "/secrets/") {
			_ = json.NewEncoder(w).Encode(&corev1.Secret{
				TypeMeta:   metav1.TypeMeta{Kind: "Secret", APIVersion: "v1"},
				ObjectMeta: metav1.ObjectMeta{Name: "burrowd-api-token", Namespace: "burrow"},
				Data:       map[string][]byte{"token": []byte("s3cr3t")},
			})
			return
		}
		got.path, got.query, got.bodyEnv = r.URL.Path, r.URL.RawQuery, ""
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err == nil {
			if env, ok := body["env"].(string); ok {
				got.bodyEnv = env
			}
		}
		switch {
		case strings.Contains(r.URL.Path, "/addons/backup-health"):
			_ = json.NewEncoder(w).Encode(map[string]any{"addon": "postgres", "summary": "backups: none recorded"})
		case strings.Contains(r.URL.Path, "/addons/backups"):
			_ = json.NewEncoder(w).Encode(map[string]any{"backups": []any{}})
		case strings.Contains(r.URL.Path, "/addons/restore-instance"):
			_ = json.NewEncoder(w).Encode(map[string]any{
				"addon": "postgres", "environment": got.bodyEnv, "instance": "burrow-postgres-staging",
				"recovery_target": "the newest state in the repository", "safety_backup": "bk0",
				"apps": []string{"web"}, "reconnected": []string{"web"},
			})
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{
				"backup": map[string]any{
					"id": "bk1", "app": "web", "environment": "staging", "created_at": "2026-08-01T00:00:00Z",
					"status": "completed", "path": "/backups/web/bk1.dump", "destination": "cluster",
				},
			})
		}
	}))
}

// pinStagingHandle points the local config at a handle whose environment NAME is `staging`, pinned
// on the kubeconfig's current context so the connection and the environment name the same cluster.
// The handle's env is distinct from its app namespace, so an assertion on it proves the NAME was
// sent rather than the namespace.
func pinStagingHandle(t *testing.T) {
	t.Helper()
	cfg := &localconfig.Config{
		Current: "staging",
		Environments: []localconfig.Environment{
			{Name: "staging", Context: "staging", AppNamespace: "team-staging", Env: "staging"},
		},
	}
	if err := cfg.Save(); err != nil {
		t.Fatalf("save config: %v", err)
	}
}

// runAddon runs an add-on command against a fake cluster reached through a kubeconfig, which is the
// path the environment resolution actually decides — --control-plane bypasses it.
func runAddon(t *testing.T, kubeconfig string, args ...string) (stdout, stderr string) {
	t.Helper()
	var out, errb bytes.Buffer
	if err := run(context.Background(), append(args, "--kubeconfig", kubeconfig), &out, &errb); err != nil {
		t.Fatalf("%v: %v\nstderr: %s", args, err, errb.String())
	}
	return out.String(), errb.String()
}

// backupScopeFixture sets up an isolated config, a kubeconfig whose current context is `staging`,
// and a fake burrowd behind it, returning the kubeconfig path and the recorded request.
func backupScopeFixture(t *testing.T) (kubeconfig string, got *backupRequest) {
	t.Helper()
	t.Setenv("BURROW_CONTROL_PLANE_URL", "")
	t.Setenv("BURROW_API_TOKEN", "")
	tempConfig(t)
	forbidCloud(t)

	got = &backupRequest{}
	srv := fakeBackupCluster(got)
	t.Cleanup(srv.Close)
	return writeKubeconfig(t, twoContextConfig(srv.URL, srv.URL)), got
}

// TestBackupListingsScopeToThePinnedEnvironment is the bug in issue #570: `addon backups` and
// `addon backup-health` sent the RAW --env flag rather than the resolved environment, so with the
// flag omitted an empty value reached the server — which reads it as no filter. A pinned
// environment was silently ignored and the answer described environments nobody had asked about.
func TestBackupListingsScopeToThePinnedEnvironment(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"backups", []string{"addon", "backups", "postgres"}},
		{"backup-health", []string{"addon", "backup-health", "postgres"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			kubeconfig, got := backupScopeFixture(t)
			pinStagingHandle(t)

			runAddon(t, kubeconfig, tc.args...)

			if got.env() != "staging" {
				t.Errorf("%s sent environment %q, want the pinned environment staging (path %q, query %q)",
					tc.name, got.env(), got.path, got.query)
			}
		})
	}
}

// TestBackupListingsAllEnvironmentsSendsNoFilter asserts --all-environments is what now sends the
// empty value: the server keeps it as its "no filter" encoding, and this is the only way a client
// reaches it. Without the flag the span-everything answer is unreachable by accident.
func TestBackupListingsAllEnvironmentsSendsNoFilter(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"backups", []string{"addon", "backups", "postgres", "--all-environments"}},
		{"backups shorthand", []string{"addon", "backups", "postgres", "-A"}},
		{"backup-health", []string{"addon", "backup-health", "postgres", "--all-environments"}},
		{"backup-health shorthand", []string{"addon", "backup-health", "postgres", "-A"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			kubeconfig, got := backupScopeFixture(t)
			pinStagingHandle(t)

			runAddon(t, kubeconfig, tc.args...)

			if got.env() != "" {
				t.Errorf("%s sent environment %q, want none: --all-environments is the request for every environment (path %q, query %q)",
					tc.name, got.env(), got.path, got.query)
			}
		})
	}
}

// TestBackupListingsNameTheDefaultEnvironmentWhenNothingIsPinned covers the install that has never
// pinned a handle. There is still an active environment there — the default one install creates —
// so the listing is scoped to it rather than falling back to the widened answer the flag now owns.
func TestBackupListingsNameTheDefaultEnvironmentWhenNothingIsPinned(t *testing.T) {
	kubeconfig, got := backupScopeFixture(t)

	runAddon(t, kubeconfig, "addon", "backups", "postgres")

	if got.env() != "prod" {
		t.Errorf("sent environment %q, want the default environment prod (path %q, query %q)", got.env(), got.path, got.query)
	}
}

// TestBackupWritesActInThePinnedEnvironment is the same defect on the verbs that ACT, and the worse
// half of it: a backup or a restore that resolves its environment differently from the deploy before
// it acts on another environment's instance. `restore` is the sharpest — it overwrites a live
// database — so each of the three is asserted rather than sampled.
func TestBackupWritesActInThePinnedEnvironment(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"backup", []string{"addon", "backup", "postgres", "web"}},
		{"backup-instance", []string{"addon", "backup-instance", "postgres"}},
		{"restore", []string{"addon", "restore", "postgres", "web", "--backup", "bk1", "--confirm"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			kubeconfig, got := backupScopeFixture(t)
			pinStagingHandle(t)

			runAddon(t, kubeconfig, tc.args...)

			if got.env() != "staging" {
				t.Errorf("%s acted in environment %q, want the pinned environment staging (path %q)", tc.name, got.env(), got.path)
			}
		})
	}
}

// TestRestoreInstanceRewindsThePinnedEnvironment is the same defect at its largest blast radius: a
// physical restore takes back EVERY database on an instance, so an operator with staging pinned and
// no --env typed was rewinding production. The environment is resolved once and reaches the call,
// which is also what makes the typed-name prompt name the instance that is actually destroyed.
func TestRestoreInstanceRewindsThePinnedEnvironment(t *testing.T) {
	kubeconfig, got := backupScopeFixture(t)
	pinStagingHandle(t)

	runAddon(t, kubeconfig, "addon", "restore-instance", "postgres", "--latest", "--acknowledge-data-loss", "--confirm")

	if got.env() != "staging" {
		t.Errorf("restore-instance rewound environment %q, want the pinned environment staging (path %q)", got.env(), got.path)
	}
}

// TestRestoreInstanceNamesThePinnedInstanceInItsRefusal reaches the resolved environment by its
// other route: the label in the off-terminal refusal is derived from the environment BEFORE anything
// is contacted (ADR-0064 §2), so it is the one place the resolution is visible without a connection
// — and a prompt that named another environment's instance would be asking for consent to the wrong
// thing.
func TestRestoreInstanceNamesThePinnedInstanceInItsRefusal(t *testing.T) {
	t.Setenv("BURROW_CONTROL_PLANE_URL", "")
	t.Setenv("BURROW_API_TOKEN", "")
	tempConfig(t)
	forbidCloud(t)
	kubeconfig := unreachableCluster(t)
	pinStagingHandle(t)

	origTerm := stdinIsTerminal
	stdinIsTerminal = func(io.Reader) bool { return false }
	t.Cleanup(func() { stdinIsTerminal = origTerm })

	var out, errb bytes.Buffer
	err := run(context.Background(), []string{"addon", "restore-instance", "postgres", "--latest", "--kubeconfig", kubeconfig}, &out, &errb)
	if err == nil {
		t.Fatal("restore-instance off a terminal was accepted without --acknowledge-data-loss")
	}
	want, nameErr := controlplane.AddonInstanceName(controlplane.AddonPostgres, "staging")
	if nameErr != nil {
		t.Fatalf("derive instance name: %v", nameErr)
	}
	if !strings.Contains(err.Error(), want) {
		t.Errorf("refusal %q does not name the pinned environment's instance %q", err, want)
	}
}

// TestExplicitEnvStillWins keeps the flag's precedence: naming an environment is somebody being
// deliberate, and it overrides the pin exactly as it does on every other command. It holds on the
// listing and on the rewind alike, since a restore aimed by hand at an environment other than the
// pinned one is precisely when the flag has to be obeyed.
func TestExplicitEnvStillWins(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"backups", []string{"addon", "backups", "postgres", "--env", "prod"}},
		{"restore-instance", []string{"addon", "restore-instance", "postgres", "--latest", "--acknowledge-data-loss", "--confirm", "--env", "prod"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			kubeconfig, got := backupScopeFixture(t)
			pinStagingHandle(t)

			runAddon(t, kubeconfig, tc.args...)

			if got.env() != "prod" {
				t.Errorf("%s sent environment %q, want the named environment prod (path %q)", tc.name, got.env(), got.path)
			}
		})
	}
}

// TestEnvAndAllEnvironmentsAreRefusedTogether asserts the contradiction is refused rather than
// resolved by precedence, and refused BEFORE anything is contacted: either silent winner ignores
// something the person typed.
func TestEnvAndAllEnvironmentsAreRefusedTogether(t *testing.T) {
	t.Setenv("BURROW_CONTROL_PLANE_URL", "")
	t.Setenv("BURROW_API_TOKEN", "")
	tempConfig(t)
	forbidCloud(t)
	kubeconfig := unreachableCluster(t)

	var out, errb bytes.Buffer
	err := run(context.Background(), []string{"addon", "backups", "postgres", "--env", "prod", "--all-environments", "--kubeconfig", kubeconfig}, &out, &errb)
	if err == nil {
		t.Fatal("--env with --all-environments was accepted; the two scopes contradict each other")
	}
	for _, want := range []string{"--env", "--all-environments"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal %q does not name %s", err, want)
		}
	}
}

// TestRestoreInstanceRefusesAllEnvironments asserts the parser refuses the widening flag rather than
// accepting and ignoring it, on the verb where being wrong about scope destroys data. The refusal
// lands before anything is contacted: the fake cluster fails the test if it is reached.
func TestRestoreInstanceRefusesAllEnvironments(t *testing.T) {
	t.Setenv("BURROW_CONTROL_PLANE_URL", "")
	t.Setenv("BURROW_API_TOKEN", "")
	tempConfig(t)
	forbidCloud(t)
	kubeconfig := unreachableCluster(t)
	pinStagingHandle(t)

	var out, errb bytes.Buffer
	err := run(context.Background(), []string{"addon", "restore-instance", "postgres", "--latest", "-A", "--acknowledge-data-loss", "--confirm", "--kubeconfig", kubeconfig}, &out, &errb)
	if err == nil {
		t.Fatal("restore-instance accepted -A; a rewind acts on one instance in one environment")
	}
	if !strings.Contains(err.Error(), "unknown shorthand flag") {
		t.Errorf("refusal = %q, want the parser rejecting the flag outright", err)
	}
}

// TestWriteVerbsHaveNoAllEnvironmentsFlag. A backup or a restore runs against exactly one instance
// in exactly one environment, so "every environment" is not a scope those verbs have; the flag is
// absent rather than accepted-and-ignored.
func TestWriteVerbsHaveNoAllEnvironmentsFlag(t *testing.T) {
	for _, cmd := range []struct {
		name string
		cmd  *cobra.Command
	}{
		{"backup", newAddonBackupCmd()},
		{"backup-instance", newAddonBackupInstanceCmd()},
		{"restore", newAddonRestoreCmd()},
		{"restore-instance", newAddonRestoreInstanceCmd()},
	} {
		if cmd.cmd.Flags().Lookup("all-environments") != nil {
			t.Errorf("`addon %s` binds --all-environments; a command that acts has one environment", cmd.name)
		}
	}
	for _, cmd := range []struct {
		name string
		cmd  *cobra.Command
	}{
		{"backups", newAddonBackupsCmd()},
		{"backup-health", newAddonBackupHealthCmd()},
	} {
		f := cmd.cmd.Flags().Lookup("all-environments")
		if f == nil {
			t.Fatalf("`addon %s` does not bind --all-environments", cmd.name)
		}
		if f.Shorthand != "A" {
			t.Errorf("`addon %s` binds --all-environments with shorthand %q, want A (the kubectl convention)", cmd.name, f.Shorthand)
		}
	}
}

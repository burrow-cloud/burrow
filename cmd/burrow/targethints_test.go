// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package main

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/burrow-cloud/burrow/client"
	"github.com/burrow-cloud/burrow/controlplane"
)

// operatorCommands is every command a managed tenant is refused, in the form a hint would name it.
// It is the reason this file exists: a hint that contains one of these strings, printed while the
// managed product is the active target, is advice the reader cannot take (issue #580).
//
// It is deliberately a list of PREFIXES rather than of exact command lines, so a hint that names one
// with different arguments is caught too. The refusals themselves are catalogued in
// clusteronly_test.go; this is the subset a hint has any reason to reach for.
var operatorCommands = []string{
	"burrow addon install",
	"burrow addon remove",
	"burrow addon connect",
	"burrow addon backup",
	"burrow addon restore",
	"burrow cluster config set",
	"burrow addon config postgres standbys",
	"burrow addon config postgres storage",
	"burrow addon config postgres <setting>",
	"burrow guard set",
	"burrow config registry",
	"burrow config provider",
	"burrow env add",
	"burrow cluster install",
	"burrow cluster upgrade",
}

// assertNoOperatorCommand fails when a string aimed at a managed tenant names a command they cannot
// run.
func assertNoOperatorCommand(t *testing.T, what, s string) {
	t.Helper()
	for _, cmd := range operatorCommands {
		if strings.Contains(s, cmd) {
			t.Errorf("%s names %q, which the managed product refuses:\n%s", what, cmd, s)
		}
	}
}

// TestManagedHintsNameNoOperatorCommand is the rule this file holds, applied to every hint whose
// wording depends on the target. Each managed form must name no refused command; each cluster form
// must be exactly what it was, since a self-hosted operator's hint was already right.
func TestManagedHintsNameNoOperatorCommand(t *testing.T) {
	cases := []struct {
		name        string
		managed     string
		cluster     string
		wantCluster string
	}{
		{
			name:        "logs source note",
			managed:     logsSourceNote(true),
			cluster:     logsSourceNote(false),
			wantCluster: "install the logs add-on (`burrow addon install logs`), then query with `burrow addon logs`",
		},
		{
			name:        "settle timeout",
			managed:     settleTimeoutHint(true),
			cluster:     settleTimeoutHint(false),
			wantCluster: "Raise the bound with `burrow cluster config set deploy.settle_timeout <duration>`",
		},
		{
			name:        "sql row cap",
			managed:     sqlRowCapHint(true, "staging"),
			cluster:     sqlRowCapHint(false, "staging"),
			wantCluster: "raise the cap with `burrow cluster config set --env staging addon.sql_rows <n>`",
		},
		{
			name:        "addon config listing tail",
			managed:     addonConfigListTail(true),
			cluster:     addonConfigListTail(false),
			wantCluster: "Change one with `burrow addon config postgres <setting> <value>`",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assertNoOperatorCommand(t, "the managed "+tc.name, tc.managed)
			if !strings.Contains(tc.cluster, tc.wantCluster) {
				t.Errorf("the cluster %s changed; want it to still contain %q:\n%s", tc.name, tc.wantCluster, tc.cluster)
			}
			if tc.managed == tc.cluster {
				t.Errorf("the %s is identical on both targets, so nothing distinguishes them:\n%s", tc.name, tc.managed)
			}
		})
	}
}

// TestManagedLogsNoteStillSaysWhereDurableLogsAre: the managed form is not the cluster form with a
// sentence deleted. The hazard the note exists for — live pod logs read as a durable history —
// applies to a tenant exactly as it does to an operator, so the remedy is still named, in the one
// command that works there.
func TestManagedLogsNoteStillSaysWhereDurableLogsAre(t *testing.T) {
	note := logsSourceNote(true)
	for _, want := range []string{"live Kubernetes pod logs", "not retained across restarts", "`burrow addon logs`"} {
		if !strings.Contains(note, want) {
			t.Errorf("the managed source note does not carry %q:\n%s", want, note)
		}
	}
}

// TestManagedSettleAndCapHintsPointAtTheLimitsRead: where a bound is genuinely not a tenant's to
// change, the hint says so and names the read that shows it — `burrow cluster config list`, which
// answers on either target (clusterconfig.go). Stating the value's whereabouts is what makes this a
// remedy rather than a deletion.
func TestManagedSettleAndCapHintsPointAtTheLimitsRead(t *testing.T) {
	for _, s := range []string{settleTimeoutHint(true), sqlRowCapHint(true, "staging")} {
		if !strings.Contains(s, "burrow cluster config list") {
			t.Errorf("a managed limit hint does not point at the listing that shows the value:\n%s", s)
		}
	}
}

// TestAppLogsOnCloudTargetDoesNotAdviseInstallingTheAddon is issue #580 end to end, on the operate
// path (resolveConnect): signed in to the managed product, `burrow app logs` must not tell the
// tenant to install an add-on the product refuses to let them install.
func TestAppLogsOnCloudTargetDoesNotAdviseInstallingTheAddon(t *testing.T) {
	signedInToCloud(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"lines":[{"pod":"web-1","message":"hello"}]}`))
	}))
	defer srv.Close()
	pointCloudAt(t, srv.URL)

	var out, errb bytes.Buffer
	if err := run(context.Background(), []string{"app", "logs", "web"}, &out, &errb); err != nil {
		t.Fatalf("burrow app logs against a cloud target: %v\nstderr: %s", err, errb.String())
	}
	assertNoOperatorCommand(t, "the source note on a managed target", errb.String())
	if !strings.Contains(errb.String(), "`burrow addon logs`") {
		t.Errorf("stderr = %q, want the query that does work on the managed product", errb.String())
	}
	if !strings.Contains(out.String(), "web-1  hello") {
		t.Errorf("stdout = %q, want the log line", out.String())
	}
}

// TestAddonConfigListingOnCloudTargetDoesNotAdviseChangingIt covers the OTHER wiring: `addon config
// postgres` reaches either target through readClient rather than resolveConnect, so the target kind
// is recorded in a different place and is asserted separately (main.go, eitherTargetClient).
func TestAddonConfigListingOnCloudTargetDoesNotAdviseChangingIt(t *testing.T) {
	signedInToCloud(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"addon":"postgres","environment":"default","instance":"pg-default","settings":[{"setting":"standbys","value":"1","description":"hot standbys","consequence":"adding one restarts attached apps"}]}`))
	}))
	defer srv.Close()
	pointCloudAt(t, srv.URL)

	var out, errb bytes.Buffer
	if err := run(context.Background(), []string{"addon", "config", "postgres"}, &out, &errb); err != nil {
		t.Fatalf("burrow addon config postgres against a cloud target: %v\nstderr: %s", err, errb.String())
	}
	assertNoOperatorCommand(t, "the settings listing on a managed target", out.String())
	if !strings.Contains(out.String(), "standbys") {
		t.Errorf("stdout = %q, want the instance's settings", out.String())
	}
}

// TestClusterTargetKeepsTheOperatorHints is the other half of the guarantee: a self-hosted install
// is untouched. The CLI reached through --control-plane, which names a control plane outright and
// consults no target, keeps every operator hint exactly as it was.
func TestClusterTargetKeepsTheOperatorHints(t *testing.T) {
	_, errOut, err := runCLI(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"lines":[{"pod":"web-1","message":"hello"}]}`))
	}, "app", "logs", "web")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.Contains(errOut, "`burrow addon install logs`") {
		t.Errorf("stderr = %q, want the self-hosted source note unchanged", errOut)
	}
}

// TestDeployAndRollbackDeadlineHintsFollowTheTarget pins the settle-timeout line through the two
// reports that print it, since each renders it from its own failure path.
func TestDeployAndRollbackDeadlineHintsFollowTheTarget(t *testing.T) {
	rollout := &client.RolloutReport{Settled: false, Reason: controlplane.ReasonDeadlineExceeded}
	deploy := client.DeployResult{
		Release: client.Release{ID: "r2", App: "web", Image: "img:2", Replicas: 1},
		Rollout: rollout,
	}
	rollback := client.RollbackResult{
		Release:               client.Release{ID: "r3", App: "web", Image: "img:1", Replicas: 1},
		RolledBackToReleaseID: "r1",
		Rollout:               rollout,
	}
	assertNoOperatorCommand(t, "the managed deploy report", deployHuman("web", deploy, true))
	assertNoOperatorCommand(t, "the managed rollback report", rollbackHuman("web", rollback, true))
	for name, report := range map[string]string{
		"deploy":   deployHuman("web", deploy, false),
		"rollback": rollbackHuman("web", rollback, false),
	} {
		if !strings.Contains(report, "`burrow cluster config set deploy.settle_timeout <duration>`") {
			t.Errorf("the cluster %s report lost the bound to raise:\n%s", name, report)
		}
	}
}

// TestSQLTruncationHintFollowsTheTarget pins the row-cap line through `addon sql`'s own renderer,
// which is a command the managed product answers (clusteronly.go) and so one whose hint a tenant
// actually reads.
func TestSQLTruncationHintFollowsTheTarget(t *testing.T) {
	res := client.SQLResult{
		Environment: "staging",
		Columns:     []string{"id"},
		Rows:        [][]*string{{ptr("1")}},
		RowCount:    1,
		Truncated:   true,
		RowLimit:    1,
	}
	assertNoOperatorCommand(t, "the managed sql result", formatSQLResult("web", res, true))
	if got := formatSQLResult("web", res, false); !strings.Contains(got, "addon.sql_rows") {
		t.Errorf("the cluster sql result lost the cap to raise:\n%s", got)
	}
}

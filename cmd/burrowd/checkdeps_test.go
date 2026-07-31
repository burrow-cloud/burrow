// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/burrow-cloud/burrow/controlplane"
)

// The probe is the half of ADR-0076 §4 that touches a credential, so the tests that matter most here
// are the ones that prove none of it comes back out.

// secretDSN is a connection string shaped exactly like the one Burrow writes into an app's Secret,
// with a password in the userinfo and a second one in the query. Nothing derived from it may appear
// in any reported field.
const secretDSN = "postgres://app_web:sup3r-s3cret@burrow-postgres.burrow-addons.svc:5432/web?sslmode=disable&password=another-s3cret"

// forbidden are the substrings that would mean a credential escaped.
var forbidden = []string{"sup3r-s3cret", "another-s3cret", "app_web", "postgres://"}

// assertNoSecret fails when any part of the credential reached a reported field.
func assertNoSecret(t *testing.T, where string, res controlplane.DependencyResult) {
	t.Helper()
	blob, err := json.Marshal(res)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, bad := range forbidden {
		if strings.Contains(string(blob), bad) {
			t.Fatalf("%s leaked %q into a reported field: %s", where, bad, blob)
		}
	}
}

// TestSafeTargetKeepsOnlyHostAndPort is the redaction, and it works by CONSTRUCTION rather than by
// spotting secrets: the userinfo and the query are dropped because they are not read, not because
// they were recognised. A redaction that works by pattern fails on the credential shape it has not
// seen.
func TestSafeTargetKeepsOnlyHostAndPort(t *testing.T) {
	cases := []struct{ dsn, want string }{
		{secretDSN, "burrow-postgres.burrow-addons.svc:5432"},
		// pgx fills the default port in, which is more informative than omitting it and is still only
		// the host and port.
		{"postgres://user:pw@db.example.com/app", "db.example.com:5432"},
		{"postgres://db:5432/app", "db:5432"},
		{"not a url at all", "the configured database"},
		// An empty credential must not be answered from THIS process's own PGHOST/defaults, and a unix
		// socket path is a local filesystem path rather than anything a reader could act on.
		{"", "the configured database"},
		{"   ", "the configured database"},
		{"postgres:///app", "the configured database"},
		{"://///", "the configured database"},
	}
	for _, c := range cases {
		got := safeTarget(c.dsn)
		if got != c.want {
			t.Errorf("safeTarget(%q) = %q, want %q", c.dsn, got, c.want)
		}
		for _, bad := range forbidden {
			if bad != "postgres://" && strings.Contains(got, bad) {
				t.Errorf("safeTarget(%q) = %q, which carries %q", c.dsn, got, bad)
			}
		}
	}
}

// TestProbeFailureDiscardsTheDriverMessage is the rule stated as a test. A driver is entitled to
// quote the connection string it was handed, and this text reaches a deploy result, a log line and an
// audit row — so the error is CLASSIFIED into a closed-set reason and then dropped, never wrapped.
func TestProbeFailureDiscardsTheDriverMessage(t *testing.T) {
	// An error shaped like a real driver's: it quotes the whole DSN, which is exactly why wrapping is
	// not allowed.
	chatty := errors.New(`failed to connect to ` + secretDSN + `: server error`)
	res := probeFailure(controlplane.DependencyResult{Kind: controlplane.DependencyPostgres},
		chatty, safeTarget(secretDSN), "DATABASE_URL")

	if res.Outcome != controlplane.DependencyFailed {
		t.Errorf("outcome = %q, want failed", res.Outcome)
	}
	if !controlplane.IsDependencyReason(res.Reason) {
		t.Errorf("reason %q is outside the closed vocabulary", res.Reason)
	}
	assertNoSecret(t, "probeFailure", res)
	if !strings.Contains(res.Detail, "burrow-postgres.burrow-addons.svc:5432") {
		t.Errorf("detail = %q, want it to name the host and port so the failure is diagnosable", res.Detail)
	}
}

// TestProbeFailureClassifies covers the mapping every reported reason comes from. Each row is a real
// failure shape, and each answer is a member of the closed set — no prose, and nothing that could
// carry a value.
func TestProbeFailureClassifies(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"deadline", context.DeadlineExceeded, controlplane.ReasonTimedOut},
		{"dns", &net.DNSError{Err: "no such host", Name: "db"}, controlplane.ReasonHostUnresolvable},
		{"refused", &net.OpError{Op: "dial", Err: syscall.ECONNREFUSED}, controlplane.ReasonConnectionRefused},
		{"bad password", &pgconn.PgError{Code: "28P01", Message: "password authentication failed for user \"app_web\""}, controlplane.ReasonAuthenticationFailed},
		{"no such database", &pgconn.PgError{Code: "3D000", Message: `database "web" does not exist`}, controlplane.ReasonQueryFailed},
		{"anything else", errors.New("something went wrong"), controlplane.ReasonUnreachable},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res := probeFailure(controlplane.DependencyResult{Kind: controlplane.DependencyPostgres},
				c.err, safeTarget(secretDSN), "DATABASE_URL")
			if res.Reason != c.want {
				t.Errorf("reason = %q, want %q", res.Reason, c.want)
			}
			if res.Detail == "" {
				t.Error("no detail: a reason with no context is not diagnosable")
			}
			assertNoSecret(t, c.name, res)
		})
	}
}

// TestPostgresErrorMessagesNeverTravel: a Postgres server is free to echo the role name it rejected,
// and the role name is derived from the credential. The SQLSTATE — a five-character standard code —
// is reported instead, which is what a reader actually branches on.
func TestPostgresErrorMessagesNeverTravel(t *testing.T) {
	res := probeFailure(controlplane.DependencyResult{Kind: controlplane.DependencyPostgres},
		&pgconn.PgError{Code: "28P01", Message: `password authentication failed for user "app_web"`, Detail: secretDSN},
		safeTarget(secretDSN), "DATABASE_URL")
	assertNoSecret(t, "PgError", res)
	if !strings.Contains(res.Detail, "28P01") {
		t.Errorf("detail = %q, want the SQLSTATE", res.Detail)
	}
}

// TestCheckPostgresReportsAnUnsetCredential is the misconfiguration §4 exists for. Burrow provisioned
// the database and wrote the connection string into the app's Secret, so the variable being absent
// from the running container is a real fault and one nothing else in the product would catch.
func TestCheckPostgresReportsAnUnsetCredential(t *testing.T) {
	res := checkPostgres(context.Background(),
		controlplane.ProbeCheck{Kind: controlplane.DependencyPostgres, EnvKey: "DATABASE_URL"},
		func(string) string { return "" })
	if res.Outcome != controlplane.DependencyFailed || res.Reason != controlplane.ReasonCredentialUnset {
		t.Fatalf("result = %+v, want failed/CredentialUnset", res)
	}
	if !strings.Contains(res.Detail, "DATABASE_URL") {
		t.Errorf("detail = %q, want it to name the key", res.Detail)
	}
	assertNoSecret(t, "unset credential", res)
}

// TestCheckPostgresReadsTheCredentialFromItsOwnEnvironment: the plan carries the KEY, and the value
// is resolved by Kubernetes inside the app's container. This asserts the probe actually reads it
// there rather than expecting it in the plan.
func TestCheckPostgresReadsTheCredentialFromItsOwnEnvironment(t *testing.T) {
	var asked string
	// Point at a port nothing is listening on so the check fails fast and deterministically; the
	// assertion is about WHERE the credential came from, not about reaching a database.
	res := checkPostgres(context.Background(),
		controlplane.ProbeCheck{Kind: controlplane.DependencyPostgres, EnvKey: "DATABASE_URL"},
		func(k string) string {
			asked = k
			return "postgres://u:p@127.0.0.1:1/app?sslmode=disable&connect_timeout=1"
		})
	if asked != "DATABASE_URL" {
		t.Errorf("the probe read %q from its environment, want DATABASE_URL", asked)
	}
	if res.Outcome != controlplane.DependencyFailed {
		t.Errorf("outcome = %q, want failed against a dead port", res.Outcome)
	}
	assertNoSecret(t, "own environment", res)
}

// TestCheckExposureReportsTheStatusAndDoesNotJudgeIt is §3's reasoning one step over. Burrow requests
// "/" because it does not know what path the app serves, so an app answering 500 there may be
// entirely correct: calling that a failed dependency would be Burrow inventing a requirement and then
// reporting the app for not meeting it. What is proven is that something is listening on the port
// the exposure routes to.
func TestCheckExposureReportsTheStatusAndDoesNotJudgeIt(t *testing.T) {
	for _, code := range []int{200, 404, 500} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(code)
		}))
		res := checkExposure(context.Background(), controlplane.ProbeCheck{Kind: controlplane.DependencyExposure, URL: srv.URL})
		srv.Close()
		if res.Outcome != controlplane.DependencyPassed {
			t.Errorf("HTTP %d reported as %q, want passed: the check measures reachability, not the status code", code, res.Outcome)
		}
		if res.Status != code {
			t.Errorf("status = %d, want %d reported", res.Status, code)
		}
	}
}

// TestCheckExposureFailsWhenNothingIsListening is the misconfiguration the exposure check is for: an
// app published on a port it does not serve.
func TestCheckExposureFailsWhenNothingIsListening(t *testing.T) {
	res := checkExposure(context.Background(),
		controlplane.ProbeCheck{Kind: controlplane.DependencyExposure, URL: "http://127.0.0.1:1"})
	if res.Outcome != controlplane.DependencyFailed {
		t.Fatalf("result = %+v, want failed", res)
	}
	if !controlplane.IsDependencyReason(res.Reason) {
		t.Errorf("reason %q is outside the closed vocabulary", res.Reason)
	}
}

// TestCheckExposureDoesNotFollowRedirects: a redirect is an answer, and following one would take the
// check off the app's own port, which is the one thing it is measuring.
func TestCheckExposureDoesNotFollowRedirects(t *testing.T) {
	elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	}))
	defer elsewhere.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, elsewhere.URL, http.StatusFound)
	}))
	defer srv.Close()

	res := checkExposure(context.Background(), controlplane.ProbeCheck{Kind: controlplane.DependencyExposure, URL: srv.URL})
	if res.Status != http.StatusFound {
		t.Errorf("status = %d, want %d: the redirect itself is the answer", res.Status, http.StatusFound)
	}
}

// TestCheckDependenciesPrintsOneMarkedLine is the contract with the engine, which reads the marked
// line rather than the whole stream because the probe runs in the USER's image and an entrypoint
// wrapper is free to print a banner.
func TestCheckDependenciesPrintsOneMarkedLine(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	plan, err := controlplane.MarshalProbePlan(controlplane.ProbePlan{Checks: []controlplane.ProbeCheck{
		{Kind: controlplane.DependencyExposure, URL: srv.URL},
		{Kind: controlplane.DependencyPostgres, EnvKey: "DATABASE_URL"},
	}})
	if err != nil {
		t.Fatalf("MarshalProbePlan: %v", err)
	}
	var out bytes.Buffer
	env := map[string]string{controlplane.ProbePlanEnv: plan}
	if err := checkDependencies(context.Background(), &out, func(k string) string { return env[k] }); err != nil {
		t.Fatalf("checkDependencies: %v — a dependency that did not answer is a RESULT, not an error", err)
	}
	rep, err := controlplane.ParseProbeReport(out.String())
	if err != nil {
		t.Fatalf("ParseProbeReport(%q): %v", out.String(), err)
	}
	if len(rep.Results) != 2 {
		t.Fatalf("results = %+v, want one per check in plan order", rep.Results)
	}
	if rep.Results[0].Kind != controlplane.DependencyExposure || rep.Results[1].Kind != controlplane.DependencyPostgres {
		t.Errorf("results are out of plan order: %+v", rep.Results)
	}
	for _, r := range rep.Results {
		assertNoSecret(t, "checkDependencies", r)
	}
	if n := strings.Count(out.String(), controlplane.ProbeResultPrefix); n != 1 {
		t.Errorf("printed %d marked lines, want exactly one", n)
	}
}

// TestCheckDependenciesNeedsItsPlan: with no plan there is nothing to report, which is the one case
// that IS an error — the engine turns it into a skipped result rather than mistaking silence for a
// clean bill of health.
func TestCheckDependenciesNeedsItsPlan(t *testing.T) {
	var out bytes.Buffer
	if err := checkDependencies(context.Background(), &out, func(string) string { return "" }); err == nil {
		t.Fatal("checkDependencies succeeded with no plan")
	}
	if out.Len() != 0 {
		t.Errorf("output = %q, want nothing printed", out.String())
	}
}

// TestInstallProbeCopiesTheBinaryExecutable is the mechanism ADR-0076's consequences call real work.
// The app's image may have no shell and no `cp`, so the binary copies ITSELF, and it must land
// executable by whatever UID the user's image runs as.
func TestInstallProbeCopiesTheBinaryExecutable(t *testing.T) {
	dir := t.TempDir()
	if err := installProbe(dir); err != nil {
		t.Fatalf("installProbe: %v", err)
	}
	path := filepath.Join(dir, controlplane.ProbeBinaryName)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if info.Mode().Perm()&0o111 != 0o111 {
		t.Errorf("mode = %v, want executable by every UID: the container that runs it is the user's image and may run as anyone", info.Mode())
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatalf("Executable: %v", err)
	}
	want, err := os.Stat(self)
	if err != nil {
		t.Fatalf("stat self: %v", err)
	}
	if info.Size() != want.Size() {
		t.Errorf("copied %d bytes, want %d: a partial binary fails in a way that reads as the app's image being broken", info.Size(), want.Size())
	}
	// No .partial is left behind: the copy goes to a temporary name and is renamed, so a check
	// container can never observe a half-written binary.
	if _, err := os.Stat(path + ".partial"); !os.IsNotExist(err) {
		t.Errorf("the temporary copy survived: %v", err)
	}
}

// TestInstallProbeNeedsATarget: a missing argument must fail loudly rather than write somewhere
// surprising.
func TestInstallProbeNeedsATarget(t *testing.T) {
	if err := installProbe(""); err == nil {
		t.Fatal("installProbe(\"\") succeeded")
	}
}

// TestCheckPostgresClassifiesAnUnparsableCredential is a regression guard on a subtlety of the
// driver. sql.Open with the stdlib driver does NOT parse the connection string — it defers that to
// the first connection — so opening and then querying would report a malformed credential as an
// unreachable database and send someone to debug a database that is fine. The credential is parsed
// up front, with the driver's own parser, and the verdict comes from there.
func TestCheckPostgresClassifiesAnUnparsableCredential(t *testing.T) {
	for _, dsn := range []string{"this is not a dsn at all", "postgres://%%%", "://///"} {
		res := checkPostgres(context.Background(),
			controlplane.ProbeCheck{Kind: controlplane.DependencyPostgres, EnvKey: "DATABASE_URL"},
			func(string) string { return dsn })
		if res.Reason != controlplane.ReasonCredentialUnparsable {
			t.Errorf("checkPostgres(%q) reason = %q, want %q: a credential the driver cannot read is not an unreachable database",
				dsn, res.Reason, controlplane.ReasonCredentialUnparsable)
		}
		if res.Outcome != controlplane.DependencyFailed {
			t.Errorf("checkPostgres(%q) outcome = %q, want failed", dsn, res.Outcome)
		}
		assertNoSecret(t, "unparsable credential", res)
		// The parser quotes what it could not read, and what it could not read is the credential.
		if strings.Contains(res.Detail, dsn) {
			t.Errorf("detail = %q, which quotes the credential", res.Detail)
		}
	}
}

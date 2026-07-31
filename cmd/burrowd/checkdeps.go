// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/stdlib"

	"github.com/burrow-cloud/burrow/controlplane"
)

// `burrowd install-probe` and `burrowd check-dependencies` are the deploy-time dependency check
// (ADR-0076 §4). Neither is the control plane: both are the same binary run inside a check Job's
// pod, and they connect to nothing this process normally needs — no Postgres, no cluster, no API
// token.
//
// WHY THE CHECK RUNS HERE RATHER THAN IN BURROWD'S OWN POD. A check run from the control plane
// proves the CONTROL PLANE can reach the database. The question is whether the APP can, and the two
// differ over precisely the things that break: the app's service account, its namespace's network
// policy, its DNS search path, and the credential Kubernetes injected into its container and not
// into burrowd's. So the check runs in the app's own image with the app's own environment, and reads
// the credential out of its own process environment — where the app itself reads it.
//
// WHY THE BINARY IS COPIED IN. The app's image is allowed to contain nothing: no shell, no psql, no
// curl. That is the image users are told to build, and it is the image this check is most valuable
// on. `install-probe` is Burrow's image copying its own executable into a shared emptyDir, so the
// check container needs the app's image to provide nothing at all — not even a `cp`, which is why
// the binary copies itself rather than being copied by one.
//
// NO SECRET VALUE LEAVES THIS PROCESS. That is the load-bearing property of the whole file. The
// credential is read from the environment and handed to the driver; every failure is turned into a
// reason from controlplane's closed set by probeFailure, and THE DRIVER'S OWN MESSAGE IS DISCARDED
// rather than wrapped, because a driver is entitled to quote the connection string it was given and
// this output travels to a deploy result, a log line and an audit row. What may be reported is what
// Burrow can prove is not a secret: the host and port, SELECTED from the config the driver's own
// parser produced rather than filtered out of the string, and a SQLSTATE, which is a five-character
// standard code.

const (
	// probeDialTimeout bounds one dependency's check. It is generous enough that a database under
	// load is not called unreachable and short enough that three dead dependencies cannot consume the
	// whole check window. It is a constant rather than an operational limit for the reason ADR-0068
	// §2 gives: nobody has a real reason to pick a different number for how long a one-shot
	// connection attempt waits, and ADR-0076 §4 asks for no such knob.
	probeDialTimeout = 10 * time.Second
	// probeBinaryMode is the mode the copied binary is written with: executable by everyone, because
	// the container that runs it is the USER's image and may run as any UID at all.
	probeBinaryMode fs.FileMode = 0o755
)

// installProbe copies this executable into dir so a container running another image can execute it
// (ADR-0076 §4). It is the init container's whole job.
//
// It writes to a temporary name and renames, so a check container that somehow started early can
// never observe a half-written binary — the rename is atomic within the directory, and a partially
// copied executable would fail in a way that reads as the app's image being broken.
func installProbe(dir string) error {
	if dir == "" {
		return errors.New("no target directory given")
	}
	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locating this executable: %w", err)
	}
	src, err := os.Open(self)
	if err != nil {
		return fmt.Errorf("opening this executable: %w", err)
	}
	defer src.Close()

	final := filepath.Join(dir, controlplane.ProbeBinaryName)
	tmp := final + ".partial"
	dst, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, probeBinaryMode)
	if err != nil {
		return fmt.Errorf("creating %s: %w", tmp, err)
	}
	if _, err := io.Copy(dst, src); err != nil {
		dst.Close()
		return fmt.Errorf("copying the probe into %s: %w", dir, err)
	}
	if err := dst.Close(); err != nil {
		return fmt.Errorf("writing the probe into %s: %w", dir, err)
	}
	// The mode is set explicitly as well as at create: a restrictive umask in the init container's
	// image would otherwise leave a binary the app's user cannot execute.
	if err := os.Chmod(tmp, probeBinaryMode); err != nil {
		return fmt.Errorf("making the probe executable: %w", err)
	}
	if err := os.Rename(tmp, final); err != nil {
		return fmt.Errorf("placing the probe at %s: %w", final, err)
	}
	return nil
}

// checkDependencies runs the plan in this container's environment and prints the report on its
// marked line (ADR-0076 §4).
//
// It returns an error ONLY when it could not produce a report at all. A dependency that did not
// answer is a RESULT, printed and exited zero, exactly as a non-zero exit from `burrow app run` is a
// result rather than an error: the engine has to tell "the database refused the connection" apart
// from "the check never ran", because only the first says anything about the app.
func checkDependencies(ctx context.Context, out io.Writer, getenv func(string) string) error {
	encoded := getenv(controlplane.ProbePlanEnv)
	if encoded == "" {
		return fmt.Errorf("%s is not set", controlplane.ProbePlanEnv)
	}
	plan, err := controlplane.ParseProbePlan(encoded)
	if err != nil {
		return err
	}
	if len(plan.Checks) == 0 {
		return errors.New("the plan names no checks")
	}
	rep := controlplane.ProbeReport{Results: make([]controlplane.DependencyResult, 0, len(plan.Checks))}
	for _, c := range plan.Checks {
		rep.Results = append(rep.Results, runProbeCheck(ctx, c, getenv))
	}
	line, err := json.Marshal(rep)
	if err != nil {
		return fmt.Errorf("rendering the result: %w", err)
	}
	_, err = fmt.Fprintln(out, controlplane.ProbeResultPrefix+string(line))
	return err
}

// runProbeCheck dispatches one check.
func runProbeCheck(ctx context.Context, c controlplane.ProbeCheck, getenv func(string) string) controlplane.DependencyResult {
	switch c.Kind {
	case controlplane.DependencyPostgres:
		return checkPostgres(ctx, c, getenv)
	case controlplane.DependencyExposure:
		return checkExposure(ctx, c)
	default:
		return controlplane.DependencyResult{
			Kind:    c.Kind,
			Outcome: controlplane.DependencySkipped,
			Reason:  controlplane.ReasonCheckNotRun,
			Detail:  "this build of the probe does not know how to check that dependency",
		}
	}
}

// checkPostgres connects with the app's own credential and runs the most trivial query there is
// (ADR-0076 §4). SELECT 1 is chosen because it proves the whole chain — resolve, connect, negotiate
// TLS, authenticate, be allowed into the database, execute — while touching no table the app owns
// and holding no lock.
func checkPostgres(ctx context.Context, c controlplane.ProbeCheck, getenv func(string) string) controlplane.DependencyResult {
	res := controlplane.DependencyResult{Kind: controlplane.DependencyPostgres}
	dsn := getenv(c.EnvKey)
	if strings.TrimSpace(dsn) == "" {
		res.Outcome = controlplane.DependencyFailed
		res.Reason = controlplane.ReasonCredentialUnset
		res.Detail = fmt.Sprintf("%s is not set in this app's container, but Burrow provisioned a database for it and wrote the connection string into its Secret. The Secret may not be mounted, or the key may have been removed.", c.EnvKey)
		return res
	}
	// The credential is PARSED FIRST, with the driver's own parser, and the verdict is taken from it.
	// sql.Open would not do it: the stdlib driver defers parsing to the first connection, so a
	// malformed connection string would surface as a failure to connect and be reported as an
	// unreachable database — sending someone to debug a database that is fine. Parsing here is also
	// what makes safeTarget total, since pgconn accepts the keyword/value form ("host=x user=y") that
	// no URL parser handles.
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		res.Outcome = controlplane.DependencyFailed
		res.Reason = controlplane.ReasonCredentialUnparsable
		// The parser's error is DISCARDED, not wrapped: it quotes the input it could not read, and the
		// input is the credential.
		res.Detail = fmt.Sprintf("%s is set but is not a connection string the driver can read.", c.EnvKey)
		return res
	}
	target := safeTargetFor(&cfg.Config)

	// A single connection with a bounded lifetime: this is one query, and a pool would outlive the
	// process by exactly nothing.
	db := sql.OpenDB(stdlib.GetConnector(*cfg))
	defer db.Close()
	db.SetMaxOpenConns(1)

	dialCtx, cancel := context.WithTimeout(ctx, probeDialTimeout)
	defer cancel()
	var one int
	if err := db.QueryRowContext(dialCtx, "SELECT 1").Scan(&one); err != nil {
		return probeFailure(res, err, target, c.EnvKey)
	}
	res.Outcome = controlplane.DependencyPassed
	res.Detail = fmt.Sprintf("connected to %s with %s and ran SELECT 1", target, c.EnvKey)
	return res
}

// checkExposure requests the app's own published port through the Service in front of it and reports
// the status code (ADR-0076 §4).
//
// IT DOES NOT JUDGE THE STATUS CODE, and that is §3's reasoning applied one step over. Burrow
// requests "/" because it has no idea what path the app serves; an app that answers 404 or 500 there
// may be entirely correct, and calling that a failed dependency would be Burrow inventing a
// requirement and then reporting the app for not meeting it. What this proves — and what a
// misconfigured published port fails — is that something is listening on the port the exposure
// routes to and speaking HTTP. The status is reported so a reader can see it; the verdict is
// reachability.
func checkExposure(ctx context.Context, c controlplane.ProbeCheck) controlplane.DependencyResult {
	res := controlplane.DependencyResult{Kind: controlplane.DependencyExposure}
	if c.URL == "" {
		res.Outcome = controlplane.DependencySkipped
		res.Reason = controlplane.ReasonCheckNotRun
		res.Detail = "no address to request"
		return res
	}
	reqCtx, cancel := context.WithTimeout(ctx, probeDialTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, c.URL, nil)
	if err != nil {
		res.Outcome = controlplane.DependencySkipped
		res.Reason = controlplane.ReasonCheckNotRun
		res.Detail = "the address could not be requested"
		return res
	}
	// Redirects are not followed: a redirect is an answer, and following one would take the check off
	// the app's own port, which is the one thing it is meant to be measuring.
	client := &http.Client{
		Timeout:       probeDialTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := client.Do(req)
	if err != nil {
		return probeFailure(res, err, c.URL, "")
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<10))
	res.Outcome = controlplane.DependencyPassed
	res.Status = resp.StatusCode
	res.Detail = fmt.Sprintf("%s answered with HTTP %d", c.URL, resp.StatusCode)
	return res
}

// probeFailure is the ONE place a dependency's error becomes a reported reason, and the one place the
// no-secret rule is enforced (ADR-0076 §4).
//
// err is CLASSIFIED and then DROPPED. It is never wrapped into the detail and never formatted with
// %v, because a database driver is entitled to include the connection string it was handed in its own
// message and this text reaches a deploy result, a log line and an audit row. target is the host and
// port, already through safeTarget.
func probeFailure(res controlplane.DependencyResult, err error, target, envKey string) controlplane.DependencyResult {
	res.Outcome = controlplane.DependencyFailed

	var pgErr *pgconn.PgError
	var dnsErr *net.DNSError
	var netErr net.Error
	switch {
	case errors.Is(err, context.DeadlineExceeded), errors.As(err, &netErr) && netErr.Timeout():
		res.Reason = controlplane.ReasonTimedOut
		res.Detail = fmt.Sprintf("%s did not answer within %s", target, probeDialTimeout)
	case errors.As(err, &dnsErr):
		res.Reason = controlplane.ReasonHostUnresolvable
		res.Detail = fmt.Sprintf("the host in %s could not be resolved from inside this app's container", target)
	case errors.Is(err, syscall.ECONNREFUSED):
		res.Reason = controlplane.ReasonConnectionRefused
		res.Detail = fmt.Sprintf("%s refused the connection: the host resolved and nothing is accepting on that port", target)
	case errors.As(err, &pgErr) && strings.HasPrefix(pgErr.Code, "28"):
		// SQLSTATE class 28 is invalid authorization. The code is reported and the message is not:
		// a server is free to echo the role name it rejected, and the role name is derived from the
		// credential.
		res.Reason = controlplane.ReasonAuthenticationFailed
		res.Detail = fmt.Sprintf("%s rejected the credential in %s (SQLSTATE %s). The database may have been reprovisioned since this app was attached; `burrow addon attach postgres <app>` rewrites it.", target, envKey, pgErr.Code)
	case errors.As(err, &pgErr):
		res.Reason = controlplane.ReasonQueryFailed
		res.Detail = fmt.Sprintf("%s answered but refused SELECT 1 (SQLSTATE %s)", target, pgErr.Code)
	default:
		res.Reason = controlplane.ReasonUnreachable
		res.Detail = fmt.Sprintf("%s could not be reached from inside this app's container", target)
	}
	return res
}

// safeTargetFor reduces a PARSED connection config to the part that is provably not a secret: its
// host and port. The password is not read, and neither is anything else on the config — the fields
// are selected rather than filtered, because a redaction that works by spotting secrets fails on the
// one shape it has not seen.
func safeTargetFor(cfg *pgconn.Config) string {
	// A host that begins with "/" is a unix socket DIRECTORY, which pgx also fills in from the
	// environment when a connection string names no host at all. It is a local filesystem path rather
	// than anything a reader could act on, so it is not reported: naming it would be both useless and
	// a way for a path Burrow did not compose to reach a deploy result.
	if cfg == nil || cfg.Host == "" || strings.HasPrefix(cfg.Host, "/") {
		return unknownTarget
	}
	if cfg.Port == 0 {
		return cfg.Host
	}
	return net.JoinHostPort(cfg.Host, strconv.Itoa(int(cfg.Port)))
}

// safeTarget is safeTargetFor over a raw connection string, for the callers that have not parsed one.
// A string the driver cannot read yields a fixed placeholder rather than any part of itself.
func safeTarget(dsn string) string {
	// An empty string is refused rather than parsed. pgx resolves a missing host from PGHOST and its
	// own defaults, so parsing "" would answer with something about THIS process's environment — which
	// is neither the app's dependency nor Burrow's to report.
	dsn = strings.TrimSpace(dsn)
	if dsn == "" {
		return unknownTarget
	}
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		return unknownTarget
	}
	return safeTargetFor(&cfg.Config)
}

// unknownTarget is what a failure names when Burrow cannot say anything about the target without
// quoting the credential.
const unknownTarget = "the configured database"

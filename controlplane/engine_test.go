// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package controlplane_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	cp "github.com/burrow-cloud/burrow/controlplane"
	"github.com/burrow-cloud/burrow/controlplane/internal/fake"
)

func newEngine(t *testing.T, policy cp.Policy) (*cp.Engine, *fake.Kubernetes, *fake.Database, *fake.Clock) {
	t.Helper()
	k := fake.NewKubernetes()
	d := fake.NewDatabase()
	d.SetPolicy(policy)
	c := fake.NewClock(time.Date(2026, 6, 23, 12, 0, 0, 0, time.UTC))
	e, err := cp.New(cp.Deps{
		Kubernetes: k, Database: d, Clock: c, IDs: fake.NewIDs(), Resolver: fake.NewResolver(),
		Credentials: fake.NewCredentials(), DNS: fake.NewDNSFactory(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return e, k, d, c
}

// permissive avoids guardrail interference for tests not about guardrails.
func permissive() cp.Policy {
	p := cp.DefaultPolicy()
	p.MaxReplicas = 1000
	return p.With(cp.GuardrailScaleToZero, cp.DispositionAllow)
}

// mustGuardrail asserts err is a guardrail refusal with the given code.
func mustGuardrail(t *testing.T, err error, code cp.GuardrailCode) {
	t.Helper()
	g, ok := cp.AsGuardrail(err)
	if !ok {
		t.Fatalf("err = %v, want a GuardrailError", err)
	}
	if g.Code != code {
		t.Fatalf("guardrail code = %q, want %q", g.Code, code)
	}
}

func TestNewValidatesDeps(t *testing.T) {
	k, d, c, id := fake.NewKubernetes(), fake.NewDatabase(), fake.NewClock(time.Now()), fake.NewIDs()
	good := cp.Deps{
		Kubernetes: k, Database: d, Clock: c, IDs: id, Resolver: fake.NewResolver(),
		Credentials: fake.NewCredentials(), DNS: fake.NewDNSFactory(),
	}

	if _, err := cp.New(good); err != nil {
		t.Fatalf("valid deps: %v", err)
	}

	// Each missing seam is rejected.
	bad := good
	bad.Kubernetes = nil
	if _, err := cp.New(bad); err == nil {
		t.Errorf("missing Kubernetes should error")
	}
	bad = good
	bad.IDs = nil
	if _, err := cp.New(bad); err == nil {
		t.Errorf("missing IDs should error")
	}
	bad = good
	bad.Database = nil
	if _, err := cp.New(bad); err == nil {
		t.Errorf("missing Database should error")
	}
	bad = good
	bad.Credentials = nil
	if _, err := cp.New(bad); err == nil {
		t.Errorf("missing Credentials should error")
	}
	bad = good
	bad.DNS = nil
	if _, err := cp.New(bad); err == nil {
		t.Errorf("missing DNS should error")
	}
}

func TestDeployHappyPath(t *testing.T) {
	ctx := context.Background()
	e, k, d, _ := newEngine(t, permissive())

	// Env is sourced from the app's config store at deploy time, not from the request (ADR-0028).
	if err := d.SetAppEnv(ctx, "web", "K", "V"); err != nil {
		t.Fatalf("SetAppEnv: %v", err)
	}

	res, err := e.Deploy(ctx, cp.DeployRequest{App: "web", Image: "registry.example.com/web:1", Replicas: 2})
	if err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	if res.Release.Status != cp.ReleaseDeployed {
		t.Errorf("release status = %q, want deployed", res.Release.Status)
	}
	if res.Release.Digest != "" {
		t.Errorf("digest = %q, want empty (burrowd does not resolve; ADR-0040)", res.Release.Digest)
	}
	if res.SupersededReleaseID != "" {
		t.Errorf("first deploy should supersede nothing, got %q", res.SupersededReleaseID)
	}

	// Applied to the cluster, with the env from the store rendered into the spec.
	spec, ok := k.Spec("web")
	if !ok || spec.Image != "registry.example.com/web:1" || spec.Replicas != 2 {
		t.Errorf("cluster spec = %+v ok=%v, want web:1 x2", spec, ok)
	}
	if spec.Env["K"] != "V" {
		t.Errorf("cluster spec env = %+v, want K=V from the store", spec.Env)
	}
	// Recorded in the database.
	saved, err := d.LatestRelease(ctx, "web")
	if err != nil || saved.Status != cp.ReleaseDeployed {
		t.Errorf("saved release = %+v err=%v", saved, err)
	}
}

// TestDeploySameImageRollsViaReleaseAnnotation proves the fix: two deploys of the SAME image
// reference produce pod templates with DIFFERENT release IDs, so the server-side apply changes and
// a rollout happens (fresh pods) — the flow behind "fix the pull credential, then re-deploy".
func TestDeploySameImageRollsViaReleaseAnnotation(t *testing.T) {
	ctx := context.Background()
	e, k, _, _ := newEngine(t, permissive())

	res1, err := e.Deploy(ctx, cp.DeployRequest{App: "web", Image: "registry.example.com/web:1", Replicas: 1})
	if err != nil {
		t.Fatalf("first Deploy: %v", err)
	}
	spec1, ok := k.Spec("web")
	if !ok {
		t.Fatalf("no cluster spec after first deploy")
	}
	if spec1.ReleaseID != res1.Release.ID {
		t.Errorf("first spec ReleaseID = %q, want the release ID %q", spec1.ReleaseID, res1.Release.ID)
	}

	res2, err := e.Deploy(ctx, cp.DeployRequest{App: "web", Image: "registry.example.com/web:1", Replicas: 1})
	if err != nil {
		t.Fatalf("second Deploy: %v", err)
	}
	spec2, ok := k.Spec("web")
	if !ok {
		t.Fatalf("no cluster spec after second deploy")
	}
	if spec2.ReleaseID != res2.Release.ID {
		t.Errorf("second spec ReleaseID = %q, want the release ID %q", spec2.ReleaseID, res2.Release.ID)
	}
	if spec1.ReleaseID == spec2.ReleaseID {
		t.Errorf("re-deploy of an identical image kept ReleaseID %q: the pod template did not change, so no rollout would happen", spec1.ReleaseID)
	}
}

// TestReapplyEnvKeepsCurrentReleaseID asserts a config reapply stamps the CURRENT release's ID, so
// it does not spuriously churn the release annotation — its intended roll comes from the env change,
// not an extra release bump.
func TestReapplyEnvKeepsCurrentReleaseID(t *testing.T) {
	ctx := context.Background()
	e, k, _, _ := newEngine(t, permissive())

	res, err := e.Deploy(ctx, cp.DeployRequest{App: "web", Image: "registry.example.com/web:1", Replicas: 1})
	if err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	if err := e.SetConfig(ctx, "web", "", "K", "V", false); err != nil {
		t.Fatalf("SetConfig: %v", err)
	}
	spec, ok := k.Spec("web")
	if !ok {
		t.Fatalf("no cluster spec after config set")
	}
	if spec.ReleaseID != res.Release.ID {
		t.Errorf("reapply ReleaseID = %q, want the current release ID %q (no spurious churn)", spec.ReleaseID, res.Release.ID)
	}
}

func TestDeployGuardrails(t *testing.T) {
	ctx := context.Background()
	e, k, _, _ := newEngine(t, cp.Policy{MaxReplicas: 5}.With(cp.GuardrailAppDeploy, cp.DispositionAllow))

	_, err := e.Deploy(ctx, cp.DeployRequest{App: "web", Image: "img:1", Replicas: 6})
	mustGuardrail(t, err, cp.GuardrailReplicaCeiling)
	_, err = e.Deploy(ctx, cp.DeployRequest{App: "web", Image: "img:1", Replicas: 0})
	mustGuardrail(t, err, cp.GuardrailScaleToZero)
	// A refused deploy touches nothing.
	if _, ok := k.Spec("web"); ok {
		t.Errorf("refused deploy should not apply to the cluster")
	}
}

// TestDeployAppDeployGuardrailHolds confirms the app.deploy guardrail can hold a deploy for
// confirmation: unconfirmed it is held (nothing applied), confirmed it proceeds (ADR-0007).
func TestDeployAppDeployGuardrailHolds(t *testing.T) {
	ctx := context.Background()
	// Deploy defaults to allow; an operator raises it to confirm to require sign-off.
	e, k, _, _ := newEngine(t, cp.DefaultPolicy().With(cp.GuardrailAppDeploy, cp.DispositionConfirm))

	// Held for confirmation: the deploy does not happen.
	_, err := e.Deploy(ctx, cp.DeployRequest{App: "web", Image: "img:1", Replicas: 1})
	mustGuardrail(t, err, cp.GuardrailAppDeploy)
	if g, _ := cp.AsGuardrail(err); !g.NeedsConfirmation {
		t.Errorf("NeedsConfirmation = false, want true")
	}
	if _, ok := k.Spec("web"); ok {
		t.Errorf("held deploy should not apply to the cluster")
	}

	// With confirmation it proceeds.
	if _, err := e.Deploy(ctx, cp.DeployRequest{App: "web", Image: "img:1", Replicas: 1, Confirm: true}); err != nil {
		t.Fatalf("confirmed deploy: %v", err)
	}
	if spec, ok := k.Spec("web"); !ok || spec.Image != "img:1" {
		t.Errorf("confirmed deploy should apply img:1, got %+v ok=%v", spec, ok)
	}
}

// TestDeployAppDeployGuardrailDenies confirms a deny disposition refuses the deploy outright, even
// with confirm set (ADR-0020).
func TestDeployAppDeployGuardrailDenies(t *testing.T) {
	ctx := context.Background()
	e, k, _, _ := newEngine(t, cp.DefaultPolicy().With(cp.GuardrailAppDeploy, cp.DispositionDeny))

	_, err := e.Deploy(ctx, cp.DeployRequest{App: "web", Image: "img:1", Replicas: 1, Confirm: true})
	mustGuardrail(t, err, cp.GuardrailAppDeploy)
	if g, _ := cp.AsGuardrail(err); g.NeedsConfirmation {
		t.Errorf("NeedsConfirmation = true, want false for a deny")
	}
	if _, ok := k.Spec("web"); ok {
		t.Errorf("denied deploy should not apply to the cluster")
	}
}

func TestDeploySupersedesPrevious(t *testing.T) {
	ctx := context.Background()
	e, k, d, _ := newEngine(t, permissive())

	v1, err := e.Deploy(ctx, cp.DeployRequest{App: "web", Image: "img:1", Replicas: 1})
	if err != nil {
		t.Fatalf("deploy v1: %v", err)
	}
	v2, err := e.Deploy(ctx, cp.DeployRequest{App: "web", Image: "img:2", Replicas: 1})
	if err != nil {
		t.Fatalf("deploy v2: %v", err)
	}

	if v2.Release.Supersedes != v1.Release.ID {
		t.Errorf("v2.Supersedes = %q, want %q", v2.Release.Supersedes, v1.Release.ID)
	}
	if v2.SupersededReleaseID != v1.Release.ID {
		t.Errorf("v2.SupersededReleaseID = %q, want %q", v2.SupersededReleaseID, v1.Release.ID)
	}
	// v1 now superseded, v2 running.
	old, _ := d.Release(ctx, v1.Release.ID)
	if old.Status != cp.ReleaseSuperseded {
		t.Errorf("v1 status = %q, want superseded", old.Status)
	}
	if spec, _ := k.Spec("web"); spec.Image != "img:2" {
		t.Errorf("cluster image = %q, want img:2", spec.Image)
	}
}

func TestStatus(t *testing.T) {
	ctx := context.Background()
	e, _, _, _ := newEngine(t, permissive())
	_, _ = e.Deploy(ctx, cp.DeployRequest{App: "web", Image: "img:1", Replicas: 3})

	st, err := e.Status(ctx, "web", "")
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if !st.HasRelease || !st.Running {
		t.Fatalf("status = %+v, want hasRelease and running", st)
	}
	if st.Workload.DesiredReplicas != 3 || !st.Workload.Available {
		t.Errorf("deployment = %+v, want desired 3 available", st.Workload)
	}
	if st.Release.Image != "img:1" {
		t.Errorf("release image = %q, want img:1", st.Release.Image)
	}
}

func TestStatusImagePullIssue(t *testing.T) {
	ctx := context.Background()
	e, k, _, _ := newEngine(t, permissive())
	if _, err := e.Deploy(ctx, cp.DeployRequest{App: "web", Image: "ghcr.io/burrow-cloud/website:0.1.1", Replicas: 1}); err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	// The cluster cannot pull the private image: the pod lands in ImagePullBackOff.
	k.SetImagePullFailure("web", cp.ReasonImagePullBackOff)

	st, err := e.Status(ctx, "web", "")
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if st.Workload.Available {
		t.Fatalf("workload = %+v, want not available", st.Workload)
	}
	if st.Workload.IssueReason != cp.ReasonImagePullBackOff {
		t.Errorf("issue reason = %q, want %q", st.Workload.IssueReason, cp.ReasonImagePullBackOff)
	}
	for _, want := range []string{`registry "ghcr.io"`, "burrow config registry login ghcr.io"} {
		if !strings.Contains(st.Workload.Issue, want) {
			t.Errorf("issue = %q, want it to contain %q", st.Workload.Issue, want)
		}
	}
}

// TestStatusImagePullNotFound checks that when the kubelet's waiting message reports the image is
// absent (a wrong or unpushed tag) rather than a credential failure, the Issue points at the tag,
// not the login command (ADR-0040 §4).
func TestStatusImagePullNotFound(t *testing.T) {
	ctx := context.Background()
	e, k, _, _ := newEngine(t, permissive())
	if _, err := e.Deploy(ctx, cp.DeployRequest{App: "web", Image: "ghcr.io/burrow-cloud/website:9.9.9", Replicas: 1}); err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	k.SetImagePullFailureMessage("web", cp.ReasonErrImagePull, "manifest for ghcr.io/burrow-cloud/website:9.9.9 not found: manifest unknown")

	st, err := e.Status(ctx, "web", "")
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if !strings.Contains(st.Workload.Issue, "check the tag") {
		t.Errorf("issue = %q, want it to mention checking the tag", st.Workload.Issue)
	}
	if strings.Contains(st.Workload.Issue, "burrow config registry login") {
		t.Errorf("issue = %q, should not suggest login for a not-found image", st.Workload.Issue)
	}
}

func TestStatusHealthyNoIssue(t *testing.T) {
	ctx := context.Background()
	e, _, _, _ := newEngine(t, permissive())
	if _, err := e.Deploy(ctx, cp.DeployRequest{App: "web", Image: "img:1", Replicas: 2}); err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	st, err := e.Status(ctx, "web", "")
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if !st.Workload.Available {
		t.Fatalf("workload = %+v, want available", st.Workload)
	}
	if st.Workload.Issue != "" || st.Workload.IssueReason != "" {
		t.Errorf("healthy workload carries issue = %q / %q, want empty", st.Workload.Issue, st.Workload.IssueReason)
	}
}

func TestStatusUnknownApp(t *testing.T) {
	e, _, _, _ := newEngine(t, permissive())
	if _, err := e.Status(context.Background(), "ghost", ""); !errors.Is(err, cp.ErrNotFound) {
		t.Fatalf("Status(ghost) err = %v, want ErrNotFound", err)
	}
}

func TestLogs(t *testing.T) {
	ctx := context.Background()
	e, k, _, _ := newEngine(t, permissive())
	_, _ = e.Deploy(ctx, cp.DeployRequest{App: "web", Image: "img:1", Replicas: 1})
	k.SetLogs("web", []cp.LogLine{{Pod: "web-1", Message: "hello"}})

	lines, err := e.Logs(ctx, "web", "", cp.LogOptions{})
	if err != nil || len(lines) != 1 || lines[0].Message != "hello" {
		t.Fatalf("Logs = %+v, err=%v", lines, err)
	}
	if _, err := e.Logs(ctx, "ghost", "", cp.LogOptions{}); !errors.Is(err, cp.ErrNotFound) {
		t.Fatalf("Logs(ghost) err = %v, want ErrNotFound", err)
	}
}

func TestScale(t *testing.T) {
	ctx := context.Background()
	e, k, _, _ := newEngine(t, cp.Policy{MaxReplicas: 10}.With(cp.GuardrailAppDeploy, cp.DispositionAllow))
	if _, err := e.Deploy(ctx, cp.DeployRequest{App: "web", Image: "img:1", Replicas: 2}); err != nil {
		t.Fatalf("Deploy: %v", err)
	}

	res, err := e.Scale(ctx, "web", "", 5, false)
	if err != nil {
		t.Fatalf("Scale: %v", err)
	}
	if res.PreviousReplicas != 2 || res.Replicas != 5 {
		t.Errorf("scale result = %+v, want prev 2 new 5", res)
	}
	if st, _ := k.WorkloadStatus(ctx, "web"); st.DesiredReplicas != 5 {
		t.Errorf("cluster desired = %d, want 5", st.DesiredReplicas)
	}

	// Guardrails apply to scale too.
	_, err = e.Scale(ctx, "web", "", 0, false)
	mustGuardrail(t, err, cp.GuardrailScaleToZero)
	_, err = e.Scale(ctx, "web", "", 99, false)
	mustGuardrail(t, err, cp.GuardrailReplicaCeiling)
	// Unknown app.
	if _, err := e.Scale(ctx, "ghost", "", 3, false); !errors.Is(err, cp.ErrNotFound) {
		t.Errorf("scale ghost err = %v, want ErrNotFound", err)
	}
}

// TestPolicyReadLive proves the engine reads the guardrail policy from the database on each
// operation, so a `guard set` takes effect without a restart (ADR-0020).
func TestPolicyReadLive(t *testing.T) {
	ctx := context.Background()
	e, _, d, _ := newEngine(t, permissive())
	if _, err := e.Deploy(ctx, cp.DeployRequest{App: "web", Image: "img:1", Replicas: 2}); err != nil {
		t.Fatalf("deploy: %v", err)
	}
	// Tighten the policy at runtime; the next operation must observe it.
	d.SetPolicy(cp.Policy{MaxReplicas: 1})
	_, err := e.Scale(ctx, "web", "", 5, false)
	mustGuardrail(t, err, cp.GuardrailReplicaCeiling)
}

func TestGuardrailsListAndSet(t *testing.T) {
	ctx := context.Background()
	e, _, _, _ := newEngine(t, cp.DefaultPolicy())

	gs, err := e.Guardrails(ctx, "")
	if err != nil {
		t.Fatalf("Guardrails: %v", err)
	}
	got := map[cp.GuardrailCode]cp.Disposition{}
	for _, g := range gs {
		got[g.Code] = g.Disposition
	}
	if got[cp.GuardrailReplicaCeiling] != cp.DispositionDeny || got[cp.GuardrailScaleToZero] != cp.DispositionConfirm {
		t.Errorf("default dispositions = %v, want ceiling=deny app.scale_to_zero=confirm", got)
	}

	// A valid set is reflected on the next list.
	if err := e.SetGuardrail(ctx, "", cp.GuardrailScaleToZero, cp.DispositionAllow); err != nil {
		t.Fatalf("SetGuardrail: %v", err)
	}
	gs, _ = e.Guardrails(ctx, "")
	for _, g := range gs {
		if g.Code == cp.GuardrailScaleToZero && g.Disposition != cp.DispositionAllow {
			t.Errorf("after set, app.scale_to_zero = %q, want allow", g.Disposition)
		}
	}

	// Unknown guardrail and invalid disposition are rejected as ErrInvalid.
	if err := e.SetGuardrail(ctx, "", "nonsense", cp.DispositionAllow); !errors.Is(err, cp.ErrInvalid) {
		t.Errorf("unknown guardrail err = %v, want ErrInvalid", err)
	}
	if err := e.SetGuardrail(ctx, "", cp.GuardrailScaleToZero, "maybe"); !errors.Is(err, cp.ErrInvalid) {
		t.Errorf("invalid disposition err = %v, want ErrInvalid", err)
	}
}

func TestExpose(t *testing.T) {
	ctx := context.Background()
	e, k, _, _ := newEngine(t, cp.DefaultPolicy())

	// Exposing before deploy is ErrNotFound (confirm to get past the expose guardrail).
	if _, err := e.Expose(ctx, cp.ExposeRequest{App: "web", Host: "web.example.com", Port: 8080, Confirm: true}); !errors.Is(err, cp.ErrNotFound) {
		t.Fatalf("expose before deploy = %v, want ErrNotFound", err)
	}
	if _, err := e.Deploy(ctx, cp.DeployRequest{App: "web", Image: "img:1", Replicas: 1}); err != nil {
		t.Fatalf("deploy: %v", err)
	}

	// Without confirm, the app.expose_public guardrail holds it for confirmation (the default).
	_, err := e.Expose(ctx, cp.ExposeRequest{App: "web", Host: "web.example.com", Port: 8080})
	if g, ok := cp.AsGuardrail(err); !ok || g.Code != cp.GuardrailExposePublic || !g.NeedsConfirmation {
		t.Fatalf("expose without confirm = %v, want app.expose_public needs-confirmation", err)
	}

	// With confirm it proceeds and records the exposure.
	res, err := e.Expose(ctx, cp.ExposeRequest{App: "web", Host: "web.example.com", Port: 8080, Confirm: true})
	if err != nil {
		t.Fatalf("expose confirmed: %v", err)
	}
	if res.Host != "web.example.com" || res.URL != "http://web.example.com" {
		t.Errorf("expose result = %+v", res)
	}
	if exp, ok := k.Exposure("web"); !ok || exp.Host != "web.example.com" || exp.Port != 8080 {
		t.Errorf("recorded exposure = %+v ok=%v", exp, ok)
	}

	// Unexpose removes it; a second unexpose is ErrNotFound.
	if err := e.Unexpose(ctx, "web", ""); err != nil {
		t.Fatalf("unexpose: %v", err)
	}
	if err := e.Unexpose(ctx, "web", ""); !errors.Is(err, cp.ErrNotFound) {
		t.Errorf("second unexpose = %v, want ErrNotFound", err)
	}
}

func TestExposeTLS(t *testing.T) {
	ctx := context.Background()
	e, _, _, _ := newEngine(t, cp.DefaultPolicy().With(cp.GuardrailExposePublic, cp.DispositionAllow))
	if _, err := e.Deploy(ctx, cp.DeployRequest{App: "web", Image: "img:1", Replicas: 1}); err != nil {
		t.Fatalf("deploy: %v", err)
	}

	res, err := e.Expose(ctx, cp.ExposeRequest{App: "web", Host: "web.example.com", Port: 8080, TLS: true, Issuer: "letsencrypt"})
	if err != nil {
		t.Fatalf("expose tls: %v", err)
	}
	if res.URL != "https://web.example.com" {
		t.Errorf("URL = %q, want https://web.example.com", res.URL)
	}
	if rr, _ := e.Reachability(ctx, "web", ""); !rr.TLS {
		t.Errorf("reachability TLS = false, want true")
	}

	// TLS without an issuer is rejected.
	if _, err := e.Expose(ctx, cp.ExposeRequest{App: "web", Host: "web.example.com", Port: 8080, TLS: true}); !errors.Is(err, cp.ErrInvalid) {
		t.Errorf("TLS without issuer err = %v, want ErrInvalid", err)
	}
}

func TestReachability(t *testing.T) {
	ctx := context.Background()
	k := fake.NewKubernetes()
	dns := fake.NewResolver()
	e, err := cp.New(cp.Deps{
		Kubernetes: k, Database: fake.NewDatabase(),
		Clock: fake.NewClock(time.Now()), IDs: fake.NewIDs(), Resolver: dns,
		Credentials: fake.NewCredentials(), DNS: fake.NewDNSFactory(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// Not deployed.
	if r, _ := e.Reachability(ctx, "web", ""); r.Deployed || r.Reachable || !strings.Contains(r.Summary, "not deployed") {
		t.Errorf("not-deployed = %+v", r)
	}

	if _, err := e.Deploy(ctx, cp.DeployRequest{App: "web", Image: "img:1", Replicas: 1}); err != nil {
		t.Fatalf("deploy: %v", err)
	}

	// Deployed and ready, but not exposed.
	if r, _ := e.Reachability(ctx, "web", ""); !r.Ready || r.Exposed || !strings.Contains(r.Summary, "not exposed") {
		t.Errorf("not-exposed = %+v", r)
	}

	// Exposed, but no external address assigned yet.
	if err := k.Expose(ctx, cp.ExposeSpec{App: "web", Host: "web.example.com", Port: 8080}); err != nil {
		t.Fatalf("expose: %v", err)
	}
	if r, _ := e.Reachability(ctx, "web", ""); !r.Exposed || r.Address != "" || !strings.Contains(r.Summary, "no external address") {
		t.Errorf("no-address = %+v", r)
	}

	// Address assigned, but DNS points elsewhere.
	k.SetIngressAddress("web", "1.2.3.4")
	dns.Set("web.example.com", "9.9.9.9")
	if r, _ := e.Reachability(ctx, "web", ""); r.DNSPointsAtCluster || !strings.Contains(r.Summary, "doesn't point at the cluster") {
		t.Errorf("dns-mismatch = %+v", r)
	}

	// DNS points at the cluster → reachable.
	dns.Set("web.example.com", "1.2.3.4")
	if r, _ := e.Reachability(ctx, "web", ""); !r.Reachable || !r.DNSPointsAtCluster || !strings.Contains(r.Summary, "reachable at http://web.example.com") {
		t.Errorf("reachable = %+v", r)
	}
}

// TestReachabilityVerdict checks the converged verdict (Reachable, URL, BlockedOn): BlockedOn
// names the first unready link, Reachable is true only when every link is green, and URL's
// scheme follows whether TLS was requested.
func TestReachabilityVerdict(t *testing.T) {
	ctx := context.Background()
	k := fake.NewKubernetes()
	dns := fake.NewResolver()
	e, err := cp.New(cp.Deps{
		Kubernetes: k, Database: fake.NewDatabase(),
		Clock: fake.NewClock(time.Now()), IDs: fake.NewIDs(), Resolver: dns,
		Credentials: fake.NewCredentials(), DNS: fake.NewDNSFactory(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// Not deployed → blocked on the deployment, not reachable, no URL.
	if r, _ := e.Reachability(ctx, "web", ""); r.Reachable || r.URL != "" || r.BlockedOn != "deployment" {
		t.Errorf("not-deployed verdict = {reachable:%v url:%q blocked:%q}", r.Reachable, r.URL, r.BlockedOn)
	}

	if _, err := e.Deploy(ctx, cp.DeployRequest{App: "web", Image: "img:1", Replicas: 1}); err != nil {
		t.Fatalf("deploy: %v", err)
	}

	// Deployed and ready but not exposed → blocked on the ingress (routing).
	if r, _ := e.Reachability(ctx, "web", ""); r.Reachable || r.BlockedOn != "ingress" {
		t.Errorf("not-exposed verdict = {reachable:%v blocked:%q}", r.Reachable, r.BlockedOn)
	}

	// Exposed with TLS requested, but no external address yet → blocked on the ingress controller.
	if err := k.Expose(ctx, cp.ExposeSpec{App: "web", Host: "web.example.com", Port: 8080, TLS: true, Issuer: "letsencrypt"}); err != nil {
		t.Fatalf("expose: %v", err)
	}
	if r, _ := e.Reachability(ctx, "web", ""); r.Reachable || r.BlockedOn != "ingress controller" {
		t.Errorf("no-address verdict = {reachable:%v blocked:%q}", r.Reachable, r.BlockedOn)
	}

	// Address assigned, but the TLS certificate has not been issued → blocked on the certificate.
	k.SetIngressAddress("web", "1.2.3.4")
	dns.Set("web.example.com", "1.2.3.4")
	if r, _ := e.Reachability(ctx, "web", ""); r.Reachable || r.BlockedOn != "tls certificate" {
		t.Errorf("no-cert verdict = {reachable:%v blocked:%q}", r.Reachable, r.BlockedOn)
	}

	// Certificate issued and every other link green → reachable at an https URL.
	k.SetCertReady("web", true)
	if r, _ := e.Reachability(ctx, "web", ""); !r.Reachable || r.BlockedOn != "" || r.URL != "https://web.example.com" {
		t.Errorf("reachable verdict = {reachable:%v url:%q blocked:%q}", r.Reachable, r.URL, r.BlockedOn)
	}

	// DNS drifting off the cluster blocks reachability again, now on dns.
	dns.Set("web.example.com", "9.9.9.9")
	if r, _ := e.Reachability(ctx, "web", ""); r.Reachable || r.BlockedOn != "dns" {
		t.Errorf("dns-drift verdict = {reachable:%v blocked:%q}", r.Reachable, r.BlockedOn)
	}
}

// TestReachabilityVerdictHTTP checks that a non-TLS app's live URL uses the http scheme.
func TestReachabilityVerdictHTTP(t *testing.T) {
	ctx := context.Background()
	k := fake.NewKubernetes()
	dns := fake.NewResolver()
	e, err := cp.New(cp.Deps{
		Kubernetes: k, Database: fake.NewDatabase(),
		Clock: fake.NewClock(time.Now()), IDs: fake.NewIDs(), Resolver: dns,
		Credentials: fake.NewCredentials(), DNS: fake.NewDNSFactory(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := e.Deploy(ctx, cp.DeployRequest{App: "web", Image: "img:1", Replicas: 1}); err != nil {
		t.Fatalf("deploy: %v", err)
	}
	if err := k.Expose(ctx, cp.ExposeSpec{App: "web", Host: "web.example.com", Port: 8080}); err != nil {
		t.Fatalf("expose: %v", err)
	}
	k.SetIngressAddress("web", "1.2.3.4")
	dns.Set("web.example.com", "1.2.3.4")
	// No TLS requested, so cert readiness is irrelevant and the URL is http.
	if r, _ := e.Reachability(ctx, "web", ""); !r.Reachable || r.URL != "http://web.example.com" {
		t.Errorf("http verdict = {reachable:%v url:%q}", r.Reachable, r.URL)
	}
}

func TestRollback(t *testing.T) {
	ctx := context.Background()
	e, k, d, _ := newEngine(t, permissive())
	v1, _ := e.Deploy(ctx, cp.DeployRequest{App: "web", Image: "img:1", Replicas: 1})
	v2, _ := e.Deploy(ctx, cp.DeployRequest{App: "web", Image: "img:2", Replicas: 1})

	res, err := e.Rollback(ctx, "web", "", false)
	if err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if res.RolledBackToReleaseID != v1.Release.ID {
		t.Errorf("rolled back to %q, want %q", res.RolledBackToReleaseID, v1.Release.ID)
	}
	if res.SupersededReleaseID != v2.Release.ID {
		t.Errorf("superseded %q, want %q", res.SupersededReleaseID, v2.Release.ID)
	}
	if res.Release.Image != "img:1" {
		t.Errorf("rollback release image = %q, want img:1 (the prior reference)", res.Release.Image)
	}
	// Cluster restored to img:1.
	if spec, _ := k.Spec("web"); spec.Image != "img:1" {
		t.Errorf("cluster image = %q, want img:1", spec.Image)
	}
	// v2 now superseded.
	old, _ := d.Release(ctx, v2.Release.ID)
	if old.Status != cp.ReleaseSuperseded {
		t.Errorf("v2 status = %q, want superseded", old.Status)
	}
}

func TestRollbackNothingToRollBack(t *testing.T) {
	ctx := context.Background()
	e, _, _, _ := newEngine(t, permissive())

	// No releases at all.
	if _, err := e.Rollback(ctx, "web", "", false); !errors.Is(err, cp.ErrNotFound) {
		t.Fatalf("rollback with no releases err = %v, want ErrNotFound", err)
	}
	// A single deploy has no prior to roll back to.
	_, _ = e.Deploy(ctx, cp.DeployRequest{App: "web", Image: "img:1", Replicas: 1})
	if _, err := e.Rollback(ctx, "web", "", false); !errors.Is(err, cp.ErrNotFound) {
		t.Fatalf("rollback with one release err = %v, want ErrNotFound", err)
	}
}

func TestRollbackGuardrailHolds(t *testing.T) {
	ctx := context.Background()
	// Rollback defaults to allow, but an operator can raise it to confirm for sign-off.
	e, k, _, _ := newEngine(t, cp.DefaultPolicy().With(cp.GuardrailRollback, cp.DispositionConfirm))
	v1, _ := e.Deploy(ctx, cp.DeployRequest{App: "web", Image: "img:1", Replicas: 1})
	_, _ = e.Deploy(ctx, cp.DeployRequest{App: "web", Image: "img:2", Replicas: 1})

	// Held for confirmation: the rollback does not happen, and the cluster keeps img:2.
	_, err := e.Rollback(ctx, "web", "", false)
	mustGuardrail(t, err, cp.GuardrailRollback)
	if g, _ := cp.AsGuardrail(err); !g.NeedsConfirmation {
		t.Errorf("NeedsConfirmation = false, want true")
	}
	if spec, _ := k.Spec("web"); spec.Image != "img:2" {
		t.Errorf("cluster image = %q, want img:2 (held rollback must not change it)", spec.Image)
	}

	// With confirmation it proceeds and restores img:1.
	res, err := e.Rollback(ctx, "web", "", true)
	if err != nil {
		t.Fatalf("confirmed rollback: %v", err)
	}
	if res.RolledBackToReleaseID != v1.Release.ID {
		t.Errorf("rolled back to %q, want %q", res.RolledBackToReleaseID, v1.Release.ID)
	}
	if spec, _ := k.Spec("web"); spec.Image != "img:1" {
		t.Errorf("cluster image = %q, want img:1", spec.Image)
	}
}

func TestRollbackGuardrailDenies(t *testing.T) {
	ctx := context.Background()
	e, _, _, _ := newEngine(t, cp.DefaultPolicy().With(cp.GuardrailRollback, cp.DispositionDeny))
	_, _ = e.Deploy(ctx, cp.DeployRequest{App: "web", Image: "img:1", Replicas: 1})
	_, _ = e.Deploy(ctx, cp.DeployRequest{App: "web", Image: "img:2", Replicas: 1})

	// A deny refuses outright — even with confirm, it does not proceed.
	_, err := e.Rollback(ctx, "web", "", true)
	mustGuardrail(t, err, cp.GuardrailRollback)
	if g, _ := cp.AsGuardrail(err); g.NeedsConfirmation {
		t.Errorf("NeedsConfirmation = true, want false for a deny")
	}
}

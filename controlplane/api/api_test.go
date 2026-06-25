// SPDX-License-Identifier: FSL-1.1-ALv2
// Copyright 2026 Nicholas Phillips

package api_test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	cp "github.com/burrow-cloud/burrow/controlplane"
	"github.com/burrow-cloud/burrow/controlplane/api"
	"github.com/burrow-cloud/burrow/controlplane/internal/fake"
)

const token = "secret-token"

func newAPI(t *testing.T) (http.Handler, *fake.Kubernetes, *fake.Registry, *fake.Database) {
	t.Helper()
	k, r, d := fake.NewKubernetes(), fake.NewRegistry(), fake.NewDatabase()
	d.SetPolicy(cp.Policy{MaxReplicas: 5})
	e, err := cp.New(cp.Deps{
		Kubernetes: k, Registry: r, Database: d,
		Clock: fake.NewClock(time.Date(2026, 6, 23, 12, 0, 0, 0, time.UTC)),
		IDs:   fake.NewIDs(),
	})
	if err != nil {
		t.Fatalf("engine: %v", err)
	}
	h, err := api.New(api.Config{Engine: e, Token: token})
	if err != nil {
		t.Fatalf("api.New: %v", err)
	}
	return h, k, r, d
}

func TestGuardEndpoints(t *testing.T) {
	h, _, _, _ := newAPI(t)

	if rr := do(h, "GET", "/v1/guard", token, ""); rr.Code != 200 || !strings.Contains(rr.Body.String(), "scale_to_zero") {
		t.Fatalf("guard list = %d %s", rr.Code, rr.Body.String())
	}

	rr := do(h, "PUT", "/v1/guard/scale_to_zero", token, `{"disposition":"allow"}`)
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), `"disposition":"allow"`) {
		t.Fatalf("guard set = %d %s", rr.Code, rr.Body.String())
	}

	// Invalid disposition and unknown guardrail are rejected (ErrInvalid -> 400).
	if rr := do(h, "PUT", "/v1/guard/scale_to_zero", token, `{"disposition":"nope"}`); rr.Code != 400 {
		t.Errorf("invalid disposition code = %d, want 400", rr.Code)
	}
	if rr := do(h, "PUT", "/v1/guard/bogus", token, `{"disposition":"allow"}`); rr.Code != 400 {
		t.Errorf("unknown guardrail code = %d, want 400", rr.Code)
	}
}

func do(h http.Handler, method, path, tok, body string) *httptest.ResponseRecorder {
	var br io.Reader
	if body != "" {
		br = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, br)
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

type errBody struct {
	Error string `json:"error"`
	Code  string `json:"code"`
	Limit *int32 `json:"limit"`
}

func TestHealthNoAuth(t *testing.T) {
	h, _, _, _ := newAPI(t)
	rec := do(h, "GET", "/healthz", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("health status = %d, want 200", rec.Code)
	}
}

func TestAuthRequired(t *testing.T) {
	h, _, _, _ := newAPI(t)
	if rec := do(h, "GET", "/v1/apps/web/status", "", ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("no token: status = %d, want 401", rec.Code)
	}
	if rec := do(h, "GET", "/v1/apps/web/status", "wrong", ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("wrong token: status = %d, want 401", rec.Code)
	}
}

func TestAuthViaCustomHeader(t *testing.T) {
	h, _, r, _ := newAPI(t)
	r.Add("img:1", "sha256:1")
	// X-Burrow-Token (no Authorization) is accepted — the header that survives the
	// API-server proxy (ADR-0014).
	req := httptest.NewRequest("POST", "/v1/apps/web/deploy", strings.NewReader(`{"image":"img:1","replicas":2}`))
	req.Header.Set("X-Burrow-Token", token)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("X-Burrow-Token auth: status = %d, body = %s", rec.Code, rec.Body.String())
	}
}

func TestDeployHappyPath(t *testing.T) {
	h, k, r, _ := newAPI(t)
	r.Add("registry.example.com/web:1", "sha256:web1")

	rec := do(h, "POST", "/v1/apps/web/deploy", token, `{"image":"registry.example.com/web:1","replicas":2,"env":{"K":"V"}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("deploy status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var res cp.DeployResult
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if res.Release.Status != cp.ReleaseDeployed {
		t.Errorf("release status = %q, want deployed", res.Release.Status)
	}
	if res.Release.App != "web" {
		t.Errorf("release app = %q, want web (from the path)", res.Release.App)
	}
	if res.Release.Digest != "sha256:web1" {
		t.Errorf("digest = %q, want sha256:web1", res.Release.Digest)
	}
	if spec, ok := k.Spec("web"); !ok || spec.Image != "registry.example.com/web:1" || spec.Replicas != 2 {
		t.Errorf("cluster spec = %+v ok=%v", spec, ok)
	}
}

func TestDeployImageNotFound(t *testing.T) {
	h, _, _, _ := newAPI(t)
	rec := do(h, "POST", "/v1/apps/web/deploy", token, `{"image":"missing:1","replicas":1}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body = %s", rec.Code, rec.Body.String())
	}
}

func TestDeployBadRequest(t *testing.T) {
	h, _, _, _ := newAPI(t)
	// Missing image is a malformed request.
	rec := do(h, "POST", "/v1/apps/web/deploy", token, `{"replicas":1}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body = %s", rec.Code, rec.Body.String())
	}
	// Invalid JSON is also 400.
	if rec := do(h, "POST", "/v1/apps/web/deploy", token, `{not json`); rec.Code != http.StatusBadRequest {
		t.Errorf("invalid JSON status = %d, want 400", rec.Code)
	}
}

func TestDeployGuardrailCeiling(t *testing.T) {
	h, _, r, _ := newAPI(t)
	r.Add("img:1", "sha256:1")
	rec := do(h, "POST", "/v1/apps/web/deploy", token, `{"image":"img:1","replicas":9}`)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body = %s", rec.Code, rec.Body.String())
	}
	var e errBody
	_ = json.Unmarshal(rec.Body.Bytes(), &e)
	if e.Code != string(cp.GuardrailReplicaCeiling) {
		t.Errorf("code = %q, want %q", e.Code, cp.GuardrailReplicaCeiling)
	}
	if e.Limit == nil || *e.Limit != 5 {
		t.Errorf("limit = %v, want 5", e.Limit)
	}
}

func TestStatus(t *testing.T) {
	h, _, r, _ := newAPI(t)
	r.Add("img:1", "sha256:1")
	do(h, "POST", "/v1/apps/web/deploy", token, `{"image":"img:1","replicas":3}`)

	rec := do(h, "GET", "/v1/apps/web/status", token, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var res cp.StatusResult
	_ = json.Unmarshal(rec.Body.Bytes(), &res)
	if !res.HasRelease || !res.Running || res.Workload.DesiredReplicas != 3 {
		t.Errorf("status result = %+v", res)
	}
}

func TestStatusUnknown(t *testing.T) {
	h, _, _, _ := newAPI(t)
	if rec := do(h, "GET", "/v1/apps/ghost/status", token, ""); rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestScaleAndGuardrail(t *testing.T) {
	h, _, r, _ := newAPI(t)
	r.Add("img:1", "sha256:1")
	do(h, "POST", "/v1/apps/web/deploy", token, `{"image":"img:1","replicas":2}`)

	rec := do(h, "POST", "/v1/apps/web/scale", token, `{"replicas":4}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("scale status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var res cp.ScaleResult
	_ = json.Unmarshal(rec.Body.Bytes(), &res)
	if res.PreviousReplicas != 2 || res.Replicas != 4 {
		t.Errorf("scale result = %+v, want prev 2 new 4", res)
	}

	// Scale to zero is refused by policy.
	if rec := do(h, "POST", "/v1/apps/web/scale", token, `{"replicas":0}`); rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("scale-to-zero status = %d, want 422", rec.Code)
	}
}

func TestRollback(t *testing.T) {
	h, k, r, _ := newAPI(t)
	r.Add("img:1", "sha256:1")
	r.Add("img:2", "sha256:2")
	do(h, "POST", "/v1/apps/web/deploy", token, `{"image":"img:1","replicas":1}`)
	do(h, "POST", "/v1/apps/web/deploy", token, `{"image":"img:2","replicas":1}`)

	rec := do(h, "POST", "/v1/apps/web/rollback", token, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("rollback status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var res cp.RollbackResult
	_ = json.Unmarshal(rec.Body.Bytes(), &res)
	if res.Release.Image != "img:1" {
		t.Errorf("rollback image = %q, want img:1", res.Release.Image)
	}
	if spec, _ := k.Spec("web"); spec.Image != "img:1" {
		t.Errorf("cluster image = %q, want img:1", spec.Image)
	}
}

func TestLogs(t *testing.T) {
	h, k, r, _ := newAPI(t)
	r.Add("img:1", "sha256:1")
	do(h, "POST", "/v1/apps/web/deploy", token, `{"image":"img:1","replicas":1}`)
	k.SetLogs("web", []cp.LogLine{{Pod: "web-1", Message: "a"}, {Pod: "web-1", Message: "b"}})

	rec := do(h, "GET", "/v1/apps/web/logs?tail=1", token, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("logs status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var res struct {
		Lines []cp.LogLine `json:"lines"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &res)
	if len(res.Lines) != 1 || res.Lines[0].Message != "b" {
		t.Errorf("lines = %+v, want last line b", res.Lines)
	}
}

func TestNotImplementedMapsTo501(t *testing.T) {
	h, k, r, _ := newAPI(t)
	r.Add("img:1", "sha256:1")
	// An adapter that is not wired yet surfaces ErrNotImplemented; the API reports 501.
	k.SetError(fake.OpApply, fmt.Errorf("cluster adapter: %w", cp.ErrNotImplemented))
	rec := do(h, "POST", "/v1/apps/web/deploy", token, `{"image":"img:1","replicas":1}`)
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501; body = %s", rec.Code, rec.Body.String())
	}
}

func TestMethodNotAllowed(t *testing.T) {
	h, _, _, _ := newAPI(t)
	// GET on a POST-only route — the mux returns 405.
	if rec := do(h, "GET", "/v1/apps/web/deploy", token, ""); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", rec.Code)
	}
}

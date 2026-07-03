// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package api_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/burrow-cloud/burrow/client"
	cp "github.com/burrow-cloud/burrow/controlplane"
	"github.com/burrow-cloud/burrow/controlplane/api"
	"github.com/burrow-cloud/burrow/controlplane/internal/fake"
)

const token = "secret-token"

func newAPI(t *testing.T) (http.Handler, *fake.Kubernetes, *fake.Database) {
	t.Helper()
	k, d := fake.NewKubernetes(), fake.NewDatabase()
	// A restrictive baseline (empty dispositions → deny) so guardrail tests opt in explicitly,
	// but rollback and deploy have a product default of allow, so seed those to match production
	// (deploy is the core action and is what the setup `do(... /deploy ...)` calls exercise).
	d.SetPolicy(cp.Policy{MaxReplicas: 5}.
		With(cp.GuardrailRollback, cp.DispositionAllow).
		With(cp.GuardrailAppDeploy, cp.DispositionAllow))
	e, err := cp.New(cp.Deps{
		Kubernetes: k, Database: d,
		Clock:       fake.NewClock(time.Date(2026, 6, 23, 12, 0, 0, 0, time.UTC)),
		IDs:         fake.NewIDs(),
		Resolver:    fake.NewResolver(),
		Credentials: fake.NewCredentials(),
		DNS:         fake.NewDNSFactory(),
	})
	if err != nil {
		t.Fatalf("engine: %v", err)
	}
	h, err := api.New(api.Config{Engine: e, Token: token})
	if err != nil {
		t.Fatalf("api.New: %v", err)
	}
	return h, k, d
}

func TestGuardEndpoints(t *testing.T) {
	h, _, _ := newAPI(t)

	if rr := do(h, "GET", "/v1/guard", token, ""); rr.Code != 200 || !strings.Contains(rr.Body.String(), "app.scale_to_zero") {
		t.Fatalf("guard list = %d %s", rr.Code, rr.Body.String())
	}

	rr := do(h, "PUT", "/v1/guard/app.scale_to_zero", token, `{"disposition":"allow"}`)
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), `"disposition":"allow"`) {
		t.Fatalf("guard set = %d %s", rr.Code, rr.Body.String())
	}

	// Invalid disposition and unknown guardrail are rejected (ErrInvalid -> 400).
	if rr := do(h, "PUT", "/v1/guard/app.scale_to_zero", token, `{"disposition":"nope"}`); rr.Code != 400 {
		t.Errorf("invalid disposition code = %d, want 400", rr.Code)
	}
	if rr := do(h, "PUT", "/v1/guard/bogus", token, `{"disposition":"allow"}`); rr.Code != 400 {
		t.Errorf("unknown guardrail code = %d, want 400", rr.Code)
	}
}

// TestGuardEndpointsEnvScoped confirms the guard endpoints carry the optional env query through to the
// engine: a registered env scopes the set, an unknown env is 404, and a cluster-level guardrail
// cannot be env-scoped (400) (ADR-0035 phase 2c).
func TestGuardEndpointsEnvScoped(t *testing.T) {
	h, _, d := newAPI(t)
	if err := d.CreateEnvironment(context.Background(), "prod", "burrow-apps-prod"); err != nil {
		t.Fatalf("CreateEnvironment: %v", err)
	}

	// Scope app.delete to prod: the response reflects the env-specific disposition with its source.
	rr := do(h, "PUT", "/v1/guard/app.delete?env=prod", token, `{"disposition":"deny"}`)
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), `"source":"env"`) {
		t.Fatalf("env guard set = %d %s", rr.Code, rr.Body.String())
	}
	// The global policy is untouched: a plain list does not carry the env source.
	if rr := do(h, "GET", "/v1/guard", token, ""); rr.Code != 200 || strings.Contains(rr.Body.String(), `"source"`) {
		t.Errorf("global guard list leaked an env source = %d %s", rr.Code, rr.Body.String())
	}
	// An unknown environment is a 404.
	if rr := do(h, "PUT", "/v1/guard/app.delete?env=ghost", token, `{"disposition":"deny"}`); rr.Code != 404 {
		t.Errorf("unknown env code = %d, want 404", rr.Code)
	}
	if rr := do(h, "GET", "/v1/guard?env=ghost", token, ""); rr.Code != 404 {
		t.Errorf("unknown env list code = %d, want 404", rr.Code)
	}
	// A cluster-level guardrail cannot be env-scoped (400).
	if rr := do(h, "PUT", "/v1/guard/addon.install?env=prod", token, `{"disposition":"deny"}`); rr.Code != 400 {
		t.Errorf("cluster-level env scope code = %d, want 400", rr.Code)
	}
}

// newProviderAPI builds an API whose engine exposes the credential store and DNS factory, so
// the provider-endpoint test can seed the token the CLI would have written and control the
// vendor's verdict.
func newProviderAPI(t *testing.T) (http.Handler, *fake.Credentials, *fake.DNSFactory) {
	t.Helper()
	d := fake.NewDatabase()
	d.SetPolicy(cp.DefaultPolicy()) // dns.write/dns.delete default to confirm
	creds := fake.NewCredentials()
	dnsF := fake.NewDNSFactory()
	e, err := cp.New(cp.Deps{
		Kubernetes: fake.NewKubernetes(), Database: d,
		Clock: fake.NewClock(time.Date(2026, 6, 25, 12, 0, 0, 0, time.UTC)),
		IDs:   fake.NewIDs(), Resolver: fake.NewResolver(),
		Credentials: creds, DNS: dnsF,
	})
	if err != nil {
		t.Fatalf("engine: %v", err)
	}
	h, err := api.New(api.Config{Engine: e, Token: token})
	if err != nil {
		t.Fatalf("api.New: %v", err)
	}
	return h, creds, dnsF
}

func TestProviderEndpoints(t *testing.T) {
	h, creds, dnsF := newProviderAPI(t)

	// Add a provider: the token VALUE travels in the BODY (never the path or query), is validated,
	// then written into the credential store. The response carries the Secret key, never the value.
	rr := do(h, "POST", "/v1/providers", token, `{"type":"digitalocean","token":"dop_v1_tok"}`)
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), `"capabilities":["dns"]`) {
		t.Fatalf("add provider = %d %s", rr.Code, rr.Body.String())
	}
	// The response must NOT echo the token value back.
	if strings.Contains(rr.Body.String(), "dop_v1_tok") {
		t.Errorf("provider-add response leaked the token value: %s", rr.Body.String())
	}
	// burrowd wrote the token into the credential store under the provider key.
	if tok, ok := creds.Get("digitalocean"); !ok || tok != "dop_v1_tok" {
		t.Errorf("credential store has %q ok=%v, want dop_v1_tok true", tok, ok)
	}

	// List shows it.
	if rr := do(h, "GET", "/v1/providers", token, ""); rr.Code != 200 || !strings.Contains(rr.Body.String(), `"name":"digitalocean"`) {
		t.Fatalf("list providers = %d %s", rr.Code, rr.Body.String())
	}

	// An unsupported type is a 400 (ErrInvalid).
	if rr := do(h, "POST", "/v1/providers", token, `{"type":"aws","token":"x"}`); rr.Code != 400 {
		t.Errorf("unknown type code = %d, want 400", rr.Code)
	}

	// A token the vendor rejects is a 400, and nothing is recorded.
	dnsF.SetVerifyError(fmt.Errorf("rejected: %w", cp.ErrInvalid))
	if rr := do(h, "POST", "/v1/providers", token, `{"type":"cloudflare","token":"bad"}`); rr.Code != 400 {
		t.Errorf("rejected token code = %d, want 400", rr.Code)
	}
	if _, ok := creds.Get("cloudflare"); ok {
		t.Errorf("a rejected token must not be written to the credential store")
	}

	// The endpoints require the token like every other /v1 route.
	if rr := do(h, "GET", "/v1/providers", "", ""); rr.Code != 401 {
		t.Errorf("unauthenticated list code = %d, want 401", rr.Code)
	}
}

// TestConnectAddonAuthEndpointTakesTokenInBody connects an authenticated backend and asserts the
// bearer token VALUE travels in the BODY (never the path or query), is written into the credential
// store, and is not echoed back in the response (ADR-0030).
func TestConnectAddonAuthEndpointTakesTokenInBody(t *testing.T) {
	h, creds, _ := newProviderAPI(t)

	rr := do(h, "POST", "/v1/addons/connect", token,
		`{"backend":"loki","endpoint":"loki.svc:3100","secret_key":"addon-loki","token":"s3cr3t"}`)
	if rr.Code != 200 {
		t.Fatalf("connect addon = %d %s", rr.Code, rr.Body.String())
	}
	// The response (the recorded AddonInfo) carries the key, never the token value.
	if strings.Contains(rr.Body.String(), "s3cr3t") {
		t.Errorf("connect response leaked the token value: %s", rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), `"secret_key":"addon-loki"`) {
		t.Errorf("connect response should record the secret key: %s", rr.Body.String())
	}
	// burrowd wrote the token into the credential store under the key.
	if tok, ok := creds.Get("addon-loki"); !ok || tok != "s3cr3t" {
		t.Errorf("credential store has %q ok=%v, want s3cr3t true", tok, ok)
	}
}

func TestDomainEndpoints(t *testing.T) {
	h, _, _ := newProviderAPI(t)
	if rr := do(h, "POST", "/v1/providers", token, `{"type":"digitalocean","token":"tok"}`); rr.Code != 200 {
		t.Fatalf("register provider = %d %s", rr.Code, rr.Body.String())
	}

	// Add with confirm succeeds and reports the inferred record type.
	rr := do(h, "POST", "/v1/domains", token, `{"host":"app.example.com","provider":"digitalocean","address":"203.0.113.5","confirm":true}`)
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), `"type":"A"`) {
		t.Fatalf("add domain = %d %s", rr.Code, rr.Body.String())
	}

	// Without confirm the dns.write guardrail holds it (422, needs confirmation).
	rr = do(h, "POST", "/v1/domains", token, `{"host":"x.example.com","provider":"digitalocean","address":"203.0.113.6"}`)
	if rr.Code != 422 || !strings.Contains(rr.Body.String(), `"needs_confirmation":true`) {
		t.Errorf("unconfirmed add = %d %s, want 422 needs_confirmation", rr.Code, rr.Body.String())
	}

	// Remove via DELETE with provider + confirm in the query.
	if rr := do(h, "DELETE", "/v1/domains/app.example.com?provider=digitalocean&confirm=true", token, ""); rr.Code != 200 {
		t.Errorf("remove domain = %d %s", rr.Code, rr.Body.String())
	}

	// Authenticated like every other /v1 route.
	if rr := do(h, "POST", "/v1/domains", "", `{}`); rr.Code != 401 {
		t.Errorf("unauthenticated add code = %d, want 401", rr.Code)
	}
}

// TestAuditEndpoint exercises the read path: a deploy records audit rows, and GET /v1/audit
// returns them newest-first, with the app/operation/outcome filters applied.
func TestAuditEndpoint(t *testing.T) {
	h, _, _ := newAPI(t)
	if rr := do(h, "POST", "/v1/apps/web/deploy", token, `{"image":"registry.example.com/web:1","replicas":2}`); rr.Code != 200 {
		t.Fatalf("deploy = %d %s", rr.Code, rr.Body.String())
	}

	rec := do(h, "GET", "/v1/audit?app=web", token, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("audit = %d %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Entries []cp.AuditEntry `json:"entries"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out.Entries) != 2 {
		t.Fatalf("audit entries = %d, want 2 (allowed + executed)", len(out.Entries))
	}
	// Newest first: the executed row precedes the allowed decision.
	if out.Entries[0].Outcome != cp.AuditExecuted || out.Entries[1].Outcome != cp.AuditAllowed {
		t.Errorf("outcomes = %s,%s, want executed,allowed (newest first)", out.Entries[0].Outcome, out.Entries[1].Outcome)
	}

	// Outcome filter narrows to one.
	rec = do(h, "GET", "/v1/audit?app=web&outcome=executed", token, "")
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if len(out.Entries) != 1 || out.Entries[0].Outcome != cp.AuditExecuted {
		t.Errorf("outcome filter returned %d rows, want 1 executed", len(out.Entries))
	}

	// A bad limit is a 400.
	if rr := do(h, "GET", "/v1/audit?limit=nope", token, ""); rr.Code != http.StatusBadRequest {
		t.Errorf("bad limit = %d, want 400", rr.Code)
	}
}

// TestEnvironmentEndpointsRoundTrip exercises POST/GET /v1/environments through the typed client
// against a live httptest server (ADR-0035 phase 2a): registering an environment and listing it,
// with the implicit default first.
func TestEnvironmentEndpointsRoundTrip(t *testing.T) {
	d := fake.NewDatabase()
	e, err := cp.New(cp.Deps{
		Kubernetes: fake.NewKubernetes(), Database: d,
		Clock: fake.NewClock(time.Date(2026, 6, 29, 12, 0, 0, 0, time.UTC)),
		IDs:   fake.NewIDs(), Resolver: fake.NewResolver(),
		Credentials: fake.NewCredentials(), DNS: fake.NewDNSFactory(),
		AppNamespace: "burrow-apps",
	})
	if err != nil {
		t.Fatalf("engine: %v", err)
	}
	h, err := api.New(api.Config{Engine: e, Token: token})
	if err != nil {
		t.Fatalf("api.New: %v", err)
	}
	srv := httptest.NewServer(h)
	defer srv.Close()
	c := client.NewClient(srv.URL, token)
	ctx := context.Background()

	if err := c.AddEnvironment(ctx, "staging", "burrow-apps-staging"); err != nil {
		t.Fatalf("AddEnvironment: %v", err)
	}
	// A duplicate is rejected (ErrInvalid -> 400).
	if err := c.AddEnvironment(ctx, "staging", "other"); err == nil {
		t.Errorf("duplicate AddEnvironment should error")
	}

	envs, err := c.ListEnvironments(ctx)
	if err != nil {
		t.Fatalf("ListEnvironments: %v", err)
	}
	if len(envs) != 2 {
		t.Fatalf("environments = %+v, want 2 (default + staging)", envs)
	}
	if envs[0].Name != "default" || !envs[0].Default || envs[0].Namespace != "burrow-apps" {
		t.Errorf("first environment should be the default in the app namespace: %+v", envs[0])
	}
	if envs[1].Name != "staging" || envs[1].Default || envs[1].Namespace != "burrow-apps-staging" {
		t.Errorf("registered environment wrong: %+v", envs[1])
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
	h, _, _ := newAPI(t)
	rec := do(h, "GET", "/healthz", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("health status = %d, want 200", rec.Code)
	}
}

func TestAuthRequired(t *testing.T) {
	h, _, _ := newAPI(t)
	if rec := do(h, "GET", "/v1/apps/web/status", "", ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("no token: status = %d, want 401", rec.Code)
	}
	if rec := do(h, "GET", "/v1/apps/web/status", "wrong", ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("wrong token: status = %d, want 401", rec.Code)
	}
}

func TestAuthViaCustomHeader(t *testing.T) {
	h, _, _ := newAPI(t)
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
	h, k, _ := newAPI(t)

	rec := do(h, "POST", "/v1/apps/web/deploy", token, `{"image":"registry.example.com/web:1","replicas":2}`)
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
	if res.Release.Digest != "" {
		t.Errorf("digest = %q, want empty (burrowd does not resolve; ADR-0040)", res.Release.Digest)
	}
	if spec, ok := k.Spec("web"); !ok || spec.Image != "registry.example.com/web:1" || spec.Replicas != 2 {
		t.Errorf("cluster spec = %+v ok=%v", spec, ok)
	}
}

func TestDeployBadRequest(t *testing.T) {
	h, _, _ := newAPI(t)
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
	h, _, _ := newAPI(t)
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
	h, _, _ := newAPI(t)
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
	h, _, _ := newAPI(t)
	if rec := do(h, "GET", "/v1/apps/ghost/status", token, ""); rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestScaleAndGuardrail(t *testing.T) {
	h, _, _ := newAPI(t)
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

func TestExposeEndpoints(t *testing.T) {
	h, _, d := newAPI(t)
	d.SetPolicy(cp.DefaultPolicy().With(cp.GuardrailExposePublic, cp.DispositionAllow))
	do(h, "POST", "/v1/apps/web/deploy", token, `{"image":"img:1","replicas":1}`)

	rec := do(h, "POST", "/v1/apps/web/expose", token, `{"host":"web.example.com","port":8080}`)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "web.example.com") {
		t.Fatalf("expose = %d %s", rec.Code, rec.Body.String())
	}
	if rec := do(h, "POST", "/v1/apps/web/unexpose", token, ""); rec.Code != http.StatusOK {
		t.Fatalf("unexpose = %d %s", rec.Code, rec.Body.String())
	}
	if rec := do(h, "POST", "/v1/apps/web/unexpose", token, ""); rec.Code != http.StatusNotFound {
		t.Errorf("second unexpose = %d, want 404", rec.Code)
	}
}

func TestReachabilityEndpoint(t *testing.T) {
	h, _, _ := newAPI(t)
	do(h, "POST", "/v1/apps/web/deploy", token, `{"image":"img:1","replicas":1}`)
	rec := do(h, "GET", "/v1/apps/web/reachability", token, "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "not exposed") {
		t.Fatalf("reachability = %d %s", rec.Code, rec.Body.String())
	}
}

func TestExposeGuardrailHolds(t *testing.T) {
	h, _, _ := newAPI(t)
	do(h, "POST", "/v1/apps/web/deploy", token, `{"image":"img:1","replicas":1}`)
	// newAPI leaves app.expose_public unset → deny, so exposure is refused (422 guardrail).
	if rec := do(h, "POST", "/v1/apps/web/expose", token, `{"host":"web.example.com","port":8080}`); rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("expose code = %d, want 422", rec.Code)
	}
}

func TestRollback(t *testing.T) {
	h, k, _ := newAPI(t)
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
	h, k, _ := newAPI(t)
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

func TestConfigEndpoints(t *testing.T) {
	h, k, _ := newAPI(t)
	do(h, "POST", "/v1/apps/web/deploy", token, `{"image":"img:1","replicas":1}`)

	// Set rolls the workload by default: the value reaches the live spec.
	if rec := do(h, "POST", "/v1/apps/web/config", token, `{"key":"LOG_LEVEL","value":"debug"}`); rec.Code != http.StatusOK {
		t.Fatalf("config set = %d %s", rec.Code, rec.Body.String())
	}
	if spec, _ := k.Spec("web"); spec.Env["LOG_LEVEL"] != "debug" {
		t.Errorf("spec env = %+v, want LOG_LEVEL=debug after set", spec.Env)
	}

	// no_restart persists without rolling.
	if rec := do(h, "POST", "/v1/apps/web/config", token, `{"key":"FEATURE","value":"on","no_restart":true}`); rec.Code != http.StatusOK {
		t.Fatalf("config set no_restart = %d %s", rec.Code, rec.Body.String())
	}
	if _, present := func() (string, bool) { s, _ := k.Spec("web"); v, ok := s.Env["FEATURE"]; return v, ok }(); present {
		t.Errorf("FEATURE should not be in the live spec until the next deploy")
	}

	// List round-trips both keys.
	rec := do(h, "GET", "/v1/apps/web/config", token, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("config list = %d %s", rec.Code, rec.Body.String())
	}
	var listed struct {
		Config map[string]string `json:"config"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &listed); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if listed.Config["LOG_LEVEL"] != "debug" || listed.Config["FEATURE"] != "on" {
		t.Errorf("listed config = %+v, want LOG_LEVEL=debug and FEATURE=on", listed.Config)
	}

	// Unset removes a key and rolls.
	if rec := do(h, "DELETE", "/v1/apps/web/config/LOG_LEVEL", token, ""); rec.Code != http.StatusOK {
		t.Fatalf("config unset = %d %s", rec.Code, rec.Body.String())
	}
	if spec, _ := k.Spec("web"); spec.Env["LOG_LEVEL"] != "" {
		t.Errorf("spec env = %+v, want LOG_LEVEL removed", spec.Env)
	}

	// An invalid config key is a 400.
	if rec := do(h, "POST", "/v1/apps/web/config", token, `{"key":"1BAD","value":"x"}`); rec.Code != http.StatusBadRequest {
		t.Errorf("bad key = %d, want 400", rec.Code)
	}
}

func TestSecretEndpoints(t *testing.T) {
	h, k, _ := newAPI(t)
	do(h, "POST", "/v1/apps/web/deploy", token, `{"image":"img:1","replicas":1}`)

	// `secret set` carries a VALUE over the authenticated API; burrowd writes it to the per-app
	// Secret (ADR-0029). Set via the API and assert the value lands in the fake Secret, the
	// response echoes the app+KEY only (never the value), and the running workload rolls.
	rec := do(h, "POST", "/v1/apps/web/secrets", token, `{"key":"STRIPE_KEY","value":"sk_live_x"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("secret set = %d %s", rec.Code, rec.Body.String())
	}
	if b := rec.Body.String(); strings.Contains(b, "sk_live_x") {
		t.Fatalf("secret set response leaked the value: %s", b)
	}
	if v, ok := k.SecretValue("web", "STRIPE_KEY"); !ok || v != "sk_live_x" {
		t.Errorf("STRIPE_KEY in fake Secret = %q, %v; want sk_live_x written via the API", v, ok)
	}
	if _, rolled := k.RestartedAt("web"); !rolled {
		t.Error("default secret set should roll the running workload")
	}
	// A no_restart set writes the value but does not roll. Use a fresh app to reset roll state.
	do(h, "POST", "/v1/apps/noroll/deploy", token, `{"image":"img:1","replicas":1}`)
	if rec := do(h, "POST", "/v1/apps/noroll/secrets", token, `{"key":"K","value":"v","no_restart":true}`); rec.Code != http.StatusOK {
		t.Fatalf("secret set no_restart = %d %s", rec.Code, rec.Body.String())
	}
	if v, ok := k.SecretValue("noroll", "K"); !ok || v != "v" {
		t.Errorf("K in fake Secret = %q, %v; want v", v, ok)
	}
	if _, rolled := k.RestartedAt("noroll"); rolled {
		t.Error("no_restart secret set must not roll the workload")
	}
	// An invalid key is a 400 — the value never makes it to the Secret.
	if rec := do(h, "POST", "/v1/apps/web/secrets", token, `{"key":"1BAD","value":"x"}`); rec.Code != http.StatusBadRequest {
		t.Errorf("set bad key = %d, want 400", rec.Code)
	}

	// Seed a second key directly for the list/unset assertions below.
	k.SetSecret("web", "DATABASE_URL", "postgres://y")

	// List returns KEYS only, sorted — never the values.
	rec = do(h, "GET", "/v1/apps/web/secrets", token, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("secret list = %d %s", rec.Code, rec.Body.String())
	}
	if b := rec.Body.String(); strings.Contains(b, "sk_live_x") || strings.Contains(b, "postgres://y") {
		t.Fatalf("secret list leaked a value: %s", b)
	}
	var listed struct {
		Keys []string `json:"keys"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &listed); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(listed.Keys) != 2 || listed.Keys[0] != "DATABASE_URL" || listed.Keys[1] != "STRIPE_KEY" {
		t.Errorf("keys = %v, want [DATABASE_URL STRIPE_KEY]", listed.Keys)
	}

	// Unset removes a key and rolls the running workload by default.
	if rec := do(h, "DELETE", "/v1/apps/web/secrets/STRIPE_KEY", token, ""); rec.Code != http.StatusOK {
		t.Fatalf("secret unset = %d %s", rec.Code, rec.Body.String())
	}
	if _, ok := k.SecretValue("web", "STRIPE_KEY"); ok {
		t.Error("STRIPE_KEY should be removed")
	}
	if _, rolled := k.RestartedAt("web"); !rolled {
		t.Error("default unset should roll the workload")
	}

	// no_restart=true removes without rolling. Reset roll state by re-deploying a fresh app.
	do(h, "POST", "/v1/apps/api/deploy", token, `{"image":"img:1","replicas":1}`)
	k.SetSecret("api", "TOKEN", "t")
	if rec := do(h, "DELETE", "/v1/apps/api/secrets/TOKEN?no_restart=true", token, ""); rec.Code != http.StatusOK {
		t.Fatalf("secret unset no_restart = %d %s", rec.Code, rec.Body.String())
	}
	if _, rolled := k.RestartedAt("api"); rolled {
		t.Error("no_restart unset must not roll the workload")
	}

	// An invalid key on unset is a 400 too.
	if rec := do(h, "DELETE", "/v1/apps/web/secrets/1BAD", token, ""); rec.Code != http.StatusBadRequest {
		t.Errorf("bad key = %d, want 400", rec.Code)
	}
}

func TestNotImplementedMapsTo501(t *testing.T) {
	h, k, _ := newAPI(t)
	// An adapter that is not wired yet surfaces ErrNotImplemented; the API reports 501.
	k.SetError(fake.OpApply, fmt.Errorf("cluster adapter: %w", cp.ErrNotImplemented))
	rec := do(h, "POST", "/v1/apps/web/deploy", token, `{"image":"img:1","replicas":1}`)
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501; body = %s", rec.Code, rec.Body.String())
	}
}

func TestMethodNotAllowed(t *testing.T) {
	h, _, _ := newAPI(t)
	// GET on a POST-only route — the mux returns 405.
	if rec := do(h, "GET", "/v1/apps/web/deploy", token, ""); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", rec.Code)
	}
}

func TestAutoscaleEndpoint(t *testing.T) {
	h, k, d := newAPI(t)
	d.SetPolicy(cp.Policy{MaxReplicas: 5}.With(cp.GuardrailAutoscale, cp.DispositionAllow))

	rec := do(h, "POST", "/v1/apps/web/autoscale", token, `{"min":1,"max":4,"cpu":90}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("autoscale status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var res cp.AutoscaleResult
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if res.MaxReplicas != 4 || res.CPUPercent != 90 {
		t.Errorf("result = %+v, want max 4 cpu 90", res)
	}
	if _, ok := k.Autoscaler("web"); !ok {
		t.Errorf("HPA not applied by the endpoint")
	}

	// DELETE turns autoscaling off.
	if rec := do(h, "DELETE", "/v1/apps/web/autoscale", token, ""); rec.Code != http.StatusOK {
		t.Fatalf("disable status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if _, ok := k.Autoscaler("web"); ok {
		t.Errorf("HPA should be gone after disable")
	}
}

func TestAutoscaleMaxOverCeilingDenied(t *testing.T) {
	h, _, d := newAPI(t)
	d.SetPolicy(cp.Policy{MaxReplicas: 5}.With(cp.GuardrailAutoscale, cp.DispositionAllow))
	// A max above the ceiling is denied via the replica-ceiling guardrail (422, a structured
	// guardrail refusal).
	rec := do(h, "POST", "/v1/apps/web/autoscale", token, `{"min":1,"max":50,"cpu":80}`)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "app.replica_ceiling") {
		t.Errorf("body = %s, want the replica-ceiling code", rec.Body.String())
	}
}

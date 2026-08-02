// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package api_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// doInstall drives a request as a caller that expects to be talking to a particular install
// (ADR-0084 §5). An empty installID sends no header at all, which is what a target recorded before
// install ids existed does.
func doInstall(h http.Handler, path, tok, installID string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("GET", path, nil)
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	if installID != "" {
		req.Header.Set("X-Burrow-Install", installID)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// TestInstallGateRefusesAnotherInstall is the failure the whole mechanism exists for: a cluster
// destroyed and recreated under a kube context name a provider generates deterministically. The
// context resolves, the credential is accepted, and the Burrow that answers is a different one. The
// refusal names both ids and carries the answering install's id structurally, the way a too-old
// refusal carries server_version.
func TestInstallGateRefusesAnotherInstall(t *testing.T) {
	h, _, _ := newAPIInstall(t, "server-install")

	rec := doInstall(h, "/v1/apps", token, "target-install")
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body = %s", rec.Code, rec.Body.String())
	}
	var e errBody
	if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if e.Code != "install_mismatch" {
		t.Errorf("code = %q, want install_mismatch", e.Code)
	}
	if e.ServerInstallID != "server-install" {
		t.Errorf("server_install_id = %q, want server-install (the caller has to learn what actually answered)", e.ServerInstallID)
	}
	// Both ids belong in the message: "something is wrong" is not the useful fact, "the Burrow you
	// recorded and the Burrow that answered are different" is.
	for _, want := range []string{"target-install", "server-install"} {
		if !strings.Contains(e.Error, want) {
			t.Errorf("error %q, want substring %q", e.Error, want)
		}
	}
}

// TestInstallGateServesTheInstallItIs confirms the ordinary case: a caller naming this install is
// served, with no interference from the check at all.
func TestInstallGateServesTheInstallItIs(t *testing.T) {
	h, _, _ := newAPIInstall(t, "server-install")

	rec := doInstall(h, "/v1/apps", token, "server-install")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
}

// TestInstallGateServesACallerThatNamesNoInstall covers skew in one direction: an older CLI, or a
// target recorded before install ids existed, sends no header. It has claimed nothing, so there is
// nothing to contradict — the same tolerance the version gate gives a pre-handshake client, and the
// reason existing installs keep working.
func TestInstallGateServesACallerThatNamesNoInstall(t *testing.T) {
	h, _, _ := newAPIInstall(t, "server-install")

	rec := doInstall(h, "/v1/apps", token, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
}

// TestInstallGateServesWhenTheServerHasNoID covers skew in the other direction: a burrowd installed
// before install ids existed does not know its own, so it cannot establish that any caller is wrong.
// Refusing on an unknown would break every user of an older install the moment their CLI learned to
// send the header.
func TestInstallGateServesWhenTheServerHasNoID(t *testing.T) {
	h, _, _ := newAPIInstall(t, "")

	for _, sent := range []string{"", "some-install"} {
		rec := doInstall(h, "/v1/apps", token, sent)
		if rec.Code != http.StatusOK {
			t.Errorf("caller install %q: status = %d, want 200; body = %s", sent, rec.Code, rec.Body.String())
		}
	}
}

// TestInstallMismatchPrecedesTheVersionRefusal covers a caller that is both pointed at the wrong
// install and running a client this control plane would refuse as too old. It hears about the
// install. Telling somebody to upgrade a binary, on the authority of a control plane they never
// meant to talk to, is a remedy for a problem they do not have.
func TestInstallMismatchPrecedesTheVersionRefusal(t *testing.T) {
	h, _, _ := newAPIConfig(t, "v0.9.1", "server-install")

	req := httptest.NewRequest("GET", "/v1/apps", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-Burrow-Install", "target-install")
	req.Header.Set("X-Burrow-Client-Version", "v0.7.0")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (the install, not the version); body = %s", rec.Code, rec.Body.String())
	}
	var e errBody
	if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if e.Code != "install_mismatch" {
		t.Errorf("code = %q, want install_mismatch", e.Code)
	}
}

// TestInstallGateRunsAfterAuthentication confirms an unauthenticated request never learns this
// install's id: the credential is checked first, so a mismatch is only ever reported to somebody who
// was already allowed to talk to this control plane.
func TestInstallGateRunsAfterAuthentication(t *testing.T) {
	h, _, _ := newAPIInstall(t, "server-install")

	rec := doInstall(h, "/v1/apps", "", "target-install")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401; body = %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "server-install") {
		t.Errorf("an anonymous request must not learn the install id:\n%s", rec.Body.String())
	}
}

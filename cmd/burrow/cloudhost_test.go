// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package main

import (
	"bytes"
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/burrow-cloud/burrow/internal/cloudcred"
	"github.com/burrow-cloud/burrow/localconfig"
)

// The managed product is named by one host and addressed at another, and these tests hold that line.
// The apex serves the marketing website and answers every API path with a 404, so a device flow or a
// control-plane call aimed at it cannot work; the console is where the control plane answers. The
// two were one constant once, which is exactly how sign-in came to be pointed at a marketing site.

// TestCloudCallsGoToTheConsoleAndNotTheApex asserts the origin the device flow and every cloud call
// are built from. It is deliberately written against the literal host rather than against
// localconfig.CloudAPIEndpoint, so redefining the constant cannot make the assertion agree with
// whatever it was changed to.
func TestCloudCallsGoToTheConsoleAndNotTheApex(t *testing.T) {
	const wantHost = "console.burrow-cloud.dev"

	u, err := url.Parse(defaultCloudBaseURL)
	if err != nil {
		t.Fatalf("the cloud base URL does not parse: %v", err)
	}
	if u.Scheme != "https" {
		t.Errorf("cloud base URL scheme = %q, want https", u.Scheme)
	}
	if u.Host != wantHost {
		t.Errorf("cloud base URL host = %q, want %q — the apex serves the marketing site and 404s every API path", u.Host, wantHost)
	}
	if u.Host == localconfig.CloudEndpoint {
		t.Error("cloud calls are aimed at the product's identity host, which is the marketing site")
	}
}

// TestRecordedCloudTargetIsAddressedAtTheConsole is the join between the two constants: a target
// records the IDENTITY, and the origin resolved from it must be the console. Anything that made
// cloudBaseURLFor return the apex again would fail here.
func TestRecordedCloudTargetIsAddressedAtTheConsole(t *testing.T) {
	if got := cloudBaseURLFor(localconfig.CloudEndpoint); got != defaultCloudBaseURL {
		t.Errorf("cloudBaseURLFor(%q) = %q, want %q", localconfig.CloudEndpoint, got, defaultCloudBaseURL)
	}
	// A self-hosted endpoint is still addressed as written: only the known managed one is remapped.
	if got, want := cloudBaseURLFor("burrow.example.com"), "https://burrow.example.com"; got != want {
		t.Errorf("cloudBaseURLFor(other) = %q, want %q", got, want)
	}
}

// TestSettingsURLsNameTheConsole covers the prose the CLI prints. A settings link that resolves to a
// marketing page tells somebody trying to revoke a credential that the page does not exist.
func TestSettingsURLsNameTheConsole(t *testing.T) {
	const wantURL = "https://console.burrow-cloud.dev/settings"

	var out bytes.Buffer
	reportCredentialLocations(&out, "/home/dev/.burrow/credentials/burrow-cloud.dev.json", "/home/dev/.burrow/agents/burrow-cloud.dev.json")
	if !strings.Contains(out.String(), wantURL) {
		t.Errorf("the sign-in summary does not name %s:\n%s", wantURL, out.String())
	}

	rejected := cloudcred.RejectedMessage(cloudcred.Credential{Kind: cloudcred.KindCLI, CredentialID: "cred_cli_1"})
	if !strings.Contains(rejected, wantURL) {
		t.Errorf("the refusal message does not name %s: %q", wantURL, rejected)
	}
}

// TestCredentialsAndTargetsWrittenBeforeTheConsoleHostStillLoad is the compatibility half. The
// credential filename and the target's endpoint field are keyed to the product's identity, so
// everything already on disk — written when the identity was the only host there was — has to keep
// loading untouched. This writes both by hand, in their pre-change shape, rather than through the
// code that produces them.
func TestCredentialsAndTargetsWrittenBeforeTheConsoleHostStillLoad(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BURROW_CONFIG", filepath.Join(dir, "config"))

	const oldToken = "tok_issued_before_the_console_host_existed"
	if err := os.WriteFile(filepath.Join(dir, "config"), []byte(
		"currentTarget: burrow-cloud.dev\n"+
			"targets:\n"+
			"  - name: burrow-cloud.dev\n"+
			"    kind: burrow-cloud\n"+
			"    endpoint: burrow-cloud.dev\n"), 0o600); err != nil {
		t.Fatalf("seed the config: %v", err)
	}
	credDir := filepath.Join(dir, "credentials")
	if err := os.MkdirAll(credDir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	cred, err := json.Marshal(cloudcred.Credential{
		Endpoint: "burrow-cloud.dev", Kind: cloudcred.KindCLI,
		TenantID: "t_1234", CredentialID: "cred_cli_1", Token: oldToken,
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	// The filename is the identity, and stays the identity: burrow-cloud.dev.json.
	if err := os.WriteFile(filepath.Join(credDir, "burrow-cloud.dev.json"), cred, 0o600); err != nil {
		t.Fatalf("seed the credential: %v", err)
	}

	loaded, err := cloudcred.Load(cloudcred.KindCLI)
	if err != nil {
		t.Fatalf("a credential written before the console host existed no longer loads: %v", err)
	}
	if loaded.Token != oldToken {
		t.Error("the credential loaded, but not the one on disk")
	}

	cfg, err := localconfig.Load()
	if err != nil {
		t.Fatalf("a config written before the console host existed no longer loads: %v", err)
	}
	tgt, ok, err := cfg.ActiveTarget()
	if err != nil || !ok {
		t.Fatalf("ActiveTarget() = (%v, %v, %v), want the recorded cloud target", tgt, ok, err)
	}
	if tgt.Kind != localconfig.TargetKindCloud || tgt.Endpoint != localconfig.CloudEndpoint {
		t.Errorf("target = %+v, want the managed product at %s", tgt, localconfig.CloudEndpoint)
	}
	// And it is now addressed at the console, without anything on disk having changed.
	if got := cloudBaseURLFor(tgt.Endpoint); got != defaultCloudBaseURL {
		t.Errorf("the recorded target resolves to %q, want %q", got, defaultCloudBaseURL)
	}
}

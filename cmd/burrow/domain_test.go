// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// TestDomainAddNamesThePublishCommand is issue #475. `domain add` writes a DNS record and needs an
// address; --app reads one FROM an app that is already published. With nothing published yet neither
// flag has an answer, and an error naming only the two flags leaves the reader with no next step —
// the command they want is `burrow app publish`, so the error says so.
func TestDomainAddNamesThePublishCommand(t *testing.T) {
	_, _, err := runCLI(t, func(w http.ResponseWriter, _ *http.Request) {
		t.Error("the control plane was called although no address was given")
		_ = json.NewEncoder(w).Encode(map[string]any{})
	}, "app", "domain", "add", "app.example.com")
	if err == nil {
		t.Fatal("domain add succeeded with neither --address nor --app")
	}
	for _, want := range []string{"--address", "--app", "burrow app publish"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want it to name %q", err.Error(), want)
		}
	}
}

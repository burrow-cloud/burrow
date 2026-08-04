// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package main

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

// The deploy progress stream is for a HUMAN watching a terminal (issue #480). An agent driving a
// deploy composes one JSON outcome envelope and cannot use a stream of ticks, so `burrow-agent`
// asks for none and its output is unchanged. These tests pin that, because the failure mode is
// silent: a stream would be decoded correctly and simply flood the agent's context.

// TestAgentDeployAsksForNoProgressStream is the request half. burrow-agent must not send the ndjson
// Accept header, so a control plane that offers a stream still answers it with the plain object.
func TestAgentDeployAsksForNoProgressStream(t *testing.T) {
	f := newFakeCP(t)
	var accept string
	f.handler = func(w http.ResponseWriter, r *http.Request) {
		accept = r.Header.Get("Accept")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"release": map[string]any{"id": "r1", "app": "web", "image": "img:1", "status": "deployed"},
		})
	}
	out, code := runMutate(t, f, "deploy", "web", "--image", "img:1")
	if code != exitCodeExecuted {
		t.Fatalf("exit code = %d, want %d (%s)", code, exitCodeExecuted, out)
	}
	if strings.Contains(accept, "x-ndjson") {
		t.Errorf("Accept = %q; burrow-agent must not ask for the deploy progress stream", accept)
	}
}

// TestAgentDeployPrintsExactlyOneEnvelope is the output half: stdout is one JSON document, whatever
// the control plane is willing to narrate. A second document — or a tick line — would break every
// agent that reads this output.
func TestAgentDeployPrintsExactlyOneEnvelope(t *testing.T) {
	f := newFakeCP(t)
	f.handler = func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"release": map[string]any{"id": "r1", "app": "web", "image": "img:1", "status": "deployed"},
		})
	}
	out, _ := runMutate(t, f, "deploy", "web", "--image", "img:1")
	dec := json.NewDecoder(strings.NewReader(out))
	var first outcome
	if err := dec.Decode(&first); err != nil {
		t.Fatalf("stdout is not a JSON envelope: %v (%q)", err, out)
	}
	if first.Outcome != outcomeExecuted || first.Operation != "deploy" {
		t.Errorf("envelope = %+v, want an executed deploy", first)
	}
	var extra json.RawMessage
	if err := dec.Decode(&extra); err != io.EOF {
		t.Errorf("stdout carries more than one document: %q", out)
	}
}

// TestAgentIgnoresAStreamItDidNotAskFor is the belt-and-braces case: even a control plane that
// answers ndjson without being asked must not corrupt the envelope. The agent's classification of
// the outcome is what an agent acts on, and it comes from the ordinary decode either way.
func TestAgentIgnoresAStreamItDidNotAskFor(t *testing.T) {
	f := newFakeCP(t)
	f.handler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"result":{"release":{"id":"r1","app":"web","image":"img:1","status":"deployed"}}}`+"\n")
	}
	out, _ := runMutate(t, f, "deploy", "web", "--image", "img:1")
	oc := decodeOutcome(t, out)
	// The agent asked for no stream, so it decodes the body as the plain result. What matters is that
	// it prints one well-formed envelope rather than crashing or emitting a partial one.
	if oc.Operation != "deploy" {
		t.Errorf("operation = %q, want deploy", oc.Operation)
	}
}

// TestAgentClassificationIsUnchangedByTheStream re-pins the hold and the deny through the deploy
// verb: both arrive status-coded because the control plane writes the stream's header only once it
// has committed to work, and both must still resolve to their own outcome rather than to an error.
func TestAgentClassificationIsUnchangedByTheStream(t *testing.T) {
	cases := []struct {
		name  string
		write func(w http.ResponseWriter)
		want  string
		code  int
	}{
		{
			name:  "held",
			write: func(w http.ResponseWriter) { held(w, "deploy", "app.deploy", "prod is guarded") },
			want:  outcomeHeld,
			code:  exitCodeHeld,
		},
		{
			name:  "denied",
			write: func(w http.ResponseWriter) { denied(w, "deploy", "app.deploy", "prod is closed") },
			want:  outcomeDenied,
			code:  exitCodeDenied,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFakeCP(t)
			f.handler = func(w http.ResponseWriter, r *http.Request) { c.write(w) }
			out, code := runMutate(t, f, "deploy", "web", "--image", "img:1")
			oc := decodeOutcome(t, out)
			if oc.Outcome != c.want {
				t.Errorf("outcome = %q, want %q", oc.Outcome, c.want)
			}
			if oc.Code != "app.deploy" {
				t.Errorf("code = %q, want app.deploy", oc.Code)
			}
			if code != c.code {
				t.Errorf("exit code = %d, want %d", code, c.code)
			}
		})
	}
}

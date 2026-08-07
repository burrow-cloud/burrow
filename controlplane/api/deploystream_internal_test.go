// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package api

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/burrow-cloud/burrow/controlplane"
)

// The keepalive (issue #503). Reporting cannot cover every wait — the rollout settle is one call into
// the cluster seam and can run for minutes with nothing to say — so the stream itself has to prove
// the connection is alive. These tests are in-package because the property is about the writer, not
// about any one endpoint.

// recordingWriter is a ResponseWriter that can flush and can be read back safely while the keepalive
// goroutine is still writing to it.
type recordingWriter struct {
	mu     sync.Mutex
	header http.Header
	body   strings.Builder
	status int
}

func newRecordingWriter() *recordingWriter {
	return &recordingWriter{header: http.Header{}}
}

func (w *recordingWriter) Header() http.Header { return w.header }

func (w *recordingWriter) Write(b []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.body.Write(b)
}

func (w *recordingWriter) WriteHeader(status int) { w.status = status }

func (w *recordingWriter) Flush() {}

func (w *recordingWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.body.String()
}

// TestAQuietStreamKeepsWritingIsTheWholePoint: an operation that reports one stage and then works in
// silence must still put bytes on the wire, or a proxy whose read timeout is measured between two
// successive reads closes the connection while the work is going perfectly well.
func TestAQuietStreamKeepsWritingIsTheWholePoint(t *testing.T) {
	w := newRecordingWriter()
	s := shortKeepalive(t, w, 5*time.Millisecond)
	s.progress(controlplane.DeployEvent{Stage: controlplane.StageBuild, Status: controlplane.DeployStarted})
	// The long stage: nothing to report for many keepalive intervals.
	time.Sleep(120 * time.Millisecond)
	quiet := w.String()
	s.finish(&controlplane.DeployResult{})
	s.close()

	// A keepalive is a bare newline, so it shows up as a blank line between two values.
	if !strings.Contains(quiet, "\n\n") {
		t.Fatalf("nothing was written across a silent stage: %q", quiet)
	}
}

// TestKeepalivesDoNotDisturbTheFraming is what makes whitespace the right choice: a reader decodes
// exactly the lines the operation reported, and a client written before the keepalive existed cannot
// tell it is there.
func TestKeepalivesDoNotDisturbTheFraming(t *testing.T) {
	w := newRecordingWriter()
	s := shortKeepalive(t, w, 5*time.Millisecond)
	s.progress(controlplane.DeployEvent{Stage: controlplane.StageBuild, Status: controlplane.DeployStarted})
	time.Sleep(40 * time.Millisecond)
	s.progress(controlplane.DeployEvent{Stage: controlplane.StageBuild, Status: controlplane.DeployDone})
	time.Sleep(40 * time.Millisecond)
	s.finish(&controlplane.DeployResult{Release: controlplane.Release{ID: "r1"}})
	s.close()

	var events []string
	var results int
	dec := json.NewDecoder(strings.NewReader(w.String()))
	for {
		var line struct {
			Event  *controlplane.DeployEvent  `json:"event"`
			Result *controlplane.DeployResult `json:"result"`
		}
		if err := dec.Decode(&line); err != nil {
			if err == io.EOF {
				break
			}
			t.Fatalf("decoding the stream: %v (stream was %q)", err, w.String())
		}
		switch {
		case line.Event != nil:
			events = append(events, line.Event.Stage+":"+line.Event.Status)
		case line.Result != nil:
			results++
		}
	}
	if strings.Join(events, ",") != "build:started,build:done" {
		t.Errorf("events = %v, want exactly the two reported", events)
	}
	if results != 1 {
		t.Errorf("results = %d, want exactly one", results)
	}
}

// TestNothingIsWrittenBeforeTheFirstEvent pins the keepalive to the lazy header: a stream that has
// not committed a status line must not start writing to the connection, or every refusal decided
// before the first stage would arrive as a 200 with an error inside it.
func TestNothingIsWrittenBeforeTheFirstEvent(t *testing.T) {
	w := newRecordingWriter()
	s := shortKeepalive(t, w, 5*time.Millisecond)
	time.Sleep(40 * time.Millisecond)
	got := w.String()
	s.close()

	if got != "" {
		t.Errorf("the stream wrote %q before any event; the response is not committed yet", got)
	}
	if w.status != 0 {
		t.Errorf("status = %d, want no status line written", w.status)
	}
}

// TestCloseStopsTheKeepalive is what keeps the writer legal: writing to a ResponseWriter after the
// handler returns is not allowed, and the keepalive is the only thing that could.
func TestCloseStopsTheKeepalive(t *testing.T) {
	w := newRecordingWriter()
	s := shortKeepalive(t, w, 5*time.Millisecond)
	s.progress(controlplane.DeployEvent{Stage: controlplane.StageApply, Status: controlplane.DeployStarted})
	s.close()
	after := w.String()
	time.Sleep(60 * time.Millisecond)
	if w.String() != after {
		t.Error("the stream wrote after it was closed; a handler that has returned owns nothing")
	}
	// Closing twice is what a defer plus an explicit close would do, and must not panic.
	s.close()
}

// shortKeepalive builds a stream over w whose keepalive ticks fast enough to assert in a test. The
// interval is per stream, so no test mutates anything another test can see.
func shortKeepalive(t *testing.T, w http.ResponseWriter, d time.Duration) *progressStream {
	t.Helper()
	s, ok := newProgressStream(w)
	if !ok {
		t.Fatal("newProgressStream: the writer cannot flush")
	}
	s.interval = d
	return s
}

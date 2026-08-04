// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package kube

import (
	"testing"
	"time"

	"github.com/burrow-cloud/burrow/controlplane"
)

// TestParseLogStream covers the line shapes a stamped pod log stream produces (#480): a normal
// stamped line splits into an instant plus the application's own text, a line whose prefix does not
// parse is a continuation that inherits the previous instant for that pod, and an unparseable line
// with nothing before it keeps the zero time.
func TestParseLogStream(t *testing.T) {
	at := func(s string) time.Time {
		t.Helper()
		ts, err := time.Parse(time.RFC3339Nano, s)
		if err != nil {
			t.Fatalf("parsing want time %q: %v", s, err)
		}
		return ts.UTC()
	}

	tests := []struct {
		name string
		data string
		want []controlplane.LogLine
	}{
		{
			name: "stamped lines split into instant and message",
			data: "2026-08-04T02:49:46.123456789Z GET /healthz 200 1ms\n" +
				"2026-08-04T02:49:47Z GET /auth/github/callback 500 1.888s\n",
			want: []controlplane.LogLine{
				{Pod: "web-1", Timestamp: at("2026-08-04T02:49:46.123456789Z"), Message: "GET /healthz 200 1ms"},
				{Pod: "web-1", Timestamp: at("2026-08-04T02:49:47Z"), Message: "GET /auth/github/callback 500 1.888s"},
			},
		},
		{
			name: "a message that carries its own timestamp keeps it intact",
			data: "2026-08-04T02:49:46Z 2026/08/04 02:49:46 GET /auth/github/callback 500 1.888s\n",
			want: []controlplane.LogLine{
				{Pod: "web-1", Timestamp: at("2026-08-04T02:49:46Z"), Message: "2026/08/04 02:49:46 GET /auth/github/callback 500 1.888s"},
			},
		},
		{
			name: "an unparseable line continues the previous one",
			data: "2026-08-04T02:49:46Z panic: boom\n" +
				"goroutine 1 [running]:\n" +
				"\tmain.main()\n",
			want: []controlplane.LogLine{
				{Pod: "web-1", Timestamp: at("2026-08-04T02:49:46Z"), Message: "panic: boom"},
				{Pod: "web-1", Timestamp: at("2026-08-04T02:49:46Z"), Message: "goroutine 1 [running]:"},
				{Pod: "web-1", Timestamp: at("2026-08-04T02:49:46Z"), Message: "\tmain.main()"},
			},
		},
		{
			name: "a leading unparseable line has nothing to inherit and stays zero",
			data: "truncated tail of an earlier write\n" +
				"2026-08-04T02:49:46Z started\n",
			want: []controlplane.LogLine{
				{Pod: "web-1", Message: "truncated tail of an earlier write"},
				{Pod: "web-1", Timestamp: at("2026-08-04T02:49:46Z"), Message: "started"},
			},
		},
		{
			name: "a non-UTC prefix is normalized to UTC rather than shifted to local time",
			data: "2026-08-04T04:49:46+02:00 started\n",
			want: []controlplane.LogLine{
				{Pod: "web-1", Timestamp: at("2026-08-04T02:49:46Z"), Message: "started"},
			},
		},
		{
			name: "blank application lines are dropped, stamped or not",
			data: "2026-08-04T02:49:46Z \n\n2026-08-04T02:49:47Z after\n",
			want: []controlplane.LogLine{
				{Pod: "web-1", Timestamp: at("2026-08-04T02:49:47Z"), Message: "after"},
			},
		},
		{
			name: "an empty stream yields no lines",
			data: "",
			want: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseLogStream("web-1", tt.data)
			if len(got) != len(tt.want) {
				t.Fatalf("got %d lines %+v, want %d %+v", len(got), got, len(tt.want), tt.want)
			}
			for i := range got {
				if got[i].Pod != tt.want[i].Pod || got[i].Message != tt.want[i].Message || !got[i].Timestamp.Equal(tt.want[i].Timestamp) {
					t.Errorf("line %d = %+v, want %+v", i, got[i], tt.want[i])
				}
				if !got[i].Timestamp.IsZero() && got[i].Timestamp.Location() != time.UTC {
					t.Errorf("line %d location = %v, want UTC", i, got[i].Timestamp.Location())
				}
			}
		})
	}
}

// TestParseLogStreamCarriesForwardPerPod pins the per-pod rule: a continuation inherits the last
// instant read from its own pod's stream, never one that belongs to a different pod.
func TestParseLogStreamCarriesForwardPerPod(t *testing.T) {
	first := parseLogStream("web-1", "2026-08-04T02:49:46Z started\n")
	if len(first) != 1 || first[0].Timestamp.IsZero() {
		t.Fatalf("first pod lines = %+v, want one stamped line", first)
	}
	second := parseLogStream("web-2", "no prefix here\n")
	if len(second) != 1 {
		t.Fatalf("second pod lines = %+v, want one line", second)
	}
	if !second[0].Timestamp.IsZero() {
		t.Errorf("second pod timestamp = %v, want zero (nothing to carry forward within that pod)", second[0].Timestamp)
	}
}

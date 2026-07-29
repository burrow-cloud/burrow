// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package controlplane_test

import (
	"strings"
	"testing"

	cp "github.com/burrow-cloud/burrow/controlplane"
)

// TestIssueReasonsIsAClosedSet pins the vocabulary itself. ADR-0074 §5 makes the set an interface
// an agent branches on, so a member appearing or disappearing is an API change and has to be a
// decision, not a side effect of editing a switch.
func TestIssueReasonsIsAClosedSet(t *testing.T) {
	want := []string{
		"ImagePullBackOff",
		"ErrImagePull",
		"Unschedulable",
		"VolumeUnavailable",
		"CrashLoopBackOff",
		"CreateContainerConfigError",
		"OOMKilled",
		"ProgressDeadlineExceeded",
		"DeadlineExceeded",
	}
	got := cp.IssueReasons()
	if len(got) != len(want) {
		t.Fatalf("IssueReasons() = %v, want %v", got, want)
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("IssueReasons()[%d] = %q, want %q", i, got[i], w)
		}
		if !cp.IsIssueReason(w) {
			t.Errorf("IsIssueReason(%q) = false, want true", w)
		}
	}
	// The criterion, enforced rather than merely documented: a reason that resolves on its own is
	// not an Issue, and neither is an unrecognized one.
	for _, r := range []string{"", "ContainerCreating", "PodInitializing", "Completed", "Running"} {
		if cp.IsIssueReason(r) {
			t.Errorf("IsIssueReason(%q) = true, want false", r)
		}
	}
}

// TestIssueEvidenceMessageIsActionable pins, per reason, the part of the message a person or an
// agent acts on. A reason that named no fix would be the silence ADR-0074 §2 is about, wearing a
// label.
func TestIssueEvidenceMessageIsActionable(t *testing.T) {
	cases := []struct {
		name     string
		evidence cp.IssueEvidence
		want     []string
		absent   []string
	}{
		{
			name: "unschedulable names what could not be satisfied",
			evidence: cp.IssueEvidence{
				Reason: cp.ReasonUnschedulable,
				Detail: "0/3 nodes are available: 1 node(s) had untolerated taint {workload: gpu}, 2 Insufficient cpu.",
			},
			want: []string{cp.ReasonUnschedulable, "untolerated taint {workload: gpu}", "Insufficient cpu", "tolerate the taint"},
		},
		{
			name:     "volume names the claim",
			evidence: cp.IssueEvidence{Reason: cp.ReasonVolumeUnavailable, Detail: "pod has unbound immediate PersistentVolumeClaims"},
			want:     []string{cp.ReasonVolumeUnavailable, "unbound immediate PersistentVolumeClaims", "StorageClass"},
		},
		{
			name:     "crash loop names the exit code and labels the output",
			evidence: cp.IssueEvidence{Reason: cp.ReasonCrashLoopBackOff, Container: "web", ExitCode: 2, LogTail: "panic: bad config"},
			want:     []string{`"web"`, cp.ReasonCrashLoopBackOff, "exited with code 2", "application's own output", "panic: bad config"},
		},
		{
			name:     "crash loop with no captured output still names the exit code",
			evidence: cp.IssueEvidence{Reason: cp.ReasonCrashLoopBackOff, Container: "web", ExitCode: 0},
			want:     []string{"exited with code 0", "no output"},
		},
		{
			name: "config error names the key",
			evidence: cp.IssueEvidence{
				Reason: cp.ReasonCreateContainerConfigError, Container: "web",
				Detail: "couldn't find key STRIPE_API_KEY in Secret default/burrow-app-web-secrets",
			},
			want: []string{cp.ReasonCreateContainerConfigError, "STRIPE_API_KEY", "set it and redeploy"},
		},
		{
			name:     "OOM names the limit that was hit",
			evidence: cp.IssueEvidence{Reason: cp.ReasonOOMKilled, Container: "web", Detail: "128Mi"},
			want:     []string{cp.ReasonOOMKilled, "128Mi", "Raise the limit"},
		},
		{
			name:     "OOM with no limit set says what actually killed it",
			evidence: cp.IssueEvidence{Reason: cp.ReasonOOMKilled, Container: "web"},
			want:     []string{cp.ReasonOOMKilled, "no memory limit is set"},
		},
		{
			name:     "progress deadline admits it found nothing more specific",
			evidence: cp.IssueEvidence{Reason: cp.ReasonProgressDeadlineExceeded},
			want:     []string{cp.ReasonProgressDeadlineExceeded, "progress deadline", "no blocking pod condition"},
		},
		{
			name:     "job deadline names the deadline",
			evidence: cp.IssueEvidence{Reason: cp.ReasonDeadlineExceeded},
			want:     []string{cp.ReasonDeadlineExceeded, "did not finish within its deadline"},
		},
		{
			// The pull family's prose is ADR-0017's and must not move: IssueEvidence renders it by
			// delegating to ImagePullIssue rather than by restating it.
			name:     "image pull keeps ADR-0017's message",
			evidence: cp.IssueEvidence{Reason: cp.ReasonImagePullBackOff, Image: "ghcr.io/org/app:1"},
			want:     []string{"ghcr.io/org/app:1", `registry "ghcr.io"`, "burrow config registry login ghcr.io"},
		},
		{
			name:     "a reason outside the set renders nothing",
			evidence: cp.IssueEvidence{Reason: "ContainerCreating"},
			absent:   []string{"ContainerCreating"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			msg := c.evidence.Message()
			if len(c.want) == 0 && msg != "" {
				t.Fatalf("Message() = %q, want empty", msg)
			}
			for _, w := range c.want {
				if !strings.Contains(msg, w) {
					t.Errorf("Message() = %q, want it to contain %q", msg, w)
				}
			}
			for _, a := range c.absent {
				if strings.Contains(msg, a) {
					t.Errorf("Message() = %q, want it not to contain %q", msg, a)
				}
			}
		})
	}
}

// TestEveryReasonRendersAMessage guards the set and the renderer against drifting apart: a member
// added to IssueReasons with no case in Message would surface a reason with no explanation, which
// is worse than surfacing neither.
func TestEveryReasonRendersAMessage(t *testing.T) {
	for _, r := range cp.IssueReasons() {
		if msg := (cp.IssueEvidence{Reason: r, Image: "img:1"}).Message(); msg == "" {
			t.Errorf("Message() for reason %q is empty; every member of the closed set must explain itself", r)
		}
	}
}

// TestCrashLoopLogTailIsBounded is the ADR-0074 §9 obligation on application output: it is the
// app's, it may contain anything, and neither a line count nor a byte count alone bounds it — one
// line of minified output is unbounded, and a thousand short lines are unreadable. Both apply, and
// the cut is marked so a reader can tell a truncated tail from a complete one.
func TestCrashLoopLogTailIsBounded(t *testing.T) {
	long := strings.Repeat("x", cp.IssueLogTailBytes*3)
	msg := (cp.IssueEvidence{Reason: cp.ReasonCrashLoopBackOff, Container: "web", LogTail: long}).Message()
	if len(msg) > cp.IssueLogTailBytes+400 {
		t.Errorf("message is %d bytes; the log tail is not bounded", len(msg))
	}
	if !strings.Contains(msg, "truncated") {
		t.Errorf("message = %q, want the cut marked", msg)
	}

	many := strings.Repeat("line\n", cp.IssueLogTailLines*5)
	msg = (cp.IssueEvidence{Reason: cp.ReasonCrashLoopBackOff, Container: "web", LogTail: many}).Message()
	if n := strings.Count(msg, "line"); n > cp.IssueLogTailLines {
		t.Errorf("message carried %d log lines, want at most %d", n, cp.IssueLogTailLines)
	}
}

// TestClusterMessagesAreBounded: a scheduler verdict on a large cluster names every node it
// rejected, and a status line is not a place for that.
func TestClusterMessagesAreBounded(t *testing.T) {
	verdict := strings.Repeat("node-rejected ", 400)
	msg := (cp.IssueEvidence{Reason: cp.ReasonUnschedulable, Detail: verdict}).Message()
	if len(msg) > 800 {
		t.Errorf("message is %d bytes; the scheduler's verdict is not bounded", len(msg))
	}
}

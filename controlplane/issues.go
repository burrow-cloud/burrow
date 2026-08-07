// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package controlplane

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// The helpers here widen the Issue vocabulary beyond the single image-pull family imagepull.go
// introduced for ADR-0017, to the blocking classes users actually hit (ADR-0074 §2). They live in
// the core package, dependency-free, for exactly the reason imagepull.go does: the real Kubernetes
// adapter reads the evidence off a pod and a test injects it into the fake, and BOTH build the same
// message, so the prose an operator reads is pinned by tests that need no cluster.
//
// The inclusion criterion is imagepull.go's, promoted unchanged by ADR-0074 §2 because it is what
// keeps the vocabulary from becoming noise: A REASON IS AN ISSUE WHEN IT IS BLOCKING AND
// HUMAN-FIXABLE, AND NOT WHEN IT RESOLVES ON ITS OWN. ContainerCreating and PodInitializing stay
// excluded on exactly that ground — they are a pod getting on with it, not a pod waiting for a
// person. A surface that flagged them would train its reader to ignore it.
//
// NO SECRET VALUE EVER ENTERS AN ISSUE (ADR-0074 §9). A CreateContainerConfigError names the
// missing KEY, which is the actionable part and is safe to print; the kubelet's message for that
// reason names objects and keys, never contents. A crash-loop log tail is the APPLICATION's own
// output and may contain anything at all, so it is bounded here and described to the reader as
// application output rather than as something Burrow has vetted.

// The closed IssueReason set. An agent branches on these rather than parsing the prose, so the set
// is enumerated (IssueReasons) and validated (IsIssueReason) rather than left implicit in a switch
// — ADR-0074 §5: the most important consumer of this surface is an agent, not a person.
//
// Each value is the raw Kubernetes reason string for its condition wherever Kubernetes has one, so
// the reason an agent sees is the reason it would find in `kubectl describe`. ReasonVolumeUnavailable
// is the one Burrow-coined member, because Kubernetes has no single reason string for "this pod
// cannot get its volume": the evidence arrives as a scheduler message, and splitting it out from
// ReasonUnschedulable is worth a coined name because the fix is a different one (a claim or a
// StorageClass, not capacity or a toleration).
const (
	// ReasonUnschedulable is the PodScheduled condition reason when no node can run the pod. Its
	// detail is the scheduler's own message, which names what could not be satisfied.
	ReasonUnschedulable = "Unschedulable"
	// ReasonVolumeUnavailable is Burrow's reason for a pod blocked on a volume it cannot get — an
	// unbound claim, no provisioner, or a volume pinned to a node the pod cannot run on.
	ReasonVolumeUnavailable = "VolumeUnavailable"
	// ReasonCrashLoopBackOff is the container waiting reason once the kubelet has backed off
	// restarting a container that keeps exiting.
	ReasonCrashLoopBackOff = "CrashLoopBackOff"
	// ReasonCreateContainerConfigError is the container waiting reason when the container's
	// configuration cannot be resolved — most often a ConfigMap or Secret key that is not there.
	ReasonCreateContainerConfigError = "CreateContainerConfigError"
	// ReasonOOMKilled is the terminated-state reason when the kernel killed the container for
	// exceeding its memory limit.
	ReasonOOMKilled = "OOMKilled"
	// ReasonProgressDeadlineExceeded is the Deployment Progressing-condition reason when a rollout
	// has not made progress within .spec.progressDeadlineSeconds.
	ReasonProgressDeadlineExceeded = "ProgressDeadlineExceeded"
	// ReasonStartError is the terminated-state reason the kubelet records when a container was
	// created but its command never executed — a missing binary, a path that is not the image's
	// entrypoint, a file that is not executable. It is the one blocking condition that arrives as a
	// TERMINATED state rather than a waiting one, which is exactly why it went unreported for so
	// long: every Job waiter reads waiting states, a pod in Init:StartError has none, and the Job's
	// own counters say only that it failed (issue #478).
	//
	// It earns its place by the same criterion as the rest: blocking, human-fixable, and never
	// self-resolving — a container whose command does not exist will not find one on a retry.
	ReasonStartError = "StartError"
	// ReasonDeadlineExceeded is the reason a Job ran out of time — the Job Failed-condition reason
	// when Kubernetes enforces activeDeadlineSeconds, and the reason Burrow's own Job waiters carry
	// when their client-side deadline expires (issue #352). It is the BACKSTOP member: a waiter
	// reaches it only when no pod reported anything blocking, so its detail carries what the waiter
	// did observe rather than a bare elapsed time.
	ReasonDeadlineExceeded = "DeadlineExceeded"
)

// IssueReasons returns every member of the closed IssueReason set, so a caller (or a test, or a
// generated agent schema) can enumerate the vocabulary instead of hard-coding it.
func IssueReasons() []string {
	return []string{
		ReasonImagePullBackOff,
		ReasonErrImagePull,
		ReasonUnschedulable,
		ReasonVolumeUnavailable,
		ReasonCrashLoopBackOff,
		ReasonCreateContainerConfigError,
		ReasonOOMKilled,
		ReasonProgressDeadlineExceeded,
		ReasonStartError,
		ReasonDeadlineExceeded,
	}
}

// IsIssueReason reports whether reason is a member of the closed set. A reason outside it is not an
// Issue: that is the criterion above, enforced rather than merely documented, so a new Kubernetes
// waiting reason cannot leak into the surface without a decision being made about it here.
func IsIssueReason(reason string) bool {
	for _, r := range IssueReasons() {
		if r == reason {
			return true
		}
	}
	return false
}

// JobBlockedError is what a Job waiter returns instead of waiting out its deadline, when the Job's
// pod reports a blocking, human-fixable condition (issue #352). Before it, a Job whose pod could not
// start — a missing Secret, an unpullable image, no node with room — left Job.Status.Failed and
// .Succeeded both at zero, so a waiter polling only those two counters could not tell "still
// working" from "will never start" and reported a bare timeout minutes later. ADR-0074 names that
// shape: a waiter that burns its full deadline when the cluster was saying exactly what was wrong
// has converted a diagnosis into a shrug.
//
// Reason is a member of the closed IssueReason set so a CALLER CAN BRANCH ON IT rather than parse
// the prose (ADR-0074 §5) — an agent that sees ReasonCreateContainerConfigError knows retrying is
// pointless, where a timeout string invites exactly that retry. Issue is IssueEvidence.Message()'s
// output, so the wording a user reads for a blocked Job is the wording they read for a blocked
// Deployment; there is one renderer, not two that can drift apart.
type JobBlockedError struct {
	// Job is the Job's name, so a reader can go straight to it (a blocked Job is deliberately left
	// undeleted for diagnosis).
	Job string
	// Reason is the member of the closed set. ReasonDeadlineExceeded is the backstop: the waiter ran
	// out of time without any pod reporting a blocking condition.
	Reason string
	// Issue is the rendered, actionable explanation — the same prose the status surface shows.
	Issue string
}

func (e *JobBlockedError) Error() string {
	return fmt.Sprintf("job %q: %s", e.Job, e.Issue)
}

// TimedOut reports whether this error is the deadline backstop rather than an observed blocking
// condition. It is the one distinction a caller makes on the reason without knowing the whole set.
func (e *JobBlockedError) TimedOut() bool {
	return e.Reason == ReasonDeadlineExceeded
}

const (
	// IssueLogTailLines bounds how many lines of a crash-looping container's previous log an Issue
	// carries. Enough to see a stack trace's first frames or a startup error; not a log viewer.
	IssueLogTailLines = 20
	// IssueLogTailBytes bounds the same tail in bytes, because a line count alone bounds nothing —
	// one line of minified output or a base64 blob can be megabytes. Both bounds apply.
	IssueLogTailBytes = 1200
	// issueDetailBytes bounds a cluster-supplied detail (a scheduler or kubelet message) so a long
	// node-by-node scheduling verdict cannot turn one status line into a page.
	issueDetailBytes = 400
)

// IssueEvidence is the raw, cluster-supplied evidence behind one Issue, in a form the core package
// can turn into prose. It is the seam that keeps the real adapter and the fake honest with each
// other: the adapter fills it from a pod (or a Deployment condition) and the fake takes it directly
// from a test, and both call Message, so there is exactly one place the wording lives.
//
// Fields other than Reason are optional and reason-specific; a missing one degrades the message
// rather than breaking it, because status enrichment is best-effort by contract and must never be
// the thing that fails a status call.
type IssueEvidence struct {
	// Reason is the member of the closed set this evidence is for. Empty means no issue.
	Reason string
	// Container is the container the evidence came from, when the reason is a container's.
	Container string
	// Image is the container image, for an image-pull reason.
	Image string
	// Detail is the reason-specific evidence: the scheduler's message for a scheduling or volume
	// failure, the kubelet's message for a config error, the memory limit for an OOM kill.
	Detail string
	// ExitCode is the code the previous run of a crash-looping container exited with. A crash loop
	// with code 0 is a real and confusing case (a container that keeps exiting cleanly under
	// restartPolicy Always), so it is reported as-is rather than treated as "unknown".
	ExitCode int32
	// LogTail is a bounded tail of a crash-looping container's PREVIOUS log — application output,
	// captured verbatim. Message labels it as such; Burrow does not inspect or sanitise it.
	LogTail string
	// Restarts is the container's restart count, set ONLY when this evidence is about a container
	// that was killed and CAME BACK — it is running and serving right now (issue #416). Zero means
	// the evidence is about a container that is currently blocked, which is what every reason here
	// originally described.
	//
	// Message branches on it, because "it was killed and is serving again" and "it is not serving"
	// are different sentences and a reader who cannot tell them apart cannot act on either: the
	// first is a limit to raise before the next kill, the second is an outage. It is a count rather
	// than a flag so the message can say how many, which is the difference between a one-off and a
	// container being cycled — the ledger answers that over time, and this answers it in the pod.
	Restarts int32
}

// Message renders the actionable explanation for this evidence. An unknown or empty Reason returns
// "", so a caller that forgot to check IsIssueReason produces no Issue rather than a misleading one.
func (e IssueEvidence) Message() string {
	switch e.Reason {
	case ReasonImagePullBackOff, ReasonErrImagePull:
		// Delegates to ADR-0017's message unchanged: the pull family's prose (and its
		// not-found-vs-credentials split) is settled and tested, and widening the vocabulary
		// must not move it.
		return ImagePullIssue(e.Image, e.Reason, e.Detail)
	case ReasonUnschedulable:
		return fmt.Sprintf("no node can run this app's pod (%s): %s. The scheduler's verdict names what could not be satisfied — add capacity, tolerate the taint, or relax the node selector",
			e.Reason, e.detail("the scheduler reported no reason"))
	case ReasonVolumeUnavailable:
		return fmt.Sprintf("this app's pod cannot get its volume (%s): %s. Check the PersistentVolumeClaim exists, that a StorageClass can provision it, and that the volume is reachable from a node the pod may run on",
			e.Reason, e.detail("the scheduler reported no reason"))
	case ReasonCrashLoopBackOff:
		return e.crashLoopMessage()
	case ReasonCreateContainerConfigError:
		return fmt.Sprintf("container %s cannot be created (%s): %s. The key named above is missing from the app's config or its per-app Secret — set it and redeploy",
			e.container(), e.Reason, e.detail("the kubelet reported no reason"))
	case ReasonOOMKilled:
		return fmt.Sprintf("container %s was killed for exceeding its memory limit (%s)%s%s. Raise the limit or reduce what the app allocates — restarting it alone reproduces the kill",
			e.container(), e.Reason, e.limit(), e.restarted())
	case ReasonStartError:
		// Says the command NEVER RAN, because that is the distinction the reader needs and the one
		// an exit code alone destroys: a container that starts and exits non-zero is the workload
		// reporting a result, and this is the workload never having been reached. The kubelet's
		// message names the executable it could not run, which is the whole diagnosis.
		return fmt.Sprintf("container %s was created but its command never ran (%s)%s. Nothing this container was asked to do has happened — the image does not carry the command it was given, so retrying it will fail the same way",
			e.container(), e.Reason, e.suffixDetail())
	case ReasonProgressDeadlineExceeded:
		return fmt.Sprintf("the current release did not finish rolling out within the Deployment's progress deadline (%s), and no blocking pod condition was reported. Check the app's logs for what the new pods are doing",
			e.Reason)
	case ReasonDeadlineExceeded:
		// Deliberately does NOT claim the Job was terminated: Burrow's waiters stop waiting, they do
		// not kill the Job, and a message that said otherwise would send a reader looking for a pod
		// that is still running. The detail is what the waiter last observed, which is the whole
		// point of the reason existing (issue #352).
		return fmt.Sprintf("the job did not finish within its deadline (%s)%s",
			e.Reason, e.suffixDetail())
	}
	return ""
}

// crashLoopMessage names the exit code — the one fact that separates "the app rejected its config"
// from "the app was signalled" — and appends the previous run's output when there is any. The tail
// is labelled as the application's own output because that is what it is: ADR-0074 §9 requires the
// surface to say so rather than let a reader assume Burrow has vetted it.
func (e IssueEvidence) crashLoopMessage() string {
	msg := fmt.Sprintf("container %s is restarting repeatedly (%s): its previous run exited with code %d",
		e.container(), e.Reason, e.ExitCode)
	if e.Restarts > 0 {
		// Read off a container that is RUNNING at this instant, so it is not sitting in a back-off
		// now and saying it is would send the reader looking for a pod that is down. What is true is
		// the count, the code it died with, and that it is serving between the kills.
		msg = fmt.Sprintf("container %s has restarted %d times and is serving again now (%s): its last run before that exited with code %d",
			e.container(), e.Restarts, e.Reason, e.ExitCode)
	}
	tail := boundLogTail(e.LogTail)
	if tail == "" {
		return msg + ". Its previous run produced no output"
	}
	return msg + ". Its previous run's last output follows — this is the application's own output, shown as-is:\n" + tail
}

// restarted renders the clause that says the container CAME BACK, for evidence read off a container
// that is running now. Without it, "was killed for exceeding its memory limit" sits beside an
// available workload with nothing to reconcile the two, and the reader is left guessing whether the
// app is down. It is empty for a container that is still blocked, whose message already describes a
// thing that is not serving.
func (e IssueEvidence) restarted() string {
	if e.Restarts <= 0 {
		return ""
	}
	if e.Restarts == 1 {
		return ", and restarted — it is serving again now, after 1 restart"
	}
	return fmt.Sprintf(", and restarted — it is serving again now, after %d restarts", e.Restarts)
}

// container renders the container name for a message, quoted when known.
func (e IssueEvidence) container() string {
	if e.Container == "" {
		return "in this app's pod"
	}
	return fmt.Sprintf("%q", e.Container)
}

// detail renders the cluster's own message, bounded, falling back to fallback when the cluster gave
// none — an empty middle clause would otherwise read as a truncated sentence.
func (e IssueEvidence) detail(fallback string) string {
	// The scheduler ends its verdict with a full stop and the sentence here continues after it, so
	// the trailing one is dropped rather than left to read as "…Insufficient cpu.. The scheduler".
	d := boundText(strings.TrimRight(strings.TrimSpace(collapseLines(e.Detail)), "."), issueDetailBytes)
	if d == "" {
		return fallback
	}
	return d
}

// limit renders the OOM-killed container's memory limit when it is known. A container with no limit
// at all was killed by node pressure rather than its own limit, and saying "its limit of " with
// nothing after it would be worse than saying nothing.
func (e IssueEvidence) limit() string {
	if e.Detail == "" {
		return " (no memory limit is set on it, so the node's memory pressure is what killed it)"
	}
	return fmt.Sprintf(" of %s", boundText(strings.TrimSpace(e.Detail), issueDetailBytes))
}

// suffixDetail appends the cluster's message when there is one, for reasons whose sentence is
// complete without it.
func (e IssueEvidence) suffixDetail() string {
	d := boundText(strings.TrimSpace(collapseLines(e.Detail)), issueDetailBytes)
	if d == "" {
		return ""
	}
	return ": " + d
}

// boundLogTail applies BOTH bounds to captured application output: the last IssueLogTailLines lines
// and at most IssueLogTailBytes bytes. The line bound is what makes it readable; the byte bound is
// what makes it safe, since a single line of application output has no length limit at all.
func boundLogTail(tail string) string {
	tail = strings.TrimRight(tail, "\n")
	if tail == "" {
		return ""
	}
	lines := strings.Split(tail, "\n")
	if len(lines) > IssueLogTailLines {
		lines = lines[len(lines)-IssueLogTailLines:]
	}
	return boundText(strings.Join(lines, "\n"), IssueLogTailBytes)
}

// collapseLines folds a multi-line cluster message onto one line, so a status table stays a table.
// Only application output (the crash-loop tail) keeps its line structure.
func collapseLines(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// boundText truncates s to at most n bytes on a rune boundary, marking the cut so a reader can tell
// a truncated message from a short one.
func boundText(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := s[:n]
	for len(cut) > 0 && !utf8.ValidString(cut) {
		cut = cut[:len(cut)-1]
	}
	return strings.TrimRight(cut, "\n") + "… (truncated)"
}

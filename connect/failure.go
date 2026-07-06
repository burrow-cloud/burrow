// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package connect

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"syscall"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

// ProbeTimeout caps a best-effort control-plane probe so a command (e.g. `burrow version`)
// returns promptly even when the targeted cluster is unreachable. FailureReason names this
// duration in its timeout message, so the timeout and the message it produces stay in step.
const ProbeTimeout = 5 * time.Second

// FailureReason reduces a connectivity error to a concise reason, dropping the dialed URL that
// the Kubernetes client prepends. It names the common failures explicitly (timeout, DNS,
// refused) and otherwise strips the `Get "<url>": ` prefix so the URL noise stays out of the
// message. It is the one classifier shared by `burrow version` and connect.Client so the two
// render the same failures the same way.
func FailureReason(err error) string {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return fmt.Sprintf("timed out after %s", ProbeTimeout)
	case errors.Is(err, syscall.ECONNREFUSED):
		return "connection refused"
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return "no such host"
	}
	return trimDialPrefix(err.Error())
}

// trimDialPrefix removes a leading `Get "<url>": ` (or any `<verb> "<url>": `) that the
// Kubernetes REST client puts on transport errors, leaving just the underlying reason.
func trimDialPrefix(s string) string {
	if quote := strings.Index(s, ` "`); quote >= 0 {
		if sep := strings.Index(s[quote:], `": `); sep >= 0 {
			return s[quote+sep+len(`": `):]
		}
	}
	return s
}

// isUnreachable reports whether err is a dial/DNS/timeout failure reaching the cluster, as
// opposed to an API error the server actually returned (such as a NotFound). It is what
// separates the "control plane unreachable" rendering from the "not installed" one.
func isUnreachable(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, syscall.ECONNREFUSED) {
		return true
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return true
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		return true
	}
	var netErr net.Error
	return errors.As(err, &netErr)
}

// UnreachableError classifies a connect failure as the targeted cluster's control plane being
// unreachable — a dial/DNS/timeout/refused failure — as distinct from a NotFound (burrowd not
// installed) or another API error the server actually returned. It carries the concise, URL-free
// rendering connectError already produced, so its Error() message is byte-for-byte what the flat
// error used to be (existing renderings and tests are unaffected). It exists so a caller can detect
// the unreachable case with errors.As and enrich it — the MCP server, for one, names the other
// registered environments so a human can redirect (ADR-0047 §4) — without matching on message text.
type UnreachableError struct {
	Context string // the kube context the connection targeted
	Reason  string // the concise FailureReason (no dialed URL)
}

func (e *UnreachableError) Error() string {
	return fmt.Sprintf("control plane unreachable via context %q (%s)", e.Context, e.Reason)
}

// connectError turns a failure reaching the control plane through context o into an actionable,
// context-named message, suppressing the raw Kubernetes error. A NotFound on the token Secret
// means burrowd is not installed; a dial/DNS/timeout error means the cluster is unreachable
// (returned as a typed *UnreachableError so callers can enrich it); anything else is reported with
// the context name and a trimmed reason.
func connectError(o Options, err error) error {
	ctxName, _ := TargetContextName(o.Kubeconfig, o.Context)
	switch {
	case apierrors.IsNotFound(err):
		return fmt.Errorf(`burrow is not installed in context %q (namespace %q); run "burrow install"`, ctxName, o.Namespace)
	case isUnreachable(err):
		return &UnreachableError{Context: ctxName, Reason: FailureReason(err)}
	default:
		return fmt.Errorf("connecting via context %q: %s", ctxName, FailureReason(err))
	}
}

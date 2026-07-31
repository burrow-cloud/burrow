// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package client

import (
	"context"
	"errors"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/burrow-cloud/burrow/controlplane"
)

// okServer answers every request with an empty JSON object after an optional delay.
func okServer(t *testing.T, delay time.Duration) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if delay > 0 {
			select {
			case <-time.After(delay):
			case <-r.Context().Done():
				return
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, "{}")
	}))
	t.Cleanup(srv.Close)
	return srv
}

// deadlineRecorder records the deadline each outbound request carried, which is how a test observes
// the budget the client actually applied to one call rather than the constant it was meant to.
type deadlineRecorder struct {
	inner http.RoundTripper
	last  time.Duration
}

func (d *deadlineRecorder) RoundTrip(r *http.Request) (*http.Response, error) {
	d.last = time.Duration(math.MaxInt64)
	if dl, ok := r.Context().Deadline(); ok {
		d.last = time.Until(dl)
	}
	return d.inner.RoundTrip(r)
}

// TestRequestBudgetCoversTheControlPlanesOwnBound is the regression test for issue #404: a client
// bound SHORTER than the server bound it is waiting on turns an operation the control plane goes on
// to complete into a reported failure, and the obvious response to a failed deploy is to deploy
// again.
//
// It asserts the relation rather than the value. What it measures is the bound this client would
// actually enforce on one call — the request's own deadline, capped by any client-wide
// http.Client.Timeout — against what controlplane/apiwait.go declares that call may take. The
// blanket sixty seconds this replaced fails it on every row: sixty seconds is shorter than the five
// minutes deploy.settle_timeout allows by default, let alone the thirty an operator may set.
func TestRequestBudgetCoversTheControlPlanesOwnBound(t *testing.T) {
	srv := okServer(t, 0)
	ctx := context.Background()

	cases := []struct {
		name  string
		bound time.Duration
		call  func(*Client) error
	}{
		{"deploy", controlplane.MaxDeployWait, func(c *Client) error {
			_, err := c.Deploy(ctx, "app", DeployRequest{Image: "img:1"})
			return err
		}},
		{"rollback", controlplane.MaxDeployWait, func(c *Client) error {
			_, err := c.Rollback(ctx, "app", "", false)
			return err
		}},
		{"build", controlplane.MaxBuildWait, func(c *Client) error {
			_, err := c.Build(ctx, "app", BuildRequest{})
			return err
		}},
		{"run", controlplane.MaxRunWait, func(c *Client) error {
			_, err := c.Run(ctx, "app", RunRequest{Command: []string{"true"}})
			return err
		}},
		{"backup", controlplane.MaxBackupWait, func(c *Client) error {
			_, err := c.BackupAddon(ctx, "postgres", "app", "", "")
			return err
		}},
		{"restore", controlplane.MaxBackupWait, func(c *Client) error {
			return c.RestoreAddon(ctx, "postgres", "app", "b1", "", true)
		}},
		{"remove add-on", controlplane.MaxBackupWait, func(c *Client) error {
			_, err := c.RemoveAddon(ctx, "postgres", RemoveAddonOptions{DeleteData: true, Confirm: true})
			return err
		}},
		{"add provider", controlplane.MaxProviderWait, func(c *Client) error {
			_, err := c.AddProvider(ctx, AddProviderRequest{Type: "backblaze"})
			return err
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := NewNamedClient(srv.URL, "token", ClientNameAgent, "v0")
			rec := &deadlineRecorder{inner: c.http.Transport}
			c.http.Transport = rec
			if err := tc.call(c); err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
			got := rec.last
			if c.http.Timeout > 0 && c.http.Timeout < got {
				got = c.http.Timeout
			}
			if got < tc.bound {
				t.Fatalf("%s is bounded at %s by the client but the control plane may spend %s on it; "+
					"a client bound shorter than the server bound it waits on reports an operation that "+
					"succeeded as failed (issue #404)", tc.name, got, tc.bound)
			}
		})
	}
}

// TestReadsKeepTheShortDefaultBudget pins the other half of the fix: raising the bound for the calls
// that wait must not raise it for the calls that do not. A status read is still bounded by
// DefaultTimeout, which is the bound this package has always applied.
func TestReadsKeepTheShortDefaultBudget(t *testing.T) {
	srv := okServer(t, 0)
	c := NewNamedClient(srv.URL, "token", ClientNameAgent, "v0")
	rec := &deadlineRecorder{inner: c.http.Transport}
	c.http.Transport = rec

	if _, err := c.Status(context.Background(), "app", ""); err != nil {
		t.Fatalf("status: %v", err)
	}
	if rec.last > DefaultTimeout {
		t.Fatalf("status was given %s; a call that waits for nothing keeps the %s default", rec.last, DefaultTimeout)
	}
}

// TestBudgetIsPerRequest exercises the mechanism end to end on one client: with a default budget too
// short for the server and a deploy budget long enough, the read fails and the deploy succeeds.
// Under the single client-wide timeout this replaced, one number decided both and no such split was
// expressible.
func TestBudgetIsPerRequest(t *testing.T) {
	srv := okServer(t, 100*time.Millisecond)
	c := NewNamedClient(srv.URL, "token", ClientNameAgent, "v0")
	c.budget = budgets{def: 10 * time.Millisecond, deploy: 5 * time.Second}

	if _, err := c.Status(context.Background(), "app", ""); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("status under a 10ms budget: got %v, want a deadline error", err)
	}
	if _, err := c.Deploy(context.Background(), "app", DeployRequest{Image: "img:1"}); err != nil {
		t.Fatalf("deploy under a 5s budget: %v", err)
	}
}

// TestTimeoutSaysTheOperationMayStillBeRunning pins the message a caller gets when the budget does
// elapse. An agent reading "deadline exceeded" retries; the retry is a second deploy of an app whose
// deploy is already in flight, which is the hazard issue #404 is about.
func TestTimeoutSaysTheOperationMayStillBeRunning(t *testing.T) {
	srv := okServer(t, time.Second)
	c := NewNamedClient(srv.URL, "token", ClientNameAgent, "v0")
	c.budget = budgets{def: 10 * time.Millisecond, deploy: 10 * time.Millisecond}

	_, err := c.Deploy(context.Background(), "app", DeployRequest{Image: "img:1"})
	if err == nil {
		t.Fatal("deploy under a 10ms budget: want a timeout")
	}
	if !strings.Contains(err.Error(), "may still be completing") {
		t.Fatalf("timeout message does not say the operation may still be running: %v", err)
	}
}

// TestCallersOwnDeadlineStillWins keeps the budgets a ceiling rather than an override: a caller that
// gives a deploy one millisecond gets one millisecond, and the message stays the caller's own.
func TestCallersOwnDeadlineStillWins(t *testing.T) {
	srv := okServer(t, time.Second)
	c := NewNamedClient(srv.URL, "token", ClientNameAgent, "v0")

	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
	_, err := c.Deploy(ctx, "app", DeployRequest{Image: "img:1"})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("deploy under a caller deadline: got %v, want a deadline error", err)
	}
	if strings.Contains(err.Error(), "may still be completing") {
		t.Fatalf("a deadline the caller set should speak for itself: %v", err)
	}
}

// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

// Package sys holds the production implementations of the control plane's system seams — the
// wall Clock, a crypto/rand ID source, and the DNS Resolver — the concrete values cmd/burrowd
// injects in place of the test fakes (ADR-0010). It lives under controlplane/ (not
// controlplane/internal) so cmd/burrowd and the managed module can wire it; it is
// licensed Apache-2.0.
package sys

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net"
	"time"

	"github.com/burrow-cloud/burrow/controlplane"
)

// publicDNS are the recursive resolvers reachability queries to answer "does *public* DNS point
// at the cluster?". The system resolver (the cluster's CoreDNS) is deliberately bypassed: a
// reachability check run before the record exists — the natural check-then-add-DNS flow an agent
// follows — makes CoreDNS cache an NXDOMAIN, and a provider's SOA negative-TTL (Cloudflare's can
// be minutes) then holds that stale answer long after the record is added, so a freshly pointed
// host keeps reading as unresolved. Querying a public resolver directly avoids that cache and
// matches the question reachability actually asks.
var publicDNS = []string{"1.1.1.1:53", "8.8.8.8:53"}

var (
	_ controlplane.Clock    = Clock{}
	_ controlplane.IDSource = IDs{}
	_ controlplane.Resolver = Resolver{}
)

// Clock is the real wall clock.
type Clock struct{}

// Now returns the current time.
func (Clock) Now() time.Time { return time.Now() }

// Resolver answers reachability's DNS lookups against public recursive resolvers (publicDNS),
// falling back to the system resolver only when none are reachable (e.g. a cluster with
// restricted egress), so the check still works there.
type Resolver struct{}

// LookupHost returns the addresses host resolves to in public DNS. A genuine "not found" from a
// public resolver is returned as-is (the host really does not resolve); any other failure —
// unreachable resolver, timeout — falls back to the system resolver rather than reporting the
// host unresolved.
func (Resolver) LookupHost(ctx context.Context, host string) ([]string, error) {
	r := &net.Resolver{
		PreferGo: true, // required for the custom Dial to take effect (skip the cgo resolver)
		Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			var d net.Dialer
			var err error
			for _, server := range publicDNS {
				var conn net.Conn
				if conn, err = d.DialContext(ctx, network, server); err == nil {
					return conn, nil
				}
			}
			return nil, err
		},
	}
	addrs, err := r.LookupHost(ctx, host)
	if err == nil {
		return addrs, nil
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) && dnsErr.IsNotFound {
		return addrs, err // a real NXDOMAIN from public DNS
	}
	// Couldn't get an answer from the public resolvers — fall back to the system resolver.
	return net.DefaultResolver.LookupHost(ctx, host)
}

// IDs mints release identifiers from crypto/rand: 128 bits of randomness, hex-encoded.
type IDs struct{}

// NewID returns a fresh random identifier. It panics only if the system's secure
// random source fails, which is unrecoverable and does not happen in normal operation.
func (IDs) NewID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("sys: crypto/rand failed: " + err.Error())
	}
	return hex.EncodeToString(b[:])
}

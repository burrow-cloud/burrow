// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

// Package sys holds the production implementations of the control plane's system seams — the
// wall Clock, a crypto/rand ID source, the DNS Resolver, and the publish pre-flight's
// authoritative resolver and HTTP probe — the concrete values cmd/burrowd injects in place of the
// test fakes (ADR-0010). It lives under controlplane/ (not
// controlplane/internal) so cmd/burrowd and the managed module can wire it; it is
// licensed Apache-2.0.
package sys

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
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
	_ controlplane.Clock                 = Clock{}
	_ controlplane.IDSource              = IDs{}
	_ controlplane.Resolver              = Resolver{}
	_ controlplane.AuthoritativeResolver = AuthoritativeResolver{}
	_ controlplane.HTTPProbe             = HTTPProbe{}
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
	addrs, err := resolverAt(publicDNS...).LookupHost(ctx, host)
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

// resolverAt returns a resolver that sends its queries to the given nameservers ("host:port"), in
// order, falling through to the next when one will not take the connection. PreferGo is required
// for the custom Dial to take effect at all — the cgo resolver ignores it and reads
// /etc/resolv.conf.
func resolverAt(servers ...string) *net.Resolver {
	return &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			var d net.Dialer
			var err error
			for _, server := range servers {
				var conn net.Conn
				if conn, err = d.DialContext(ctx, network, server); err == nil {
					return conn, nil
				}
			}
			return nil, err
		},
	}
}

// AuthoritativeResolver answers a lookup at the nameservers the host's own zone is delegated to,
// which is the resolver the publish pre-flight wants: a record written moments ago is already
// there, while a recursive resolver may still be inside the negative TTL of the answer it cached
// before the record existed (ADR-0041 §3).
type AuthoritativeResolver struct{}

// LookupHostAuthoritative finds the nameservers for the closest zone containing host and asks one
// of them directly. It errors when no zone in the name has a nameserver that will answer; the
// caller falls back to the recursive Resolver rather than treating that as "does not resolve".
func (AuthoritativeResolver) LookupHostAuthoritative(ctx context.Context, host string) ([]string, error) {
	name := strings.TrimSuffix(strings.TrimSpace(host), ".")
	if name == "" {
		return nil, errors.New("authoritative lookup: host is empty")
	}
	servers, err := authoritativeServers(ctx, name)
	if err != nil {
		return nil, err
	}
	return resolverAt(servers...).LookupHost(ctx, name)
}

// authoritativeServers returns the addresses of the nameservers for the closest zone containing
// name, as "host:53". It walks the name from most specific toward the registrable domain — a
// delegation can sit at any label, so `app.example.com` may be its own zone or may be served by
// `example.com`'s nameservers — and stops at the first level that both has an NS record and has a
// nameserver whose own address resolves.
func authoritativeServers(ctx context.Context, name string) ([]string, error) {
	public := resolverAt(publicDNS...)
	labels := strings.Split(name, ".")
	// Stop before the TLD: no public zone's records are served by the root, and querying a TLD's
	// nameservers for a host under it returns a referral rather than an answer.
	for i := 0; i+2 <= len(labels); i++ {
		zone := strings.Join(labels[i:], ".")
		ns, err := public.LookupNS(ctx, zone)
		if err != nil || len(ns) == 0 {
			continue
		}
		var servers []string
		for _, n := range ns {
			addrs, err := public.LookupHost(ctx, strings.TrimSuffix(n.Host, "."))
			if err != nil {
				continue
			}
			for _, a := range addrs {
				servers = append(servers, net.JoinHostPort(a, "53"))
			}
		}
		if len(servers) > 0 {
			return servers, nil
		}
	}
	return nil, fmt.Errorf("authoritative lookup: no nameserver answers for %s", name)
}

// HTTPProbe makes the publish pre-flight's one plain-HTTP request (ADR-0041 §3). It follows no
// redirect — a 301 to HTTPS still proves the cluster answered on port 80 for that host — and it
// reads no body, because the only question is whether the request arrived at all.
type HTTPProbe struct{}

// probeTimeout bounds one pre-flight request. It is short on purpose: the pre-flight polls, so a
// slow answer is retried rather than waited out inside a single request.
const probeTimeout = 10 * time.Second

// ProbeHTTP requests url over plain HTTP and returns the status code it got back.
func (HTTPProbe) ProbeHTTP(ctx context.Context, url string) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, fmt.Errorf("probing %s: %w", url, err)
	}
	client := &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("probing %s: %w", url, err)
	}
	defer resp.Body.Close()
	return resp.StatusCode, nil
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

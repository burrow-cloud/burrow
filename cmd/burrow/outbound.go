// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package main

import "net/http"

// outboundHTTPClient is the client every request to a THIRD PARTY goes through: the GitHub latest-
// release check behind `burrow version`, a manifest URL handed to the server-side applier, and the
// public-IP echo service the VPS bootstrap queries. None of those are Burrow's control plane and
// none of them may see a Burrow credential.
//
// It exists so those calls never travel on http.DefaultClient. The default client is process-global
// mutable state: anything in the process — this repository or a dependency — can install a
// RoundTripper on it, and every caller that never asked for it then inherits whatever headers that
// RoundTripper adds. The connect package used to do exactly that with the control-plane API token
// (issue #459), which put the credential on outbound requests to api.github.com and to whatever URL
// a user passed to `apply`. That write is gone, but the calls that must never carry a credential
// should not depend on nobody ever reintroducing one, so they use a client that no credential-adding
// code path can reach.
//
// It is deliberately plain: no RoundTripper, no shared state to configure. Per-call deadlines come
// from the request context, which is how each caller already bounds itself.
var outboundHTTPClient = &http.Client{}

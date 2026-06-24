# ADR-0015: Burrow's token is sent only in X-Burrow-Token (corrects ADR-0014)

## Status

Accepted. Corrects the token-header detail in
[ADR-0014](0014-self-host-connectivity-via-kubeconfig.md); ADR-0014's core decision
(self-host connectivity via the kubeconfig and the API-server proxy) stands.

## Context

ADR-0014 said the client sends Burrow's token in **both** `Authorization: Bearer` and
`X-Burrow-Token`, reasoning that "the API-server transport owns the Authorization header on
the proxy path." That reasoning was wrong, and it produced a real failure on a real
cluster.

client-go's authentication round trippers — for both a bearer token and an `exec`
credential plugin — **do not overwrite an `Authorization` header that is already set**;
they defer to it. So when the client set `Authorization: Bearer <burrow-token>` on a
request through the API-server proxy, the kubeconfig credential was **not** applied: the
API server received Burrow's token as its own credential, failed to authenticate it, and
returned `401 Unauthorized` (a Kubernetes `Status` object, before the request ever reached
burrowd).

This was masked in CI because **k3d authenticates with a client certificate (mTLS)**, where
no `Authorization` header is involved, so setting it was harmless. The **reference
DigitalOcean cluster uses an `exec`/token kubeconfig**, which exposed the bug — exactly the
value of the real-cluster smoke test.

## Decision

**Burrow's clients send the API token only in `X-Burrow-Token`, never `Authorization`.**

On the API-server proxy path, leaving `Authorization` unset lets the kubeconfig transport
authenticate to the API server normally; the API server then proxies the request to
burrowd, forwarding `X-Burrow-Token` unchanged (verified in CI). On the direct / ingress
path the same single header carries the token. burrowd reads `X-Burrow-Token` first and
still accepts `Authorization: Bearer` as a fallback for third-party tools, but Burrow's own
client never sets it.

## Consequences

- A one-line change in the `client` package: set `X-Burrow-Token`, do not set
  `Authorization`. A regression test asserts both (token present, Authorization empty).
- The proxy path now authenticates correctly with token/`exec` kubeconfigs (DigitalOcean
  and most managed clusters), not just mTLS clusters like k3d.
- ADR-0014's "the client sends both headers" consequence is superseded by this ADR; nothing
  else in ADR-0014 changes.

## Rejected alternatives

- **Strip the burrow token from Authorization only on the proxy path, keep it on the direct
  path.** Rejected: it makes the client's behavior depend on how it was constructed, for no
  benefit — `X-Burrow-Token` works on both paths since burrowd reads it everywhere.
- **Have the client overwrite the kubeconfig credential deliberately.** Rejected: the client
  has no business managing the API server's credential; it must leave `Authorization` to the
  kubeconfig transport.

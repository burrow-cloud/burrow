// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

// Package registry holds the adapter that lists a container image repository's tags over the
// Docker Registry HTTP API v2, so burrowd can see which versions exist and compute what
// auto-deploy would take (ADR-0052). It is OUTBOUND-only and used only for the optional
// auto-deploy read/watch — never on the core deploy path, which stays independent of registry
// reachability (ADR-0040).
//
// Known gap: registries that require a non-standard auth scheme — notably AWS ECR, which signs
// requests with SigV4 — are out of scope. The standard v2 Bearer-token flow implemented here
// covers GHCR (the reference registry, ADR-0046), Docker Hub, DigitalOcean's registry, and the
// Google/Artifact-Registry token endpoints. An unsupported registry surfaces an informative
// error rather than failing the caller's whole path.
package registry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/burrow-cloud/burrow/controlplane"
)

var _ controlplane.RegistryClient = (*Client)(nil)

// ErrTagsUnavailable reports that the endpoint does not serve a usable v2 tags API (e.g. a 404 or
// an unexpected body) — the repository is absent or the host is not a v2 registry. Callers use
// errors.Is to distinguish it from a transient failure.
var ErrTagsUnavailable = errors.New("registry: tags API unavailable")

// RateLimitError reports a registry 429, carrying the raw Retry-After value the registry returned
// (seconds or an HTTP date, empty if none) so a caller can back off honoring it (ADR-0052 §7).
type RateLimitError struct {
	RetryAfter string
}

func (e *RateLimitError) Error() string {
	if e.RetryAfter == "" {
		return "registry: rate limited (http 429)"
	}
	return fmt.Sprintf("registry: rate limited (http 429), retry after %s", e.RetryAfter)
}

// Client lists an image repository's tags over the Docker Registry HTTP API v2. The caller injects
// an *http.Client with a bounded timeout so tests are deterministic and a poll never hangs on an
// unresponsive registry.
type Client struct {
	http *http.Client
}

// NewClient returns a Client using hc (defaulting to http.DefaultClient). The caller is expected to
// pass an *http.Client with a bounded timeout.
func NewClient(hc *http.Client) *Client {
	if hc == nil {
		hc = http.DefaultClient
	}
	return &Client{http: hc}
}

// tagList is the subset of the v2 /tags/list JSON we read.
type tagList struct {
	Name string   `json:"name"`
	Tags []string `json:"tags"`
}

// tokenResponse is the subset of a v2 token endpoint's JSON we read: registries return the bearer
// token under "token" and/or "access_token".
type tokenResponse struct {
	Token       string `json:"token"`
	AccessToken string `json:"access_token"`
}

// ListTags returns the tags of the repository named by imageRef, following v2 tag-list pagination
// (the Link header) and the standard Bearer-token auth flow. auth carries optional basic-auth
// credentials for a private repo; the zero value lists anonymously (public GHCR/Docker Hub/DO and
// the token-auth cloud registries). A 429 surfaces as *RateLimitError; a host that does not serve a
// usable tags API surfaces as ErrTagsUnavailable.
func (c *Client) ListTags(ctx context.Context, imageRef string, auth controlplane.RegistryAuth) ([]string, error) {
	host, repo, err := parseImageRef(imageRef)
	if err != nil {
		return nil, err
	}
	u := &url.URL{Scheme: "https", Host: host, Path: "/v2/" + repo + "/tags/list"}

	var token string
	var tags []string
	for u != nil {
		page, next, err := c.getPage(ctx, u, &token, auth, repo)
		if err != nil {
			return nil, err
		}
		tags = append(tags, page...)
		u = next
	}
	return tags, nil
}

// getPage fetches one page of tags from u, negotiating a bearer token on a 401 and retrying once,
// then returns the page's tags and the next page's URL (nil when there is no more). token is shared
// across pages and updated in place when a token is obtained.
func (c *Client) getPage(ctx context.Context, u *url.URL, token *string, auth controlplane.RegistryAuth, repo string) ([]string, *url.URL, error) {
	resp, err := c.get(ctx, u, *token)
	if err != nil {
		return nil, nil, fmt.Errorf("registry: listing tags for %s: %w", repo, err)
	}
	if resp.StatusCode == http.StatusUnauthorized {
		challenge := resp.Header.Get("WWW-Authenticate")
		resp.Body.Close()
		tok, err := c.negotiateToken(ctx, challenge, auth)
		if err != nil {
			return nil, nil, err
		}
		*token = tok
		resp, err = c.get(ctx, u, *token)
		if err != nil {
			return nil, nil, fmt.Errorf("registry: listing tags for %s: %w", repo, err)
		}
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusTooManyRequests:
		return nil, nil, &RateLimitError{RetryAfter: strings.TrimSpace(resp.Header.Get("Retry-After"))}
	case resp.StatusCode == http.StatusNotFound:
		return nil, nil, fmt.Errorf("registry: %s: repository or tags endpoint not found (http 404): %w", repo, ErrTagsUnavailable)
	case resp.StatusCode < 200 || resp.StatusCode >= 300:
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, nil, fmt.Errorf("registry: listing tags for %s failed (http %d): %s", repo, resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var tl tagList
	if err := json.NewDecoder(resp.Body).Decode(&tl); err != nil {
		return nil, nil, fmt.Errorf("registry: %s: decoding tags response: %w: %w", repo, err, ErrTagsUnavailable)
	}
	return tl.Tags, parseNextLink(resp.Header.Get("Link"), u), nil
}

// get issues an authenticated GET for the v2 API. A non-empty token is sent as a Bearer header.
func (c *Client) get(ctx context.Context, u *url.URL, token string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("building request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return c.http.Do(req)
}

// negotiateToken runs the v2 Bearer-token flow from a WWW-Authenticate challenge: it GETs the
// challenge's realm with the service and scope query params (and HTTP Basic auth from auth when
// Username is set), then returns the bearer token the endpoint issues. An anonymous call (zero-value
// auth) omits the basic-auth header, which is how public repos are listed.
func (c *Client) negotiateToken(ctx context.Context, challenge string, auth controlplane.RegistryAuth) (string, error) {
	realm, params := parseBearerChallenge(challenge)
	if realm == "" {
		return "", fmt.Errorf("registry: unauthorized and no usable Bearer realm in challenge %q", challenge)
	}
	tokenURL, err := url.Parse(realm)
	if err != nil {
		return "", fmt.Errorf("registry: parsing token realm %q: %w", realm, err)
	}
	q := tokenURL.Query()
	if svc := params["service"]; svc != "" {
		q.Set("service", svc)
	}
	if scope := params["scope"]; scope != "" {
		q.Set("scope", scope)
	}
	tokenURL.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, tokenURL.String(), nil)
	if err != nil {
		return "", fmt.Errorf("registry: building token request: %w", err)
	}
	if auth.Username != "" {
		req.SetBasicAuth(auth.Username, auth.Password)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("registry: requesting token from %s: %w", tokenURL.Host, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return "", fmt.Errorf("registry: token request failed (http %d): %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var tr tokenResponse
	if err := json.NewDecoder(resp.Body).Decode(&tr); err != nil {
		return "", fmt.Errorf("registry: decoding token response: %w", err)
	}
	if tr.Token != "" {
		return tr.Token, nil
	}
	if tr.AccessToken != "" {
		return tr.AccessToken, nil
	}
	return "", fmt.Errorf("registry: token endpoint %s returned no token", tokenURL.Host)
}

// parseImageRef splits a pullable image reference into the v2 API host and the repository, dropping
// any tag or digest. An explicit registry host is the first path segment when it contains a "." or
// ":" or is exactly "localhost"; otherwise the reference is a Docker Hub name, which maps to the
// registry-1.docker.io API host and gets the implicit "library/" prefix for a single-name repo
// (e.g. "nginx" -> "library/nginx").
func parseImageRef(ref string) (apiHost, repo string, err error) {
	s := strings.TrimSpace(ref)
	if s == "" {
		return "", "", fmt.Errorf("registry: empty image reference")
	}
	if i := strings.IndexByte(s, '@'); i >= 0 {
		s = s[:i] // drop digest
	}
	host := "docker.io"
	remainder := s
	if i := strings.IndexByte(s, '/'); i >= 0 {
		if first := s[:i]; first == "localhost" || strings.ContainsAny(first, ".:") {
			host = first
			remainder = s[i+1:]
		}
	}
	// With the host separated, any remaining colon introduces the tag (never a host port).
	if i := strings.LastIndexByte(remainder, ':'); i >= 0 {
		remainder = remainder[:i]
	}
	if remainder == "" {
		return "", "", fmt.Errorf("registry: image reference %q has no repository", ref)
	}
	if host == "docker.io" {
		host = "registry-1.docker.io"
		if !strings.Contains(remainder, "/") {
			remainder = "library/" + remainder
		}
	}
	return host, remainder, nil
}

// parseBearerChallenge parses a WWW-Authenticate Bearer challenge into its realm and the remaining
// key="value" params (service, scope, ...). A challenge without a Bearer scheme yields an empty
// realm.
func parseBearerChallenge(header string) (realm string, params map[string]string) {
	params = map[string]string{}
	h := strings.TrimSpace(header)
	if !strings.HasPrefix(strings.ToLower(h), "bearer ") {
		return "", params
	}
	for _, part := range strings.Split(h[len("bearer "):], ",") {
		part = strings.TrimSpace(part)
		eq := strings.IndexByte(part, '=')
		if eq < 0 {
			continue
		}
		key := strings.TrimSpace(part[:eq])
		val := strings.Trim(strings.TrimSpace(part[eq+1:]), `"`)
		if key == "realm" {
			realm = val
			continue
		}
		params[key] = val
	}
	return realm, params
}

// parseNextLink returns the "next" page URL from a v2 Link header, resolved against base, or nil
// when there is no next link. The header value is like `</v2/repo/tags/list?last=x&n=y>; rel="next"`.
func parseNextLink(header string, base *url.URL) *url.URL {
	if strings.TrimSpace(header) == "" {
		return nil
	}
	for _, part := range strings.Split(header, ",") {
		part = strings.TrimSpace(part)
		lb := strings.IndexByte(part, '<')
		rb := strings.IndexByte(part, '>')
		if lb < 0 || rb < 0 || rb < lb {
			continue
		}
		target := part[lb+1 : rb]
		attrs := part[rb+1:]
		if !strings.Contains(attrs, `rel="next"`) && !strings.Contains(attrs, "rel=next") {
			continue
		}
		ref, err := url.Parse(target)
		if err != nil {
			continue
		}
		return base.ResolveReference(ref)
	}
	return nil
}

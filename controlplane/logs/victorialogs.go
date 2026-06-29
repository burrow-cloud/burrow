// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

// Package logs holds the production controlplane.LogsQuerier adapters — the seam burrowd uses to
// query an installed or connected logs backing service (ADR-0026). VictoriaLogs is the first.
// It lives under controlplane/ (not controlplane/internal) so cmd/burrowd can wire it; it is
// source-available under FSL-1.1-ALv2.
package logs

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/burrow-cloud/burrow/controlplane"
)

var _ controlplane.LogsQuerier = VictoriaLogs{}

// VictoriaLogs queries a VictoriaLogs store over its HTTP LogsQL API (Apache-2.0). The store's
// in-cluster endpoint (host:port) is passed per query — burrowd reaches it directly, in-cluster.
// An optional bearer token is supported for an authenticated store; the in-cluster install passes
// "" (unauthenticated).
type VictoriaLogs struct {
	http *http.Client
}

// NewVictoriaLogs returns a VictoriaLogs querier using hc (defaulting to http.DefaultClient).
func NewVictoriaLogs(hc *http.Client) VictoriaLogs {
	if hc == nil {
		hc = http.DefaultClient
	}
	return VictoriaLogs{http: hc}
}

// QueryLogs runs a LogsQL query against the store at endpoint and returns up to limit records.
// VictoriaLogs answers /select/logsql/query as newline-delimited JSON, one object per record
// with _time and _msg fields; an empty query matches everything. A non-empty token is sent as an
// Authorization: Bearer header for an authenticated store.
func (v VictoriaLogs) QueryLogs(ctx context.Context, endpoint, query string, limit int, token string) ([]controlplane.LogEntry, error) {
	if strings.TrimSpace(query) == "" {
		query = "*"
	}
	form := url.Values{"query": {query}, "limit": {strconv.Itoa(limit)}}
	u := "http://" + endpoint + "/select/logsql/query"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, fmt.Errorf("victorialogs: building request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := v.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("victorialogs: querying %s: %w", endpoint, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("victorialogs: query failed (http %d): %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var out []controlplane.LogEntry
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			continue // skip a malformed record rather than fail the whole query
		}
		out = append(out, controlplane.LogEntry{
			Time:    str(rec["_time"]),
			Message: str(rec["_msg"]),
			Pod:     firstStr(rec, "kubernetes_pod_name", "pod", "kubernetes.pod_name"),
		})
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("victorialogs: reading response: %w", err)
	}
	return out, nil
}

func str(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func firstStr(rec map[string]any, keys ...string) string {
	for _, k := range keys {
		if s := str(rec[k]); s != "" {
			return s
		}
	}
	return ""
}

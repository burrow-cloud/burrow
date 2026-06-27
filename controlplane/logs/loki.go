// SPDX-License-Identifier: FSL-1.1-ALv2
// Copyright 2026 Nicholas Phillips

package logs

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/burrow-cloud/burrow/controlplane"
)

var _ controlplane.LogsQuerier = Loki{}

// Loki queries a Grafana Loki store over its HTTP query API (AGPL-3.0). Burrow connects to an
// existing Loki the user already runs and queries it — it never distributes Loki, so its license
// does not constrain this adapter (ADR-0026). The store's in-cluster endpoint (host:port) is
// passed per query, along with an optional bearer token for an authenticated Loki.
type Loki struct {
	http *http.Client
}

// NewLoki returns a Loki querier using hc (defaulting to http.DefaultClient).
func NewLoki(hc *http.Client) Loki {
	if hc == nil {
		hc = http.DefaultClient
	}
	return Loki{http: hc}
}

// lokiResponse is the subset of Loki's /loki/api/v1/query_range JSON we read: each result is one
// stream (a label set) with a list of [<ns timestamp string>, <log line>] value pairs.
type lokiResponse struct {
	Data struct {
		Result []struct {
			Stream map[string]string `json:"stream"`
			Values [][]string        `json:"values"`
		} `json:"result"`
	} `json:"data"`
}

// QueryLogs runs a LogQL query against the Loki at endpoint and returns up to limit records. Loki
// rejects an empty selector, so an empty caller query defaults to `{job=~".+"}` ("everything") so a
// plain "show me logs" works. Results come back direction=backward (newest-first per stream); we
// flatten across streams and cap at limit. Ordering caveat: a simple concatenation across multiple
// streams is not globally time-sorted — entries are newest-first within each stream but streams are
// concatenated in Loki's response order, which is acceptable for the agent's troubleshooting use.
// A non-empty token is sent as an Authorization: Bearer header for an authenticated Loki.
func (l Loki) QueryLogs(ctx context.Context, endpoint, query string, limit int, token string) ([]controlplane.LogEntry, error) {
	if strings.TrimSpace(query) == "" {
		query = `{job=~".+"}`
	}
	form := url.Values{
		"query":     {query},
		"limit":     {strconv.Itoa(limit)},
		"direction": {"backward"},
	}
	u := "http://" + endpoint + "/loki/api/v1/query_range?" + form.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, fmt.Errorf("loki: building request: %w", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := l.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("loki: querying %s: %w", endpoint, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("loki: query failed (http %d): %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var lr lokiResponse
	if err := json.NewDecoder(resp.Body).Decode(&lr); err != nil {
		return nil, fmt.Errorf("loki: decoding response: %w", err)
	}

	out := make([]controlplane.LogEntry, 0, limit)
	for _, stream := range lr.Data.Result {
		pod := stream.Stream["pod"]
		if pod == "" {
			pod = stream.Stream["pod_name"]
		}
		for _, v := range stream.Values {
			if len(v) < 2 {
				continue // skip a malformed value pair rather than fail the whole query
			}
			out = append(out, controlplane.LogEntry{
				Time:    lokiTime(v[0]),
				Message: v[1],
				Pod:     pod,
			})
			if len(out) >= limit {
				return out, nil
			}
		}
	}
	return out, nil
}

// lokiTime formats a Loki nanosecond-epoch timestamp string as RFC3339Nano. A value that does not
// parse is returned unchanged rather than dropped.
func lokiTime(ns string) string {
	n, err := strconv.ParseInt(ns, 10, 64)
	if err != nil {
		return ns
	}
	return time.Unix(0, n).UTC().Format(time.RFC3339Nano)
}

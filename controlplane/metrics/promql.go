// SPDX-License-Identifier: FSL-1.1-ALv2
// Copyright 2026 Nicholas Phillips

// Package metrics holds the adapters that query a metrics backing service (an installed or
// connected add-on) for the agent's metrics-query path (ADR-0026).
package metrics

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/burrow-cloud/burrow/controlplane"
)

var _ controlplane.MetricsQuerier = PromQL{}

// PromQL queries a Prometheus-HTTP-API-compatible metrics store over its instant-query API. Burrow
// connects to an existing store the user already runs and queries it — it never distributes the store
// (ADR-0026). Prometheus and VictoriaMetrics share the same /api/v1/query API, so this one adapter
// serves both. The store's in-cluster endpoint (host:port) is passed per query, along with an
// optional bearer token for an authenticated store.
type PromQL struct {
	http *http.Client
}

// NewPromQL returns a PromQL querier using hc (defaulting to http.DefaultClient).
func NewPromQL(hc *http.Client) PromQL {
	if hc == nil {
		hc = http.DefaultClient
	}
	return PromQL{http: hc}
}

// promResponse is the subset of the Prometheus instant-query JSON we read. The shape of result
// depends on resultType — a list of {metric,value} objects for a vector, a bare [ts,value] pair for
// a scalar — so it is decoded lazily as RawMessage and split on resultType.
type promResponse struct {
	Status string `json:"status"`
	Data   struct {
		ResultType string          `json:"resultType"`
		Result     json.RawMessage `json:"result"`
	} `json:"data"`
}

// promVectorSample is one element of a vector result: a label set and a [<unix ts>, "<value>"] pair.
type promVectorSample struct {
	Metric map[string]string  `json:"metric"`
	Value  [2]json.RawMessage `json:"value"`
}

// QueryMetrics runs an instant PromQL query against the store at endpoint and returns the matching
// samples. A non-empty token is sent as an Authorization: Bearer header for an authenticated store.
// A vector result yields one MetricSample per series (labels from metric, value and timestamp from
// the value pair); a scalar result yields a single MetricSample with no labels.
func (p PromQL) QueryMetrics(ctx context.Context, endpoint, query, token string) ([]controlplane.MetricSample, error) {
	form := url.Values{"query": {query}}
	u := "http://" + endpoint + "/api/v1/query?" + form.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, fmt.Errorf("promql: building request: %w", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := p.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("promql: querying %s: %w", endpoint, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("promql: query failed (http %d): %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var pr promResponse
	if err := json.NewDecoder(resp.Body).Decode(&pr); err != nil {
		return nil, fmt.Errorf("promql: decoding response: %w", err)
	}
	if pr.Status != "success" {
		return nil, fmt.Errorf("promql: query status %q (not success)", pr.Status)
	}

	switch pr.Data.ResultType {
	case "vector":
		var vec []promVectorSample
		if err := json.Unmarshal(pr.Data.Result, &vec); err != nil {
			return nil, fmt.Errorf("promql: decoding vector result: %w", err)
		}
		out := make([]controlplane.MetricSample, 0, len(vec))
		for _, s := range vec {
			ts, val := decodeValuePair(s.Value)
			out = append(out, controlplane.MetricSample{Labels: s.Metric, Value: val, Time: ts})
		}
		return out, nil
	case "scalar":
		var pair [2]json.RawMessage
		if err := json.Unmarshal(pr.Data.Result, &pair); err != nil {
			return nil, fmt.Errorf("promql: decoding scalar result: %w", err)
		}
		ts, val := decodeValuePair(pair)
		return []controlplane.MetricSample{{Value: val, Time: ts}}, nil
	default:
		return nil, fmt.Errorf("promql: unsupported result type %q (want vector or scalar)", pr.Data.ResultType)
	}
}

// decodeValuePair splits a Prometheus [<unix ts number>, "<value string>"] pair into an RFC3339Nano
// timestamp and the value string. The timestamp is a float seconds; a value that does not parse as a
// number yields an empty time rather than failing the whole query. The value is the raw string Prom
// returns, preserving its exact numeric formatting.
func decodeValuePair(pair [2]json.RawMessage) (ts, val string) {
	_ = json.Unmarshal(pair[1], &val)
	var secs float64
	if err := json.Unmarshal(pair[0], &secs); err != nil {
		return "", val
	}
	whole, frac := math.Modf(secs)
	return time.Unix(int64(whole), int64(frac*1e9)).UTC().Format(time.RFC3339Nano), val
}

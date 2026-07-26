// Package promq is a small Prometheus HTTP API client.
//
// Scope is deliberately narrow: the Go side needs instant queries for fault
// verification and scoring, nothing more. The agent's diagnostic tool layer
// lives in Python and talks to Prometheus itself.
package promq

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// NoData is returned as the value when a query succeeds but matches no series.
//
// A distinct sentinel matters here: "the metric does not exist" and "the metric
// is zero" mean very different things when deciding whether a fault fired, and
// collapsing them to 0 would mask a broken query.
var NoData = math.NaN()

// IsNoData reports whether v came back as an empty result set.
func IsNoData(v float64) bool { return math.IsNaN(v) }

type Client struct {
	base string
	http *http.Client
}

func New(baseURL string, timeout time.Duration) *Client {
	return &Client{
		base: strings.TrimRight(baseURL, "/"),
		http: &http.Client{Timeout: timeout},
	}
}

type apiResponse struct {
	Status    string `json:"status"`
	ErrorType string `json:"errorType"`
	Error     string `json:"error"`
	Data      struct {
		ResultType string `json:"resultType"`
		Result     []struct {
			Metric map[string]string `json:"metric"`
			Value  []json.RawMessage `json:"value"`
		} `json:"result"`
	} `json:"data"`
}

// Instant evaluates an instant query and returns the single scalar value.
//
// It rejects multi-series results rather than silently picking one: an
// aggregation that unexpectedly fans out means the query is wrong, and a
// verifier that quietly took result[0] would report confident nonsense.
func (c *Client) Instant(ctx context.Context, query string) (float64, error) {
	u := c.base + "/api/v1/query?query=" + url.QueryEscape(query)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return 0, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, fmt.Errorf("query prometheus: %w", err)
	}
	defer resp.Body.Close()

	var out apiResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return 0, fmt.Errorf("decode response (HTTP %d): %w", resp.StatusCode, err)
	}
	if out.Status != "success" {
		return 0, fmt.Errorf("prometheus %s: %s", out.ErrorType, out.Error)
	}

	switch n := len(out.Data.Result); {
	case n == 0:
		return NoData, nil
	case n > 1:
		return 0, fmt.Errorf("query returned %d series, expected a single scalar; "+
			"add an aggregation: %s", n, query)
	}

	pair := out.Data.Result[0].Value
	if len(pair) < 2 {
		return 0, fmt.Errorf("malformed value in result: %v", pair)
	}
	// Prometheus encodes sample values as JSON strings.
	var s string
	if err := json.Unmarshal(pair[1], &s); err != nil {
		return 0, fmt.Errorf("decode sample value: %w", err)
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, fmt.Errorf("parse sample value %q: %w", s, err)
	}
	return f, nil
}

// LabelValues lists the observed values of a label, optionally restricted to a
// metric via matches (e.g. `feature_flag_evaluation_requests_total`).
func (c *Client) LabelValues(ctx context.Context, label string, matches ...string) ([]string, error) {
	q := url.Values{}
	for _, m := range matches {
		q.Add("match[]", m)
	}
	u := c.base + "/api/v1/label/" + url.PathEscape(label) + "/values"
	if len(q) > 0 {
		u += "?" + q.Encode()
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("label values: %w", err)
	}
	defer resp.Body.Close()

	var out struct {
		Status string   `json:"status"`
		Error  string   `json:"error"`
		Data   []string `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	if out.Status != "success" {
		return nil, fmt.Errorf("prometheus: %s", out.Error)
	}
	return out.Data, nil
}

// Ready reports whether Prometheus answers a trivial query.
func (c *Client) Ready(ctx context.Context) error {
	_, err := c.Instant(ctx, "vector(1)")
	return err
}

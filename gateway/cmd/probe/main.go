// Command probe verifies that the System Under Test exposes a programmatically
// reachable observability surface.
//
// This is the single highest-risk assumption in the project: if Prometheus and
// Jaeger cannot be queried from code, the diagnostic agent has nothing to stand
// on. Run this before writing any agent logic.
//
//	go run ./cmd/probe
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"
)

type check struct {
	name   string
	ok     bool
	detail string
}

func main() {
	var (
		promURL = flag.String("prom", env("PROM_URL", "http://localhost:9090"), "Prometheus base URL")
		// Jaeger is not published on the host; it sits behind Envoy under the
		// base path /jaeger/ui. See docs/sut.md §2.
		jaegerURL = flag.String("jaeger", env("JAEGER_URL", "http://localhost:8080/jaeger/ui"), "Jaeger base URL")
		frontURL  = flag.String("frontend", env("SUT_FRONTEND_URL", "http://localhost:8080"), "SUT frontend URL")
		timeout   = flag.Duration("timeout", 5*time.Second, "per-request timeout")
	)
	flag.Parse()

	ctx := context.Background()
	client := &http.Client{Timeout: *timeout}

	checks := []check{
		probeFrontend(ctx, client, *frontURL),
		probePromHealth(ctx, client, *promURL),
		probePromServices(ctx, client, *promURL),
		probePromLatency(ctx, client, *promURL),
		probeJaegerServices(ctx, client, *jaegerURL),
	}

	failed := 0
	fmt.Println()
	for _, c := range checks {
		mark := "\033[32mPASS\033[0m"
		if !c.ok {
			mark = "\033[31mFAIL\033[0m"
			failed++
		}
		fmt.Printf("  [%s] %-26s %s\n", mark, c.name, c.detail)
	}
	fmt.Println()

	if failed > 0 {
		fmt.Printf("%d/%d checks failed. The SUT observability surface is not ready.\n",
			failed, len(checks))
		os.Exit(1)
	}
	fmt.Printf("All %d checks passed. Observability surface is queryable.\n", len(checks))
}

func probeFrontend(ctx context.Context, c *http.Client, base string) check {
	code, _, err := getRaw(ctx, c, base)
	if err != nil {
		return check{"sut frontend", false, err.Error()}
	}
	return check{"sut frontend", code < 500, fmt.Sprintf("HTTP %d at %s", code, base)}
}

func probePromHealth(ctx context.Context, c *http.Client, base string) check {
	code, _, err := getRaw(ctx, c, base+"/-/healthy")
	if err != nil {
		return check{"prometheus health", false, err.Error()}
	}
	return check{"prometheus health", code == 200, fmt.Sprintf("HTTP %d", code)}
}

// minServices is the floor below which the demo is considered not warmed up.
// A healthy run reports ~23 distinct service_name values.
const minServices = 15

// probePromServices counts distinct reporting services.
//
// Deliberately not /api/v1/targets: the demo pushes over OTLP
// (prometheus runs with --web.enable-otlp-receiver) and has zero scrape
// targets, so target health is structurally always 0/0 and says nothing.
func probePromServices(ctx context.Context, c *http.Client, base string) check {
	var resp struct {
		Status string   `json:"status"`
		Data   []string `json:"data"`
	}
	if err := getJSON(ctx, c, base+"/api/v1/label/service_name/values", &resp); err != nil {
		return check{"prometheus services", false, err.Error()}
	}
	n := len(resp.Data)
	return check{
		"prometheus services",
		n >= minServices,
		fmt.Sprintf("%d reporting (need >=%d)", n, minServices),
	}
}

// probePromLatency runs a representative PromQL query. Diagnosis depends on
// latency histograms existing, so we check for one rather than merely for `up`.
func probePromLatency(ctx context.Context, c *http.Client, base string) check {
	const q = `count(count by (job) ({__name__=~".*duration.*"}))`
	var resp struct {
		Status string `json:"status"`
		Data   struct {
			Result []struct {
				Value []any `json:"value"`
			} `json:"result"`
		} `json:"data"`
	}
	u := base + "/api/v1/query?query=" + url.QueryEscape(q)
	if err := getJSON(ctx, c, u, &resp); err != nil {
		return check{"promql duration series", false, err.Error()}
	}
	if resp.Status != "success" {
		return check{"promql duration series", false, "status=" + resp.Status}
	}
	if len(resp.Data.Result) == 0 || len(resp.Data.Result[0].Value) < 2 {
		return check{"promql duration series", false, "no duration metrics yet (traffic may need warm-up)"}
	}
	return check{"promql duration series", true,
		fmt.Sprintf("%v jobs expose duration metrics", resp.Data.Result[0].Value[1])}
}

func probeJaegerServices(ctx context.Context, c *http.Client, base string) check {
	var resp struct {
		Data []string `json:"data"`
	}
	if err := getJSON(ctx, c, base+"/api/services", &resp); err != nil {
		return check{"jaeger services", false, err.Error()}
	}
	sort.Strings(resp.Data)
	sample := resp.Data
	if len(sample) > 4 {
		sample = sample[:4]
	}
	return check{
		"jaeger services",
		len(resp.Data) > 0,
		fmt.Sprintf("%d services (%s...)", len(resp.Data), strings.Join(sample, ", ")),
	}
}

func getRaw(ctx context.Context, c *http.Client, u string) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return 0, nil, err
	}
	resp, err := c.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	return resp.StatusCode, nil, nil
}

func getJSON(ctx context.Context, c *http.Client, u string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

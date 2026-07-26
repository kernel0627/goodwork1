package inject

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/kernel0627/medic/gateway/internal/promq"
	"github.com/kernel0627/medic/gateway/internal/signals"
)

func loadSignals(t *testing.T) *signals.Catalog {
	t.Helper()
	c, err := signals.LoadFromRepo()
	if err != nil {
		t.Fatalf("load signal catalog: %v", err)
	}
	return c
}

func TestCatalogIsWellFormed(t *testing.T) {
	specs := Catalog()
	if len(specs) != 13 {
		t.Errorf("catalog has %d faults, expected the 13 fault-injecting flags", len(specs))
	}

	seen := map[string]bool{}
	for _, s := range specs {
		if seen[s.Flag] {
			t.Errorf("duplicate flag %q", s.Flag)
		}
		seen[s.Flag] = true

		if strings.HasPrefix(s.Flag, "loadGenerator") {
			t.Errorf("%s is a load knob, not a fault: it moves every service's baseline "+
				"and must be held fixed across control arms", s.Flag)
		}
		if s.Variant == "" || s.Variant == OffVariant {
			t.Errorf("%s: characterisation variant is %q, must be an active variant", s.Flag, s.Variant)
		}
		if s.Signal == "" {
			t.Errorf("%s: no Signal, so the fault can never be verified", s.Flag)
		}
		if s.RootCause == "" || s.SymptomAt == "" {
			t.Errorf("%s: RootCause and SymptomAt are required for attribution scoring", s.Flag)
		}
		// Every entry's blast radius was established by reading the SUT's
		// source. Recording where keeps a later reader from re-deriving it from
		// the flag's name, which is how this catalog went wrong once.
		if s.Site == "" {
			t.Errorf("%s: no Site recorded; the blast radius must be traceable to source", s.Flag)
		}
	}
}

// TestCatalogSignalsResolve checks each fault's signal exists, its placeholders
// are all supplied, and it carries a threshold.
//
// The substitution check is the important one. An unsubstituted %SERVICE% left
// in PromQL does not error -- it matches nothing and returns zero, which reads
// exactly like a healthy service, so the fault would be dismissed as inert.
func TestCatalogSignalsResolve(t *testing.T) {
	sigs := loadSignals(t)
	for _, s := range Catalog() {
		t.Run(s.Flag, func(t *testing.T) {
			sig, ok := sigs.Get(s.Signal)
			if !ok {
				t.Fatalf("signal %q not in queries/signals.yaml (have %v)", s.Signal, sigs.Names())
			}
			if sig.MultiSeries {
				t.Errorf("signal %q returns many series and cannot drive a pass/fail decision", s.Signal)
			}
			threshold := sig.MinDelta
			if s.MinDeltaOverride > 0 {
				threshold = s.MinDeltaOverride
			}
			if threshold <= 0 {
				t.Errorf("no threshold for signal %q; any noise would pass tier 2", s.Signal)
			}
			if _, err := sigs.Query(s.Signal, s.Subs); err != nil {
				t.Errorf("query does not resolve: %v", err)
			}
		})
	}
}

// TestCatalogVariantsExist keeps the catalog honest against the real flag file:
// a variant that does not exist would be rejected at injection time, but only
// after a scenario had already been scheduled.
func TestCatalogVariantsExist(t *testing.T) {
	path := stage(t)
	in, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range Catalog() {
		if err := in.Set(s.Flag, s.Variant); err != nil {
			t.Errorf("catalog entry %s=%s not injectable: %v", s.Flag, s.Variant, err)
		}
	}
}

// TestFaultsUnreachableBySyntheticLoadAreFlagged pins the two faults the k6 load
// generator cannot drive, so neither silently ends up in the scenario library.
// Should upstream change either, this test is the place that notices.
func TestFaultsUnreachableBySyntheticLoadAreFlagged(t *testing.T) {
	want := map[string]string{
		"imageSlowLoad":        "read by a browser component; k6 does not render pages",
		"failedReadinessProbe": "flips a health check and never touches a request path",
		"intlShippingSlowdown": "applies only to non-US addresses, and every entry in " +
			"load-generator/people.json is \"United States\"",
	}
	for _, s := range Catalog() {
		reason, unreachable := want[s.Flag]
		switch {
		case unreachable && s.SyntheticLoadReaches:
			t.Errorf("%s is marked reachable but %s", s.Flag, reason)
		case !unreachable && !s.SyntheticLoadReaches:
			t.Errorf("%s is marked unreachable by synthetic load; if that is right, "+
				"record why here so it is not mistaken for an oversight", s.Flag)
		}
	}
}

// TestServicesWithoutServerMetricsUseAClientSignal guards a measurement trap.
//
// payment, recommendation and email report no server-side request metrics at all
// (all three measured at 0.000 req/s), so error_ratio on those services is
// permanently zero however hard they are failing. Faults there must be measured
// from a caller's client metrics or from container resources instead.
func TestServicesWithoutServerMetricsUseAClientSignal(t *testing.T) {
	noServerMetrics := map[string]bool{"payment": true, "recommendation": true, "email": true}
	for _, s := range Catalog() {
		if !noServerMetrics[s.Subs["service"]] {
			continue
		}
		if s.Signal == "error_ratio" || s.Signal == "error_rate" {
			t.Errorf("%s measures %q on %s, which reports no server-side metrics; "+
				"use client_error_ratio from a caller, or a container resource signal",
				s.Flag, s.Signal, s.Subs["service"])
		}
	}
}

// TestCatalogQueriesAreScalar runs every fault's verification query against a
// live Prometheus and requires each to evaluate to exactly one value.
//
// This is the test that catches what these queries are prone to. PromQL fails
// quietly: a malformed fallback chain, a mistyped label, or an aggregation that
// unexpectedly fans out all return *something*. The fault then reads as inert
// for a reason that has nothing to do with the fault.
//
// Skipped when Prometheus is unreachable so the unit suite stays hermetic.
func TestCatalogQueriesAreScalar(t *testing.T) {
	base := os.Getenv("PROM_URL")
	if base == "" {
		base = "http://localhost:9090"
	}
	prom := promq.New(base, 10*time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if err := prom.Ready(ctx); err != nil {
		t.Skipf("prometheus unreachable at %s (%v); run `make sut-up` to exercise this test", base, err)
	}
	sigs := loadSignals(t)

	for _, s := range Catalog() {
		t.Run(s.Flag, func(t *testing.T) {
			q, err := sigs.Query(s.Signal, s.Subs)
			if err != nil {
				t.Fatalf("resolve query: %v", err)
			}
			v, err := prom.Instant(ctx, q)
			if err != nil {
				t.Fatalf("query failed: %v\nquery: %s", err, q)
			}
			// Every scalar signal ends in `or vector(0)`, so no-data points at a
			// short-circuited fallback chain rather than at a quiet system.
			if promq.IsNoData(v) {
				t.Errorf("query returned no data despite its vector(0) default; "+
					"the fallback chain is probably short-circuited\nquery: %s", q)
			}
		})
	}
}

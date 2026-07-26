package inject

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/kernel0627/medic/gateway/internal/promq"
)

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
		if s.Effect == "" {
			t.Errorf("%s: no Effect query, so the fault can never be verified", s.Flag)
		}
		if s.RootCause == "" || s.SymptomAt == "" {
			t.Errorf("%s: RootCause and SymptomAt are required for attribution scoring", s.Flag)
		}
		if thresholdFor(s) == 0 {
			t.Errorf("%s: class %q has no threshold, so any noise would pass tier 2", s.Flag, s.Class)
		}
		// Every entry's blast radius was established by reading the SUT's
		// source. Recording where keeps a later reader from re-deriving it
		// from the flag's name, which is how this catalog went wrong once.
		if s.Site == "" {
			t.Errorf("%s: no Site recorded; the blast radius must be traceable to source", s.Flag)
		}
	}
}

// TestFaultsUnreachableBySyntheticLoadAreFlagged pins the two faults that the
// k6 load generator cannot drive, so neither silently ends up in the scenario
// library. Should upstream change either, this test is the place that notices.
func TestFaultsUnreachableBySyntheticLoadAreFlagged(t *testing.T) {
	want := map[string]string{
		"imageSlowLoad":        "read by a browser component; k6 does not render pages",
		"failedReadinessProbe": "flips a health check and never touches a request path",
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

// TestCatalogQueriesAreScalar runs every verification query against a live
// Prometheus and requires each to evaluate to exactly one value.
//
// This is the test that catches the failure mode these queries are prone to.
// PromQL fails quietly: a malformed fallback chain, a mistyped label, or an
// aggregation that unexpectedly fans out all return *something*. The fault then
// reads as inert and gets dropped from the scenario library for a reason that
// has nothing to do with the fault.
//
// Skipped when Prometheus is not reachable so the unit suite stays hermetic.
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

	for _, s := range Catalog() {
		t.Run(s.Flag, func(t *testing.T) {
			for name, q := range map[string]string{
				"effect": s.Effect,
				"tier1":  EvaluationQuery(s.Flag),
			} {
				v, err := prom.Instant(ctx, q)
				if err != nil {
					t.Errorf("%s query failed: %v\nquery: %s", name, err, strings.TrimSpace(q))
					continue
				}
				// NoData is a legitimate answer for tier 1 -- it means nothing
				// evaluates the flag. For the effect query the `or vector(0)`
				// tail should guarantee a value, so its absence points at a
				// broken fallback chain rather than at a quiet system.
				if name == "effect" && promq.IsNoData(v) {
					t.Errorf("effect query returned no data despite its vector(0) default; "+
						"the fallback chain is probably short-circuited\nquery: %s",
						strings.TrimSpace(q))
				}
			}
		})
	}
}

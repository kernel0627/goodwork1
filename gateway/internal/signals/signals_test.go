package signals

import (
	"strings"
	"testing"
)

func load(t *testing.T) *Catalog {
	t.Helper()
	c, err := LoadFromRepo()
	if err != nil {
		t.Fatalf("LoadFromRepo: %v", err)
	}
	return c
}

func TestCatalogLoads(t *testing.T) {
	c := load(t)
	if len(c.Names()) < 10 {
		t.Errorf("only %d signals loaded: %v", len(c.Names()), c.Names())
	}
	for _, must := range []string{
		"error_ratio", "error_rate", "latency_p99", "cpu", "memory",
		"consumer_lag", "flag_evaluations", "client_error_ratio",
	} {
		if _, ok := c.Get(must); !ok {
			t.Errorf("signal %q missing", must)
		}
	}
}

// TestScalarSignalsDeclareAThreshold guards against a signal that can be used
// for a pass/fail decision without saying what counts as a change. Absent a
// threshold the comparison degenerates to delta > 0, which any noise satisfies.
func TestScalarSignalsDeclareAThreshold(t *testing.T) {
	c := load(t)
	for _, name := range c.Names() {
		s, _ := c.Get(name)
		if s.MultiSeries {
			continue // not used for pass/fail
		}
		if s.MinDelta == 0 && name != "flag_evaluations" {
			t.Errorf("signal %q has no min_delta; any noise would count as an effect", name)
		}
		if s.Unit == "" {
			t.Errorf("signal %q has no unit; a threshold without units is unreadable", name)
		}
	}
}

func TestQuerySubstitutes(t *testing.T) {
	c := load(t)
	q, err := c.Query("error_ratio", map[string]string{"service": "checkout"})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if !strings.Contains(q, `service_name="checkout"`) {
		t.Errorf("service not substituted:\n%s", q)
	}
	if !strings.Contains(q, "[2m]") {
		t.Errorf("window default not applied:\n%s", q)
	}
	if strings.Contains(q, "%") {
		t.Errorf("placeholder survived substitution:\n%s", q)
	}
}

// TestQueryRejectsMissingSubstitution is the point of returning an error rather
// than passing the query through. A literal %SERVICE% in PromQL does not fail:
// it matches nothing and yields zero, which is indistinguishable from a healthy
// service.
func TestQueryRejectsMissingSubstitution(t *testing.T) {
	c := load(t)
	if q, err := c.Query("error_ratio", nil); err == nil {
		t.Errorf("Query with no substitutions succeeded, returning:\n%s", q)
	}
	if _, err := c.Query("client_error_ratio", map[string]string{"caller": "checkout"}); err == nil {
		t.Error("Query succeeded with RPC_PATTERN left unsubstituted")
	}
}

func TestQueryRejectsUnknownSignal(t *testing.T) {
	c := load(t)
	if _, err := c.Query("no_such_signal", nil); err == nil {
		t.Error("Query accepted an unknown signal name")
	}
}

// TestNewSemconvStatusLabels pins the four status conventions this SUT mixes.
//
// The failure this prevents is specific and silent: PromQL treats an absent
// label as empty, so `rpc_grpc_status_code!="0"` applied to a metric family
// that has no such label matches *every* series. Written that way, the error
// rate for checkout and product-catalog equalled their total request rate --
// every request counted as a failure.
func TestNewSemconvStatusLabels(t *testing.T) {
	c := load(t)
	for _, name := range []string{"error_rate", "error_ratio"} {
		s, ok := c.Get(name)
		if !ok {
			t.Fatalf("%s missing", name)
		}
		if strings.Contains(s.PromQL, `rpc_server_call_duration_seconds_count{service_name="%SERVICE%",rpc_grpc_status_code`) {
			t.Errorf("%s filters the new-semconv RPC family on rpc_grpc_status_code, "+
				"which it does not carry; the absent label makes the matcher select "+
				"every series and every request count as an error. Use "+
				"rpc_response_status_code!=\"OK\".", name)
		}
		for _, want := range []string{
			`rpc_grpc_status_code!="0"`,        // old RPC semconv: ad
			`rpc_response_status_code!="OK"`,   // new RPC semconv: checkout, product-catalog
			`http_status_code=~"5.."`,          // old HTTP semconv: frontend
			`http_response_status_code=~"5.."`, // new HTTP semconv: cart
		} {
			if !strings.Contains(s.PromQL, want) {
				t.Errorf("%s does not cover %s; services on that convention "+
					"would read as permanently healthy", name, want)
			}
		}
	}
}

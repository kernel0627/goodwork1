package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kernel0627/medic/gateway/internal/agentclient"
	"github.com/kernel0627/medic/gateway/internal/inject"
	"github.com/kernel0627/medic/gateway/internal/promq"
	"github.com/kernel0627/medic/gateway/internal/scenario"
	"github.com/kernel0627/medic/gateway/internal/signals"
)

// fakeProm serves canned instant-query answers.
//
// Values are matched by substring of the query, since the real PromQL is long and
// generated. Anything unmatched answers with an empty result set, which is how
// Prometheus reports a metric that does not exist.
type fakeProm struct {
	values map[string]float64
	server *httptest.Server
}

func newFakeProm(t *testing.T, values map[string]float64) *fakeProm {
	t.Helper()
	f := &fakeProm{values: values}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query().Get("query")
		if query == "vector(1)" {
			writeScalar(w, 1)
			return
		}
		for needle, value := range f.values {
			if strings.Contains(query, needle) {
				writeScalar(w, value)
				return
			}
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"status":"success","data":{"resultType":"vector","result":[]}}`)
	}))
	t.Cleanup(f.server.Close)
	return f
}

func writeScalar(w http.ResponseWriter, v float64) {
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w,
		`{"status":"success","data":{"resultType":"vector","result":[{"metric":{},"value":[1,"%g"]}]}}`, v)
}

func (f *fakeProm) client() *promq.Client {
	return promq.New(f.server.URL, 5*time.Second)
}

func loadSignals(t *testing.T) *signals.Catalog {
	t.Helper()
	c, err := signals.LoadFromRepo()
	if err != nil {
		t.Fatalf("load signals: %v", err)
	}
	return c
}

// stagedInjector points an injector at a throwaway copy of the flag fixture, so
// no test can write into the real SUT.
func stagedInjector(t *testing.T) *inject.Injector {
	t.Helper()
	src, err := os.ReadFile(filepath.Join("..", "inject", "testdata", "demo.flagd.json"))
	if err != nil {
		t.Fatalf("read flag fixture: %v", err)
	}
	path := filepath.Join(t.TempDir(), "demo.flagd.json")
	if err := os.WriteFile(path, src, 0o644); err != nil {
		t.Fatalf("stage flag fixture: %v", err)
	}
	in, err := inject.New(path)
	if err != nil {
		t.Fatalf("inject.New: %v", err)
	}
	return in
}

func newFakeAgentClient(baseURL string) *agentclient.Client {
	return agentclient.New(baseURL, 5*time.Second)
}

// paymentScenario has the property that makes alert leakage dangerous: the root
// cause is payment while the symptom lands on checkout.
func paymentScenario() scenario.Scenario {
	return scenario.Scenario{
		ID:             "paymentFailure-L2",
		Difficulty:     "L2",
		Faults:         []scenario.Fault{{Flag: "paymentFailure", Variant: "100%"}},
		SettleSeconds:  150,
		RecoverSeconds: 90,
		MaxSteps:       20,
		Signal:         "client_error_ratio",
		// payment reports no server-side metrics, so its fault is confirmed from
		// the caller's client metrics -- the same substitution the generated
		// library carries.
		Subs: map[string]string{
			"caller":      "checkout",
			"rpc_pattern": "oteldemo.PaymentService/.*",
		},
		MinDelta: 0.01,
		Truth: scenario.GroundTruth{
			RootCauseService: "payment",
			RootCauseClass:   "error",
			SymptomService:   "checkout",
		},
	}
}

// TestAlertNeverNamesTheRootCause is the load-bearing test of this package.
//
// The alert is the only channel from runner to agent. If the root cause could
// appear in it, the benchmark would be measuring the agent's ability to read
// rather than to diagnose, and every L2 and L3 score would be meaningless.
func TestAlertNeverNamesTheRootCause(t *testing.T) {
	prom := newFakeProm(t, map[string]float64{
		`service_name="checkout"`:    0.42, // symptom service is failing
		`container_name="`:           0,
		"kafka_consumer_records_lag": 0,
	})
	r := &Runner{Prom: prom.client(), Signals: loadSignals(t)}
	s := paymentScenario()

	baseline := map[string]float64{"error_ratio": 0, "latency_p99": 0}
	alert := r.composeAlert(context.Background(), s, baseline)

	if alert.Service != "checkout" {
		t.Errorf("alert should fire on the symptom service, got %q", alert.Service)
	}
	haystack := strings.ToLower(alert.Service + " " + alert.Summary)
	for _, forbidden := range []string{"payment", "paymentfailure", "100%"} {
		if strings.Contains(haystack, forbidden) {
			t.Errorf("alert leaks %q — the agent would be reading, not diagnosing:\n%s",
				forbidden, alert.Summary)
		}
	}
	if !strings.Contains(alert.Summary, "checkout") {
		t.Errorf("alert should describe the symptom service:\n%s", alert.Summary)
	}
}

// TestAlertPicksTheProbeThatMovedRelatively guards a units bug. The probes are a
// ratio, milliseconds, cores, a percentage and a record count, so ranking them by
// raw delta would always select whichever has the largest scale -- latency in
// milliseconds would win over any error ratio, forever.
func TestAlertPicksTheProbeThatMovedRelatively(t *testing.T) {
	prom := newFakeProm(t, map[string]float64{
		// p99 grew 10ms in absolute terms but only 10%; the ratio went 0.01 -> 0.30.
		`rpc_server_duration_milliseconds_bucket{service_name="cart"`: 0,
		`service_name="cart"`: 0.30,
	})
	r := &Runner{Prom: prom.client(), Signals: loadSignals(t)}
	s := scenario.Scenario{
		ID:            "x",
		SettleSeconds: 60,
		Truth:         scenario.GroundTruth{SymptomService: "cart", RootCauseService: "cart", RootCauseClass: "error"},
	}

	alert := r.composeAlert(context.Background(), s, map[string]float64{
		"error_ratio": 0.01,
		"latency_p99": 100,
	})
	if alert.Observed.Signal != "error_ratio" {
		t.Errorf("expected the relatively larger move (error ratio) to be reported, got %q",
			alert.Observed.Signal)
	}
}

// TestHealthyControlAlertSaysItMayBeSpurious. A control still pages the agent --
// a real oncall is sometimes woken for nothing -- and the whole measurement is
// whether the agent invents a cause anyway.
func TestHealthyControlAlertSaysItMayBeSpurious(t *testing.T) {
	prom := newFakeProm(t, map[string]float64{`service_name="frontend"`: 0.001})
	r := &Runner{Prom: prom.client(), Signals: loadSignals(t)}
	s := scenario.Scenario{
		ID:            "healthy-control-01",
		SettleSeconds: 30,
		Truth:         scenario.GroundTruth{Healthy: true, SymptomService: "frontend"},
	}

	alert := r.composeAlert(context.Background(), s, map[string]float64{"error_ratio": 0.001})
	if !strings.Contains(alert.Summary, "spurious") {
		t.Errorf("a control's alert should admit it may be spurious:\n%s", alert.Summary)
	}
	if strings.Contains(strings.ToLower(alert.Summary), "healthy control") {
		t.Errorf("the alert must not reveal that this is a control:\n%s", alert.Summary)
	}
}

func TestProbeSymptomsHandlesMissingMetrics(t *testing.T) {
	// A service with no metrics at all: every probe returns an empty result set,
	// which must fold to zero rather than propagate NaN into the alert.
	prom := newFakeProm(t, nil)
	r := &Runner{Prom: prom.client(), Signals: loadSignals(t)}

	got := r.probeSymptoms(context.Background(), "nonexistent")
	for name, value := range got {
		if value != 0 {
			t.Errorf("probe %s should fold no-data to 0, got %v", name, value)
		}
	}
	if empty := r.probeSymptoms(context.Background(), ""); len(empty) != 0 {
		t.Errorf("no symptom service means no probes, got %v", empty)
	}
}

func TestFormatValueUsesEachUnit(t *testing.T) {
	cases := []struct {
		value float64
		unit  string
		want  string
	}{
		{0.0952, "ratio", "9.5%"},
		{98.34, "percent", "98.3%"},
		{2485, "milliseconds", "2485ms"},
		{18.6, "cores", "18.60 cores"},
		{1200, "records", "1200 records"},
	}
	for _, c := range cases {
		if got := formatValue(c.value, c.unit); got != c.want {
			t.Errorf("formatValue(%v, %q) = %q, want %q", c.value, c.unit, got, c.want)
		}
	}
}

// TestUnconfirmedFaultSkipsTheAgentEntirely. Asking would spend tokens on an
// answer that gets discarded, and the fake agent below records whether it was
// called at all.
func TestUnconfirmedFaultSkipsTheAgentEntirely(t *testing.T) {
	var called bool
	agentSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/diagnose") {
			called = true
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "brain": "fake", "tools": 0})
	}))
	defer agentSrv.Close()

	// Signal reads 0 before and after, so the delta cannot reach min_delta.
	prom := newFakeProm(t, map[string]float64{"rpc_client": 0})
	inj := stagedInjector(t)

	r := &Runner{
		Inj: inj, Prom: prom.client(), Signals: loadSignals(t),
		Agent: newFakeAgentClient(agentSrv.URL),
	}
	s := paymentScenario()
	s.SettleSeconds = 1
	s.RecoverSeconds = 0

	ep, err := r.runOne(context.Background(), s)
	if err != nil {
		t.Fatalf("runOne returned an error: %v", err)
	}
	if ep.FaultConfirmed {
		t.Error("fault should not be confirmed when the signal did not move")
	}
	if called {
		t.Error("the agent must not be asked about an episode that cannot be scored")
	}
}

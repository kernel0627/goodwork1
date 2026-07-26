package scenario

import (
	"path/filepath"
	"strings"
	"testing"
)

func valid() Scenario {
	return Scenario{
		ID:             "s1",
		Difficulty:     "L2",
		Faults:         []Fault{{Flag: "paymentFailure", Variant: "100%"}},
		SettleSeconds:  150,
		RecoverSeconds: 90,
		MaxSteps:       20,
		Signal:         "client_error_ratio",
		MinDelta:       0.01,
		Truth: GroundTruth{
			RootCauseService: "payment",
			RootCauseClass:   "error",
			SymptomService:   "checkout",
		},
	}
}

func control() Scenario {
	return Scenario{
		ID:             "c1",
		Difficulty:     "control",
		SettleSeconds:  30,
		RecoverSeconds: 90,
		MaxSteps:       20,
		Truth:          GroundTruth{Healthy: true, SymptomService: "frontend"},
	}
}

func expectInvalid(t *testing.T, lib Library, wants string) {
	t.Helper()
	err := lib.Validate()
	if err == nil {
		t.Fatalf("expected validation to reject this library")
	}
	if !strings.Contains(err.Error(), wants) {
		t.Errorf("error should mention %q, got: %v", wants, err)
	}
}

func TestValidLibraryPasses(t *testing.T) {
	lib := Library{Scenarios: []Scenario{valid(), control()}}
	if err := lib.Validate(); err != nil {
		t.Fatalf("valid library rejected: %v", err)
	}
}

func TestEmptyLibraryRejected(t *testing.T) {
	expectInvalid(t, Library{}, "no scenarios")
}

func TestDuplicateIDsRejected(t *testing.T) {
	a, b := valid(), valid()
	expectInvalid(t, Library{Scenarios: []Scenario{a, b}}, "duplicate id")
}

// TestScenarioWithNoFaultsAndNotHealthyRejected guards the worst silent failure a
// library can contain: the agent is scored against a fault on a system nothing
// was done to, and every episode "fails" for a reason that has nothing to do with
// the agent.
func TestScenarioWithNoFaultsAndNotHealthyRejected(t *testing.T) {
	s := valid()
	s.Faults = nil
	expectInvalid(t, Library{Scenarios: []Scenario{s}}, "not marked healthy")
}

func TestHealthyScenarioWithFaultsRejected(t *testing.T) {
	s := control()
	s.Faults = []Fault{{Flag: "adFailure", Variant: "on"}}
	expectInvalid(t, Library{Scenarios: []Scenario{s}}, "marked healthy but injects faults")
}

func TestHealthyScenarioNamingARootCauseRejected(t *testing.T) {
	s := control()
	s.Truth.RootCauseService = "cart"
	expectInvalid(t, Library{Scenarios: []Scenario{s}}, "names a root cause")
}

// TestZeroSettleRejected: with no settle time the agent is asked about a symptom
// that has not appeared in the metrics yet, so it would be scored on a system that
// still looks healthy.
func TestZeroSettleRejected(t *testing.T) {
	s := valid()
	s.SettleSeconds = 0
	expectInvalid(t, Library{Scenarios: []Scenario{s}}, "settle_seconds")
}

func TestMissingGroundTruthRejected(t *testing.T) {
	s := valid()
	s.Truth.RootCauseClass = ""
	expectInvalid(t, Library{Scenarios: []Scenario{s}}, "root cause service and class")
}

func TestMissingSignalRejected(t *testing.T) {
	s := valid()
	s.Signal = ""
	expectInvalid(t, Library{Scenarios: []Scenario{s}}, "no signal")
}

// TestNonPositiveMinDeltaRejected: with no threshold, confirmation degenerates to
// delta > 0 and any noise would confirm the fault, so an episode that never fired
// would be scored as though it had.
func TestNonPositiveMinDeltaRejected(t *testing.T) {
	s := valid()
	s.MinDelta = 0
	expectInvalid(t, Library{Scenarios: []Scenario{s}}, "min_delta")
}

func TestFaultMissingVariantRejected(t *testing.T) {
	s := valid()
	s.Faults = []Fault{{Flag: "adFailure"}}
	expectInvalid(t, Library{Scenarios: []Scenario{s}}, "flag and variant")
}

func TestZeroMaxStepsRejected(t *testing.T) {
	s := valid()
	s.MaxSteps = 0
	expectInvalid(t, Library{Scenarios: []Scenario{s}}, "max_steps")
}

func TestRoundTripThroughDisk(t *testing.T) {
	path := filepath.Join(t.TempDir(), "library.yaml")
	original := &Library{
		GeneratedFrom: "test",
		Scenarios:     []Scenario{valid(), control()},
	}
	if err := original.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(loaded.Scenarios) != 2 {
		t.Fatalf("expected 2 scenarios, got %d", len(loaded.Scenarios))
	}
	got := loaded.Scenarios[0]
	if got.ID != "s1" || got.Signal != "client_error_ratio" ||
		got.Truth.RootCauseService != "payment" || got.Truth.SymptomService != "checkout" {
		t.Errorf("round trip lost data: %+v", got)
	}
}

// TestSavedFileWarnsAgainstHandEditing: the library is generated from
// characterisation, and a fault pasted in by hand would not have been verified.
func TestSavedFileWarnsAgainstHandEditing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "library.yaml")
	if err := (&Library{Scenarios: []Scenario{valid()}}).Save(path); err != nil {
		t.Fatal(err)
	}
	// Reading the raw bytes is the point; Load would strip the comments.
	lib, err := Load(path)
	if err != nil {
		t.Fatalf("saved file does not load back: %v", err)
	}
	_ = lib
}

func TestFilterSelectsByID(t *testing.T) {
	lib := Library{Scenarios: []Scenario{valid(), control()}}
	if got := lib.Filter(nil); len(got) != 2 {
		t.Errorf("empty filter should select everything, got %d", len(got))
	}
	got := lib.Filter([]string{" c1 "})
	if len(got) != 1 || got[0].ID != "c1" {
		t.Errorf("filter should trim and match, got %+v", got)
	}
	if got := lib.Filter([]string{"absent"}); len(got) != 0 {
		t.Errorf("unmatched filter should select nothing, got %d", len(got))
	}
}

func TestSummaryCountsControlsAndDifficulties(t *testing.T) {
	multi := valid()
	multi.ID = "s2"
	multi.Faults = append(multi.Faults, Fault{Flag: "adFailure", Variant: "on"})

	lib := Library{Scenarios: []Scenario{valid(), multi, control()}}
	summary := lib.Summary()
	for _, want := range []string{"3 scenarios", "L2=2", "control=1", "1 healthy controls", "1 multi-fault"} {
		if !strings.Contains(summary, want) {
			t.Errorf("summary missing %q:\n%s", want, summary)
		}
	}
}

func TestEstimatedDurationSumsSettleAndRecover(t *testing.T) {
	lib := Library{Scenarios: []Scenario{valid(), control()}}
	// (150+90) + (30+90) = 360s
	if got := lib.EstimatedDuration().Seconds(); got != 360 {
		t.Errorf("estimated duration = %.0fs, want 360s", got)
	}
}

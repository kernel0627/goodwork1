package scoring

import (
	"math"
	"strings"
	"testing"

	"github.com/kernel0627/medic/gateway/internal/scenario"
)

func faultScenario(id, difficulty, cause, class, symptom string) scenario.Scenario {
	return scenario.Scenario{
		ID:         id,
		Difficulty: difficulty,
		Faults:     []scenario.Fault{{Flag: "someFlag", Variant: "on"}},
		MinDelta:   0.01,
		Truth: scenario.GroundTruth{
			RootCauseService: cause,
			RootCauseClass:   class,
			SymptomService:   symptom,
		},
	}
}

func healthyScenario(id string) scenario.Scenario {
	return scenario.Scenario{
		ID:         id,
		Difficulty: "control",
		Truth:      scenario.GroundTruth{Healthy: true},
	}
}

func confirmed(s scenario.Scenario, a Answer) Episode {
	return Episode{Scenario: s, Answer: a, FaultConfirmed: true, ObservedDelta: 1}
}

func TestExactAnswerScores(t *testing.T) {
	s := grade(confirmed(
		faultScenario("s1", "L2", "payment", "error", "checkout"),
		Answer{RootCauseService: "payment", RootCauseClass: "error"},
	))
	if !s.Valid || !s.ExactCorrect || !s.ServiceCorrect || !s.ClassCorrect {
		t.Fatalf("expected a fully correct score, got %+v", s)
	}
	if s.SymptomEcho {
		t.Error("a correct answer must not also count as symptom echo")
	}
}

func TestServiceRightClassWrongIsNotExact(t *testing.T) {
	s := grade(confirmed(
		faultScenario("s1", "L2", "payment", "error", "checkout"),
		Answer{RootCauseService: "payment", RootCauseClass: "latency"},
	))
	if !s.ServiceCorrect {
		t.Error("service should be correct")
	}
	if s.ClassCorrect || s.ExactCorrect {
		t.Error("class was wrong, so exact must be false")
	}
}

func TestMatchingIsCaseAndSpaceInsensitive(t *testing.T) {
	s := grade(confirmed(
		faultScenario("s1", "L1", "product-catalog", "error", "product-catalog"),
		Answer{RootCauseService: "  Product-Catalog ", RootCauseClass: "ERROR"},
	))
	if !s.ExactCorrect {
		t.Error("answers should not be penalised for case or surrounding space")
	}
}

// TestSymptomEchoIsOnlyCountedWhenCauseAndSymptomDiffer is the point of the
// metric. It catches an agent that names the alerting service, which scores well
// on L1 -- where symptom and cause coincide -- and zero on L2 and L3. Counting
// echoes in scenarios where they coincide would dilute the rate and make the
// strategy look better than it is.
func TestSymptomEchoIsOnlyCountedWhenCauseAndSymptomDiffer(t *testing.T) {
	echoed := grade(confirmed(
		faultScenario("s1", "L2", "payment", "connectivity", "checkout"),
		Answer{RootCauseService: "checkout", RootCauseClass: "error"},
	))
	if !echoed.SymptomEcho {
		t.Error("naming the symptom service should count as echo")
	}
	if echoed.ExactCorrect {
		t.Error("an echo is not a correct answer")
	}

	// Cause == symptom: naming it is correct, not an echo.
	coincident := grade(confirmed(
		faultScenario("s2", "L1", "ad", "error", "ad"),
		Answer{RootCauseService: "ad", RootCauseClass: "error"},
	))
	if coincident.SymptomEcho {
		t.Error("no echo is possible when cause and symptom are the same service")
	}
}

func TestFalseAlarmOnHealthyControl(t *testing.T) {
	invented := grade(confirmed(
		healthyScenario("c1"),
		Answer{RootCauseService: "cart", RootCauseClass: "error"},
	))
	if !invented.FalseAlarm || invented.ExactCorrect {
		t.Fatalf("naming a cause on a healthy system is a false alarm, got %+v", invented)
	}

	restrained := grade(confirmed(healthyScenario("c2"), Answer{Healthy: true}))
	if restrained.FalseAlarm || !restrained.ExactCorrect {
		t.Fatalf("correctly reporting health should score correct, got %+v", restrained)
	}
}

func TestMissedFault(t *testing.T) {
	s := grade(confirmed(
		faultScenario("s1", "L3", "recommendation", "resource", "recommendation"),
		Answer{Healthy: true},
	))
	if !s.MissedFault || s.ExactCorrect {
		t.Fatalf("reporting healthy during a live fault is a miss, got %+v", s)
	}
}

// TestUnconfirmedFaultDiscardsRatherThanFails is load bearing. If the fault
// never fired, the agent was shown a healthy system while being scored against
// a fault. Counting that as a failure charges the agent for the harness's
// problem and understates it.
func TestUnconfirmedFaultDiscardsRatherThanFails(t *testing.T) {
	ep := Episode{
		Scenario:       faultScenario("s1", "L2", "payment", "error", "checkout"),
		Answer:         Answer{RootCauseService: "payment", RootCauseClass: "error"},
		FaultConfirmed: false,
		ObservedDelta:  0.0001,
	}
	s := grade(ep)
	if s.Valid {
		t.Error("an episode whose fault never fired must not be scored")
	}
	if !strings.Contains(s.InvalidReason, "not confirmed") {
		t.Errorf("reason should say the fault was not confirmed, got %q", s.InvalidReason)
	}
	if s.ExactCorrect {
		t.Error("an unscored episode must not carry a verdict")
	}
}

func TestHealthyControlNeedsNoFaultConfirmation(t *testing.T) {
	ep := Episode{
		Scenario:       healthyScenario("c1"),
		Answer:         Answer{Healthy: true},
		FaultConfirmed: false,
	}
	if s := grade(ep); !s.Valid {
		t.Errorf("a control injects nothing, so it cannot require confirmation: %+v", s)
	}
}

func TestAgentErrorDiscardsEpisode(t *testing.T) {
	ep := Episode{
		Scenario:       faultScenario("s1", "L1", "ad", "error", "ad"),
		FaultConfirmed: true,
		AgentError:     "connection refused",
	}
	s := grade(ep)
	if s.Valid || !strings.Contains(s.InvalidReason, "agent error") {
		t.Fatalf("an agent that never answered cannot be scored, got %+v", s)
	}
}

func TestAggregateExcludesDiscardedFromRates(t *testing.T) {
	episodes := []Episode{
		confirmed(faultScenario("a", "L1", "ad", "error", "ad"),
			Answer{RootCauseService: "ad", RootCauseClass: "error", Steps: 4}),
		confirmed(faultScenario("b", "L2", "payment", "error", "checkout"),
			Answer{RootCauseService: "checkout", RootCauseClass: "error", Steps: 6}),
		{Scenario: faultScenario("c", "L2", "cart", "error", "cart"), FaultConfirmed: false},
	}
	agg := Summarise("test", episodes)

	if agg.Total != 3 || agg.Valid != 2 || agg.Invalid != 1 {
		t.Fatalf("counts wrong: total=%d valid=%d invalid=%d", agg.Total, agg.Valid, agg.Invalid)
	}
	// 1 of 2 scored episodes was exactly right.
	if math.Abs(agg.ExactAccuracy-0.5) > 1e-9 {
		t.Errorf("exact accuracy = %.3f, want 0.5 over scored episodes only", agg.ExactAccuracy)
	}
	if math.Abs(agg.MeanSteps-5) > 1e-9 {
		t.Errorf("mean steps = %.2f, want 5 (discarded episode excluded)", agg.MeanSteps)
	}
	if len(agg.ByDifficulty) != 2 {
		t.Errorf("expected L1 and L2 buckets, got %v", agg.ByDifficulty)
	}
}

func TestReportWarnsWhenTooManyEpisodesAreDiscarded(t *testing.T) {
	var episodes []Episode
	for i := 0; i < 10; i++ {
		s := faultScenario("s", "L2", "payment", "error", "checkout")
		if i < 5 {
			episodes = append(episodes, Episode{Scenario: s, FaultConfirmed: false})
		} else {
			episodes = append(episodes, confirmed(s,
				Answer{RootCauseService: "payment", RootCauseClass: "error"}))
		}
	}
	report := Summarise("test", episodes).Report()
	if !strings.Contains(report, "WARNING") {
		t.Error("half the episodes were discarded; that is a broken harness and " +
			"the report must say so rather than quietly shrinking the denominator")
	}
}

func TestReportOnEmptyResults(t *testing.T) {
	report := Summarise("test", nil).Report()
	if !strings.Contains(report, "0 episodes") {
		t.Errorf("unexpected report for no episodes:\n%s", report)
	}
}

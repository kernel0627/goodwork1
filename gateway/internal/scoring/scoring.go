// Package scoring turns an episode into numbers.
//
// Everything here is decided by comparing structured values. Nothing is judged
// by a model, and nothing depends on wording. A benchmark whose scores come from
// an LLM judge produces numbers nobody else can reproduce, which is the failure
// mode this package exists to avoid.
package scoring

import (
	"fmt"
	"sort"
	"strings"

	"github.com/kernel0627/medic/gateway/internal/scenario"
)

// Answer is what the agent concluded. Field names mirror the JSON contract in
// docs/agent-api.md.
type Answer struct {
	RootCauseService string  `json:"root_cause_service"`
	RootCauseClass   string  `json:"root_cause_class"`
	Confidence       float64 `json:"confidence"`
	// Healthy is the agent asserting nothing is wrong. Kept separate from an
	// empty root cause so "I found nothing" is distinguishable from "I did not
	// answer".
	Healthy bool `json:"healthy"`
	// Escalate is the agent handing off rather than concluding.
	Escalate bool `json:"escalate"`
	// Steps is how many tool calls it made.
	Steps int `json:"steps"`
	// ToolCalls is the sequence of tool names, for analysing strategy.
	ToolCalls []string `json:"tool_calls"`
	// Reasoning is recorded for Bad Case analysis and never scored.
	Reasoning string `json:"reasoning"`
	// InputTokens and OutputTokens fund the cost-per-episode metric, which any
	// real deployment has to answer for.
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// Episode pairs a scenario with what happened.
type Episode struct {
	Scenario scenario.Scenario
	Answer   Answer

	// FaultConfirmed records whether the injected fault was actually observable
	// during this episode. False means the episode is discarded, not failed.
	FaultConfirmed bool
	// ObservedDelta is the shift measured when confirming the fault.
	ObservedDelta float64
	// AgentError is set when the agent service failed to answer.
	AgentError string
	DurationMS int64
}

// Score is one episode's outcome.
type Score struct {
	ScenarioID string
	Difficulty string

	// Valid is false when the episode cannot be scored fairly: the fault never
	// fired, or the agent service errored. Invalid episodes are excluded from
	// rates rather than counted as failures -- charging the agent for the
	// harness's problems would understate it.
	Valid         bool
	InvalidReason string

	ServiceCorrect bool
	ClassCorrect   bool
	ExactCorrect   bool

	// SymptomEcho is the agent naming the service the alert fired on when the
	// real cause lies elsewhere.
	//
	// Tracked because it is the failure mode a plausible-looking agent falls
	// into: repeating the symptom back scores well on L1, where symptom and
	// cause coincide, and zero on L2 and L3. Without this metric a high L1 score
	// looks like competence. With it, the strategy is visible.
	SymptomEcho bool

	// FalseAlarm is naming a root cause on a healthy control -- inventing a
	// fault. MissedFault is the reverse.
	FalseAlarm  bool
	MissedFault bool

	Escalated  bool
	Steps      int
	Confidence float64
	CostTokens int
	DurationMS int64
}

// grade scores one episode.
func grade(ep Episode) Score {
	s := Score{
		ScenarioID: ep.Scenario.ID,
		Difficulty: ep.Scenario.Difficulty,
		Escalated:  ep.Answer.Escalate,
		Steps:      ep.Answer.Steps,
		Confidence: ep.Answer.Confidence,
		CostTokens: ep.Answer.InputTokens + ep.Answer.OutputTokens,
		DurationMS: ep.DurationMS,
	}

	switch {
	case ep.AgentError != "":
		s.InvalidReason = "agent error: " + ep.AgentError
		return s
	case !ep.Scenario.Truth.Healthy && !ep.FaultConfirmed:
		// The fault did not fire, so the agent was shown a healthy system while
		// being scored against a fault. Discarding is the only honest option.
		s.InvalidReason = fmt.Sprintf(
			"fault not confirmed (observed delta %.4g < %.4g); episode discarded",
			ep.ObservedDelta, ep.Scenario.MinDelta)
		return s
	}
	s.Valid = true

	truth := ep.Scenario.Truth
	if truth.Healthy {
		// On a control the whole question is whether the agent invents a fault.
		s.FalseAlarm = !ep.Answer.Healthy && ep.Answer.RootCauseService != ""
		s.ExactCorrect = !s.FalseAlarm
		s.ServiceCorrect = s.ExactCorrect
		s.ClassCorrect = s.ExactCorrect
		return s
	}

	if ep.Answer.Healthy {
		s.MissedFault = true
		return s
	}

	s.ServiceCorrect = eq(ep.Answer.RootCauseService, truth.RootCauseService)
	s.ClassCorrect = eq(ep.Answer.RootCauseClass, truth.RootCauseClass)
	s.ExactCorrect = s.ServiceCorrect && s.ClassCorrect

	if truth.SymptomService != "" && !eq(truth.SymptomService, truth.RootCauseService) {
		s.SymptomEcho = eq(ep.Answer.RootCauseService, truth.SymptomService)
	}
	return s
}

func eq(a, b string) bool {
	return strings.EqualFold(strings.TrimSpace(a), strings.TrimSpace(b))
}

// Aggregate is a run's results.
type Aggregate struct {
	Arm string

	Total   int
	Valid   int
	Invalid int
	// InvalidReasons counts why episodes were discarded. Reported because a run
	// with many discards is not a result -- it is a broken harness, and burying
	// that in a denominator would hide it.
	InvalidReasons map[string]int

	ServiceAccuracy float64
	ClassAccuracy   float64
	ExactAccuracy   float64

	SymptomEchoRate float64
	FalseAlarmRate  float64
	MissedFaultRate float64
	EscalationRate  float64

	MeanSteps      float64
	MeanTokens     float64
	MeanDurationMS float64

	ByDifficulty map[string]*Aggregate

	Scores []Score
}

// Summarise grades every episode and aggregates, overall and per difficulty.
func Summarise(arm string, episodes []Episode) *Aggregate {
	agg := &Aggregate{
		Arm:            arm,
		InvalidReasons: map[string]int{},
		ByDifficulty:   map[string]*Aggregate{},
	}
	for _, ep := range episodes {
		agg.add(grade(ep))
	}
	agg.finalise()
	return agg
}

func (a *Aggregate) add(s Score) {
	a.Total++
	a.Scores = append(a.Scores, s)
	if !s.Valid {
		a.Invalid++
		a.InvalidReasons[reasonKey(s.InvalidReason)]++
		return
	}
	a.Valid++

	bucket := a.ByDifficulty[s.Difficulty]
	if bucket == nil {
		bucket = &Aggregate{
			Arm:            a.Arm + "/" + s.Difficulty,
			InvalidReasons: map[string]int{},
		}
		a.ByDifficulty[s.Difficulty] = bucket
	}
	bucket.Total++
	bucket.Valid++
	bucket.Scores = append(bucket.Scores, s)
}

// reasonKey collapses a reason to its category so counts stay readable.
func reasonKey(reason string) string {
	if i := strings.Index(reason, "("); i > 0 {
		return strings.TrimSpace(reason[:i])
	}
	if i := strings.Index(reason, ":"); i > 0 {
		return strings.TrimSpace(reason[:i])
	}
	return reason
}

func (a *Aggregate) finalise() {
	compute(a)
	for _, bucket := range a.ByDifficulty {
		compute(bucket)
	}
}

func compute(a *Aggregate) {
	if a.Valid == 0 {
		return
	}
	var service, class, exact, echo, falseAlarm, missed, escalated int
	var steps, tokens, duration int64
	echoDenominator := 0

	for _, s := range a.Scores {
		if !s.Valid {
			continue
		}
		if s.ServiceCorrect {
			service++
		}
		if s.ClassCorrect {
			class++
		}
		if s.ExactCorrect {
			exact++
		}
		if s.FalseAlarm {
			falseAlarm++
		}
		if s.MissedFault {
			missed++
		}
		if s.Escalated {
			escalated++
		}
		steps += int64(s.Steps)
		tokens += int64(s.CostTokens)
		duration += s.DurationMS

		// Echo rate is only defined where symptom and cause differ; scenarios
		// where they coincide would dilute it toward zero and make the metric
		// look better than it is.
		if s.SymptomEcho {
			echo++
			echoDenominator++
		} else if !s.ExactCorrect && !s.FalseAlarm {
			echoDenominator++
		}
	}

	n := float64(a.Valid)
	a.ServiceAccuracy = float64(service) / n
	a.ClassAccuracy = float64(class) / n
	a.ExactAccuracy = float64(exact) / n
	a.FalseAlarmRate = float64(falseAlarm) / n
	a.MissedFaultRate = float64(missed) / n
	a.EscalationRate = float64(escalated) / n
	a.MeanSteps = float64(steps) / n
	a.MeanTokens = float64(tokens) / n
	a.MeanDurationMS = float64(duration) / n
	if echoDenominator > 0 {
		a.SymptomEchoRate = float64(echo) / float64(echoDenominator)
	}
}

// Report renders an aggregate as text.
func (a *Aggregate) Report() string {
	var b strings.Builder
	fmt.Fprintf(&b, "arm %s — %d episodes, %d scored, %d discarded\n",
		a.Arm, a.Total, a.Valid, a.Invalid)

	if a.Invalid > 0 {
		keys := make([]string, 0, len(a.InvalidReasons))
		for k := range a.InvalidReasons {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		b.WriteString("  discarded:\n")
		for _, k := range keys {
			fmt.Fprintf(&b, "    %-40s %d\n", k, a.InvalidReasons[k])
		}
		if float64(a.Invalid)/float64(a.Total) > 0.2 {
			b.WriteString("  WARNING: over a fifth of episodes were discarded. " +
				"Fix the harness before reading anything into these rates.\n")
		}
	}
	if a.Valid == 0 {
		b.WriteString("  nothing scored\n")
		return b.String()
	}

	fmt.Fprintf(&b, "\n  %-26s %6.1f%%\n", "root cause exact", a.ExactAccuracy*100)
	fmt.Fprintf(&b, "  %-26s %6.1f%%\n", "root cause service", a.ServiceAccuracy*100)
	fmt.Fprintf(&b, "  %-26s %6.1f%%\n", "root cause class", a.ClassAccuracy*100)
	fmt.Fprintf(&b, "  %-26s %6.1f%%   (named the alerting service instead of the cause)\n",
		"symptom echo", a.SymptomEchoRate*100)
	fmt.Fprintf(&b, "  %-26s %6.1f%%   (invented a fault on a healthy system)\n",
		"false alarm", a.FalseAlarmRate*100)
	fmt.Fprintf(&b, "  %-26s %6.1f%%   (reported healthy while a fault was live)\n",
		"missed fault", a.MissedFaultRate*100)
	fmt.Fprintf(&b, "  %-26s %6.1f%%\n", "escalated", a.EscalationRate*100)
	fmt.Fprintf(&b, "  %-26s %6.1f\n", "mean tool calls", a.MeanSteps)
	fmt.Fprintf(&b, "  %-26s %6.0f\n", "mean tokens", a.MeanTokens)
	fmt.Fprintf(&b, "  %-26s %6.1fs\n", "mean duration", a.MeanDurationMS/1000)

	if len(a.ByDifficulty) > 1 {
		keys := make([]string, 0, len(a.ByDifficulty))
		for k := range a.ByDifficulty {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		b.WriteString("\n  by difficulty (exact / echo / n)\n")
		for _, k := range keys {
			d := a.ByDifficulty[k]
			label := k
			if label == "" {
				label = "unclassified"
			}
			fmt.Fprintf(&b, "    %-14s %5.1f%%  %5.1f%%  %d\n",
				label, d.ExactAccuracy*100, d.SymptomEchoRate*100, d.Valid)
		}
	}
	return b.String()
}

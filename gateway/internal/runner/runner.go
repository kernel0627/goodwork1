// Package runner executes scenarios against the agent.
//
// One episode:
//
//	revert everything      -> a fault left over from the last episode would be
//	                          attributed to this one
//	wait recover           -> and its tail would still sit inside the rate window
//	measure baselines      -> both the confirmation signal and the symptom probes
//	inject                 -> the scenario's faults
//	wait settle            -> until the symptom is actually in the metrics
//	confirm the fault      -> or discard the episode; see below
//	compose the alert      -> from measured symptoms, never from the fault list
//	ask the agent          -> one HTTP call; the agent does its own tool calls
//	revert                 -> always, including on failure
//
// # Discarding rather than failing
//
// If the injected fault cannot be measured, the agent was shown a healthy system
// while being scored against a fault. Counting that as a wrong answer charges the
// agent for the harness's problem. Such episodes are discarded, the agent is not
// even asked, and the discard is reported.
//
// # The alert cannot leak the answer
//
// Alert text is composed here from metrics measured on the *symptom* service,
// which for most scenarios is not the root cause. It never mentions the injected
// flag. That is the only channel from runner to agent, so there is nothing else
// through which the answer could reach it.
package runner

import (
	"context"
	"fmt"
	"math"
	"time"

	"github.com/kernel0627/medic/gateway/internal/agentclient"
	"github.com/kernel0627/medic/gateway/internal/inject"
	"github.com/kernel0627/medic/gateway/internal/promq"
	"github.com/kernel0627/medic/gateway/internal/scenario"
	"github.com/kernel0627/medic/gateway/internal/scoring"
	"github.com/kernel0627/medic/gateway/internal/signals"
)

// symptomProbe is one alerting rule.
//
// These mirror what a real alerting system watches, and the alert reports
// whichever breached hardest. Composing the alert from a probe rather than from
// the scenario is what keeps the root cause out of it.
type symptomProbe struct {
	signal string
	// subsKey is the placeholder the symptom service fills: container metrics
	// key on container_name, request metrics on service_name.
	subsKey string
	label   string
}

var symptomProbes = []symptomProbe{
	{signal: "error_ratio", subsKey: "service", label: "error ratio"},
	{signal: "latency_p99", subsKey: "service", label: "p99 latency"},
	{signal: "memory", subsKey: "container", label: "memory usage"},
	{signal: "cpu", subsKey: "container", label: "CPU usage"},
	{signal: "consumer_lag", subsKey: "", label: "consumer lag"},
}

// Runner holds everything an episode needs.
type Runner struct {
	Inj     *inject.Injector
	Prom    *promq.Client
	Signals *signals.Catalog
	Agent   *agentclient.Client

	// Arm labels the results. Taken from the agent's own /healthz rather than a
	// flag, so a run cannot be mislabelled as an arm it is not.
	Arm string

	// OnProgress reports each episode as it completes, so a long run is
	// observable instead of silent.
	OnProgress func(index, total int, ep scoring.Episode, err error)
}

// Run executes every scenario in order.
//
// Sequentially on purpose: two scenarios at once would inject two faults into one
// system, and no signal could then be attributed to either.
func (r *Runner) Run(ctx context.Context, scenarios []scenario.Scenario) []scoring.Episode {
	episodes := make([]scoring.Episode, 0, len(scenarios))
	for i, s := range scenarios {
		ep, err := r.runOne(ctx, s)
		episodes = append(episodes, ep)
		if r.OnProgress != nil {
			r.OnProgress(i+1, len(scenarios), ep, err)
		}
		if ctx.Err() != nil {
			break
		}
	}
	// Never leave the SUT dirty, whatever happened.
	if err := r.Inj.Reset(); err != nil && ctx.Err() == nil {
		fmt.Printf("warning: could not reset flags after run: %v\n", err)
	}
	return episodes
}

func (r *Runner) runOne(ctx context.Context, s scenario.Scenario) (scoring.Episode, error) {
	started := time.Now()
	ep := scoring.Episode{Scenario: s}

	fail := func(err error) (scoring.Episode, error) {
		ep.AgentError = err.Error()
		ep.DurationMS = time.Since(started).Milliseconds()
		return ep, err
	}

	if err := r.Inj.Reset(); err != nil {
		return fail(fmt.Errorf("reset before episode: %w", err))
	}
	if err := sleepCtx(ctx, s.Recover()); err != nil {
		return fail(err)
	}

	// Baselines, captured while the system is clean.
	var confirmBaseline float64
	if !s.Truth.Healthy {
		q, err := r.Signals.Query(s.Signal, s.Subs)
		if err != nil {
			return fail(fmt.Errorf("scenario %s signal: %w", s.ID, err))
		}
		v, err := r.Prom.Instant(ctx, q)
		if err != nil {
			return fail(fmt.Errorf("baseline for confirmation signal: %w", err))
		}
		confirmBaseline = zeroIfNoData(v)
	}
	symptomBaseline := r.probeSymptoms(ctx, s.Truth.SymptomService)

	// Inject.
	r.Inj.ClearAll()
	for _, f := range s.Faults {
		if err := r.Inj.Set(f.Flag, f.Variant); err != nil {
			return fail(fmt.Errorf("stage %s: %w", f, err))
		}
	}
	if err := r.Inj.Commit(); err != nil {
		return fail(fmt.Errorf("commit faults: %w", err))
	}
	defer func() {
		if err := r.Inj.Reset(); err != nil {
			fmt.Printf("warning: could not revert after %s: %v\n", s.ID, err)
		}
	}()

	if err := sleepCtx(ctx, s.Settle()); err != nil {
		return fail(err)
	}

	// Confirm the fault actually fired.
	if !s.Truth.Healthy {
		q, _ := r.Signals.Query(s.Signal, s.Subs)
		after, err := r.Prom.Instant(ctx, q)
		if err != nil {
			return fail(fmt.Errorf("confirmation measurement: %w", err))
		}
		ep.ObservedDelta = zeroIfNoData(after) - confirmBaseline
		ep.FaultConfirmed = ep.ObservedDelta >= s.MinDelta
		if !ep.FaultConfirmed {
			// Not asking the agent is deliberate: the episode cannot be scored,
			// and asking would spend tokens producing an answer that gets thrown
			// away.
			ep.DurationMS = time.Since(started).Milliseconds()
			return ep, nil
		}
	} else {
		ep.FaultConfirmed = true
	}

	alert := r.composeAlert(ctx, s, symptomBaseline)

	answer, err := r.Agent.Diagnose(ctx, agentclient.Request{
		EpisodeID: s.ID,
		Alert:     alert,
		Budget: agentclient.Budget{
			MaxSteps:        s.MaxSteps,
			DeadlineSeconds: int(s.Settle().Seconds()) + 300,
		},
	})
	if err != nil {
		return fail(err)
	}

	ep.Answer = scoring.Answer{
		RootCauseService: answer.RootCauseService,
		RootCauseClass:   answer.RootCauseClass,
		Confidence:       answer.Confidence,
		Healthy:          answer.Healthy,
		Escalate:         answer.Escalate,
		Steps:            answer.Steps,
		ToolCalls:        answer.ToolCalls,
		Reasoning:        answer.Reasoning,
		InputTokens:      answer.InputTokens,
		OutputTokens:     answer.OutputTokens,
	}
	ep.DurationMS = time.Since(started).Milliseconds()
	return ep, nil
}

// probeSymptoms measures every alerting rule on one service.
func (r *Runner) probeSymptoms(ctx context.Context, service string) map[string]float64 {
	out := map[string]float64{}
	if service == "" {
		return out
	}
	for _, probe := range symptomProbes {
		subs := map[string]string{}
		if probe.subsKey != "" {
			subs[probe.subsKey] = service
		}
		q, err := r.Signals.Query(probe.signal, subs)
		if err != nil {
			continue
		}
		v, err := r.Prom.Instant(ctx, q)
		if err != nil {
			continue
		}
		out[probe.signal] = zeroIfNoData(v)
	}
	return out
}

// composeAlert builds the alert from whichever symptom probe moved most.
//
// Relative change, not absolute: the probes are in different units -- a ratio,
// milliseconds, cores, a percentage, a record count -- so comparing their raw
// deltas would always pick whichever happens to have the largest scale.
func (r *Runner) composeAlert(
	ctx context.Context, s scenario.Scenario, baseline map[string]float64,
) agentclient.Alert {
	service := s.Truth.SymptomService
	now := r.probeSymptoms(ctx, service)

	bestProbe := symptomProbes[0]
	bestScore := -1.0
	var bestNow, bestBefore float64

	for _, probe := range symptomProbes {
		after, ok := now[probe.signal]
		if !ok {
			continue
		}
		before := baseline[probe.signal]
		score := math.Abs(after-before) / math.Max(math.Abs(before), 1e-6)
		if math.Abs(after-before) < 1e-9 {
			score = 0
		}
		if score > bestScore {
			bestScore, bestProbe, bestNow, bestBefore = score, probe, after, before
		}
	}

	sig, _ := r.Signals.Get(bestProbe.signal)
	summary := fmt.Sprintf(
		"%s on %s is %s (baseline %s), sustained for %s",
		bestProbe.label, service,
		formatValue(bestNow, sig.Unit), formatValue(bestBefore, sig.Unit),
		s.Settle().Round(time.Second),
	)
	if s.Truth.Healthy {
		// Controls still get an alert -- a real oncall is sometimes paged for
		// nothing, and whether the agent invents a fault in that situation is
		// exactly what a control measures.
		summary = fmt.Sprintf(
			"%s on %s is %s; alert may be spurious",
			bestProbe.label, service, formatValue(bestNow, sig.Unit))
	}

	return agentclient.Alert{
		Service: service,
		Summary: summary,
		FiredAt: time.Now().UTC().Format(time.RFC3339),
		Observed: agentclient.Observed{
			Signal:   bestProbe.signal,
			Value:    round4(bestNow),
			Baseline: round4(bestBefore),
			Unit:     sig.Unit,
		},
	}
}

func formatValue(v float64, unit string) string {
	switch unit {
	case "ratio":
		return fmt.Sprintf("%.1f%%", v*100)
	case "percent":
		return fmt.Sprintf("%.1f%%", v)
	case "milliseconds":
		return fmt.Sprintf("%.0fms", v)
	case "cores":
		return fmt.Sprintf("%.2f cores", v)
	case "records":
		return fmt.Sprintf("%.0f records", v)
	default:
		return fmt.Sprintf("%.4g", v)
	}
}

func round4(v float64) float64 {
	return math.Round(v*10000) / 10000
}

func zeroIfNoData(v float64) float64 {
	if math.IsNaN(v) {
		return 0
	}
	return v
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

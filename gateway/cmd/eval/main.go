// Command eval runs the scenario library against a live agent and scores it.
//
//	make agent-bg BRAIN=random        # start an arm
//	go run ./cmd/eval                 # run every scenario
//	go run ./cmd/eval -only healthy-control-01
//
// Results land in results/eval-<arm>.{json,md}.
//
// The arm label comes from the agent's own /healthz, not from a flag. A run
// mislabelled as an arm it is not would make every comparison built on it
// worthless, and a flag is exactly the kind of thing that gets left stale.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/kernel0627/medic/gateway/internal/agentclient"
	"github.com/kernel0627/medic/gateway/internal/inject"
	"github.com/kernel0627/medic/gateway/internal/promq"
	"github.com/kernel0627/medic/gateway/internal/runner"
	"github.com/kernel0627/medic/gateway/internal/scenario"
	"github.com/kernel0627/medic/gateway/internal/scoring"
	"github.com/kernel0627/medic/gateway/internal/signals"
)

const defaultFlagPath = "sut/opentelemetry-demo/src/flagd/demo.flagd.json"

func main() {
	var (
		promURL  = flag.String("prom", env("PROM_URL", "http://localhost:9090"), "Prometheus base URL")
		agentURL = flag.String("agent", env("AGENT_URL", "http://127.0.0.1:7802"), "agent service base URL")
		libPath  = flag.String("library", "scenarios/library.yaml", "scenario library")
		outDir   = flag.String("out", "results", "results directory")
		only     = flag.String("only", "", "comma-separated scenario ids to run")
		label    = flag.String("label", "", "optional suffix on the arm label, e.g. an ablation name")
	)
	flag.Parse()

	root, err := repoRoot()
	if err != nil {
		die(err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	prom := promq.New(*promURL, 20*time.Second)
	if err := prom.Ready(ctx); err != nil {
		die(fmt.Errorf("prometheus at %s not answering: %w\nrun `make sut-up`", *promURL, err))
	}

	sigs, err := signals.Load(filepath.Join(root, signals.DefaultPath))
	if err != nil {
		die(err)
	}
	inj, err := inject.New(filepath.Join(root, defaultFlagPath))
	if err != nil {
		die(err)
	}
	lib, err := scenario.Load(filepath.Join(root, *libPath))
	if err != nil {
		die(err)
	}

	// An episode's HTTP timeout has to exceed the agent's own deadline, or a slow
	// but successful diagnosis would be discarded as an agent error.
	agent := agentclient.New(*agentURL, 10*time.Minute)
	health, err := agent.Health(ctx)
	if err != nil {
		die(fmt.Errorf("%w\nstart one with `make agent-bg BRAIN=random`", err))
	}

	arm := health.Brain
	if *label != "" {
		arm += "/" + *label
	}
	if len(health.Withheld) > 0 {
		arm += fmt.Sprintf("[-%s]", strings.Join(health.Withheld, ","))
	}

	selected := lib.Filter(splitList(*only))
	if len(selected) == 0 {
		die(fmt.Errorf("no scenarios selected (library has %d)", len(lib.Scenarios)))
	}

	fmt.Printf("arm %s — %d tools", arm, health.Tools)
	if len(health.Withheld) > 0 {
		fmt.Printf(" (withheld: %s)", strings.Join(health.Withheld, ", "))
	}
	fmt.Printf("\n%s\n", lib.Summary())
	if len(selected) < len(lib.Scenarios) {
		fmt.Printf("running %d selected scenario(s)\n", len(selected))
	}
	var wall time.Duration
	for _, s := range selected {
		wall += s.Settle() + s.Recover()
	}
	fmt.Printf("estimated wall clock: ~%s plus agent time\n\n", wall.Round(time.Minute))

	r := &runner.Runner{
		Inj: inj, Prom: prom, Signals: sigs, Agent: agent, Arm: arm,
		OnProgress: func(i, n int, ep scoring.Episode, err error) {
			status := describe(ep, err)
			fmt.Printf("[%2d/%d] %-32s %s\n", i, n, ep.Scenario.ID, status)
		},
	}

	episodes := r.Run(ctx, selected)
	agg := scoring.Summarise(arm, episodes)

	fmt.Printf("\n%s\n", agg.Report())

	if err := writeResults(filepath.Join(root, *outDir), arm, agg, episodes); err != nil {
		die(err)
	}
	if ctx.Err() != nil {
		fmt.Fprintln(os.Stderr, "interrupted; results above are partial")
		os.Exit(130)
	}
}

// describe renders one episode's outcome for the progress line.
func describe(ep scoring.Episode, err error) string {
	if err != nil {
		return "DISCARDED  " + truncate(err.Error(), 70)
	}
	if !ep.Scenario.Truth.Healthy && !ep.FaultConfirmed {
		return fmt.Sprintf("DISCARDED  fault did not fire (delta %.4g < %.4g)",
			ep.ObservedDelta, ep.Scenario.MinDelta)
	}
	answer := ep.Answer
	switch {
	case answer.Healthy:
		if ep.Scenario.Truth.Healthy {
			return fmt.Sprintf("ok         healthy (correct)          steps=%d", answer.Steps)
		}
		return fmt.Sprintf("MISS       said healthy                steps=%d", answer.Steps)
	case answer.Escalate && answer.RootCauseService == "":
		return fmt.Sprintf("escalated  no answer                   steps=%d", answer.Steps)
	default:
		verdict := "WRONG"
		if eqFold(answer.RootCauseService, ep.Scenario.Truth.RootCauseService) &&
			eqFold(answer.RootCauseClass, ep.Scenario.Truth.RootCauseClass) {
			verdict = "ok   "
		}
		return fmt.Sprintf("%s      %s/%s   steps=%d",
			verdict, answer.RootCauseService, answer.RootCauseClass, answer.Steps)
	}
}

func writeResults(dir, arm string, agg *scoring.Aggregate, episodes []scoring.Episode) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	slug := strings.NewReplacer("/", "-", "[", "", "]", "", ",", "-").Replace(arm)

	type episodeRecord struct {
		ScenarioID     string   `json:"scenario_id"`
		Difficulty     string   `json:"difficulty"`
		TruthService   string   `json:"truth_service"`
		TruthClass     string   `json:"truth_class"`
		SymptomService string   `json:"symptom_service"`
		Healthy        bool     `json:"healthy_control"`
		FaultConfirmed bool     `json:"fault_confirmed"`
		ObservedDelta  float64  `json:"observed_delta"`
		AnswerService  string   `json:"answer_service"`
		AnswerClass    string   `json:"answer_class"`
		AnswerHealthy  bool     `json:"answer_healthy"`
		Escalated      bool     `json:"escalated"`
		Confidence     float64  `json:"confidence"`
		Steps          int      `json:"steps"`
		ToolCalls      []string `json:"tool_calls"`
		Reasoning      string   `json:"reasoning"`
		Tokens         int      `json:"tokens"`
		DurationMS     int64    `json:"duration_ms"`
		AgentError     string   `json:"agent_error,omitempty"`
	}

	records := make([]episodeRecord, 0, len(episodes))
	for _, ep := range episodes {
		records = append(records, episodeRecord{
			ScenarioID: ep.Scenario.ID, Difficulty: ep.Scenario.Difficulty,
			TruthService:   ep.Scenario.Truth.RootCauseService,
			TruthClass:     ep.Scenario.Truth.RootCauseClass,
			SymptomService: ep.Scenario.Truth.SymptomService,
			Healthy:        ep.Scenario.Truth.Healthy,
			FaultConfirmed: ep.FaultConfirmed, ObservedDelta: ep.ObservedDelta,
			AnswerService: ep.Answer.RootCauseService,
			AnswerClass:   ep.Answer.RootCauseClass,
			AnswerHealthy: ep.Answer.Healthy, Escalated: ep.Answer.Escalate,
			Confidence: ep.Answer.Confidence, Steps: ep.Answer.Steps,
			ToolCalls: ep.Answer.ToolCalls, Reasoning: ep.Answer.Reasoning,
			Tokens:     ep.Answer.InputTokens + ep.Answer.OutputTokens,
			DurationMS: ep.DurationMS, AgentError: ep.AgentError,
		})
	}

	payload := map[string]any{
		"arm":              arm,
		"total":            agg.Total,
		"scored":           agg.Valid,
		"discarded":        agg.Invalid,
		"discard_reasons":  agg.InvalidReasons,
		"exact_accuracy":   agg.ExactAccuracy,
		"service_accuracy": agg.ServiceAccuracy,
		"class_accuracy":   agg.ClassAccuracy,
		"symptom_echo":     agg.SymptomEchoRate,
		"false_alarm":      agg.FalseAlarmRate,
		"missed_fault":     agg.MissedFaultRate,
		"escalation":       agg.EscalationRate,
		"mean_steps":       agg.MeanSteps,
		"mean_tokens":      agg.MeanTokens,
		"episodes":         records,
	}
	buf, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return err
	}
	jsonPath := filepath.Join(dir, "eval-"+slug+".json")
	if err := os.WriteFile(jsonPath, append(buf, '\n'), 0o644); err != nil {
		return err
	}

	mdPath := filepath.Join(dir, "eval-"+slug+".md")
	md := fmt.Sprintf("# 评测结果 — arm `%s`\n\n```\n%s```\n", arm, agg.Report())
	if err := os.WriteFile(mdPath, []byte(md), 0o644); err != nil {
		return err
	}
	fmt.Printf("wrote %s\n      %s\n", jsonPath, mdPath)
	return nil
}

func eqFold(a, b string) bool {
	return strings.EqualFold(strings.TrimSpace(a), strings.TrimSpace(b))
}

func splitList(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	return strings.Split(s, ",")
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func repoRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "queries", "signals.yaml")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("could not find repository root from the working directory")
		}
		dir = parent
	}
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func die(err error) {
	fmt.Fprintln(os.Stderr, "eval:", err)
	os.Exit(1)
}

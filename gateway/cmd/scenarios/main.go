// Command scenarios generates the scenario library from characterisation output.
//
//	go run ./cmd/scenarios                    # read results/characterization.json
//	go run ./cmd/scenarios -controls 3        # more healthy controls
//
// Generated rather than hand-written so the rule is mechanical: only faults that
// passed both tiers of characterisation can become scenarios. A fault added by
// hand without verification produces a scenario that looks injected but is not,
// and that scenario scores the agent against a healthy system while nothing
// anywhere reports a failure.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/kernel0627/medic/gateway/internal/inject"
	"github.com/kernel0627/medic/gateway/internal/scenario"
)

type characterization struct {
	SettleSeconds  float64 `json:"settle_seconds"`
	RecoverSeconds float64 `json:"recover_seconds"`
	Faults         []struct {
		Flag                 string  `json:"flag"`
		Variant              string  `json:"variant"`
		Class                string  `json:"class"`
		RootCause            string  `json:"root_cause"`
		SymptomAt            string  `json:"symptom_at"`
		Difficulty           string  `json:"difficulty"`
		Signal               string  `json:"signal"`
		Delta                float64 `json:"delta"`
		MinDelta             float64 `json:"min_delta"`
		Valid                bool    `json:"valid"`
		Status               string  `json:"status"`
		SyntheticLoadReaches bool    `json:"synthetic_load_reaches"`
	} `json:"faults"`
}

func main() {
	var (
		in       = flag.String("in", "results/characterization.json", "characterisation output")
		out      = flag.String("out", "scenarios/library.yaml", "scenario library to write")
		controls = flag.Int("controls", 2, "healthy control scenarios to include")
		maxSteps = flag.Int("max-steps", 20, "tool-call budget per episode")
	)
	flag.Parse()

	root, err := repoRoot()
	if err != nil {
		die(err)
	}
	inPath := abs(root, *in)
	outPath := abs(root, *out)

	raw, err := os.ReadFile(inPath)
	if err != nil {
		die(fmt.Errorf("read %s: %w\nrun `make characterize` first", inPath, err))
	}
	var chars characterization
	if err := json.Unmarshal(raw, &chars); err != nil {
		die(fmt.Errorf("parse %s: %w", inPath, err))
	}

	// Subs cannot be recovered from the JSON, so they come from the catalog,
	// keyed by flag. The catalog is the definition; characterisation is evidence
	// about it.
	subsByFlag := map[string]map[string]string{}
	for _, spec := range inject.Catalog() {
		subsByFlag[spec.Flag] = spec.Subs
	}

	settle := int(chars.SettleSeconds)
	if settle <= 0 {
		settle = 150
	}
	recoverFor := int(chars.RecoverSeconds)
	if recoverFor <= 0 {
		recoverFor = 90
	}

	lib := &scenario.Library{
		GeneratedFrom: fmt.Sprintf("%s (only faults passing both verification tiers)", *in),
	}
	var skipped []string

	for _, f := range chars.Faults {
		if !f.Valid {
			skipped = append(skipped, fmt.Sprintf("%s (%s)", f.Flag, shortStatus(f.Status)))
			continue
		}
		lib.Scenarios = append(lib.Scenarios, scenario.Scenario{
			ID:          fmt.Sprintf("%s-%s", f.Flag, f.Difficulty),
			Description: describe(f.Flag, f.Variant, f.RootCause, f.SymptomAt),
			Difficulty:  f.Difficulty,
			Faults:      []scenario.Fault{{Flag: f.Flag, Variant: f.Variant}},
			Truth: scenario.GroundTruth{
				RootCauseService: f.RootCause,
				RootCauseClass:   f.Class,
				SymptomService:   f.SymptomAt,
			},
			SettleSeconds:  settle,
			RecoverSeconds: recoverFor,
			MaxSteps:       *maxSteps,
			Signal:         f.Signal,
			Subs:           subsByFlag[f.Flag],
			ExpectedDelta:  f.Delta,
			MinDelta:       f.MinDelta,
		})
	}

	// Controls carry no fault. They are the only way to measure whether the agent
	// invents a cause when there is none, which is the failure mode that makes an
	// agent unusable in production: one that always finds something will
	// eventually act on something.
	for i := 1; i <= *controls; i++ {
		lib.Scenarios = append(lib.Scenarios, scenario.Scenario{
			ID:          fmt.Sprintf("healthy-control-%02d", i),
			Description: "No fault injected. Correct answer: the system is healthy.",
			Difficulty:  "control",
			Truth: scenario.GroundTruth{
				Healthy:        true,
				SymptomService: rotateService(i),
			},
			SettleSeconds:  30, // nothing to settle; just enough for a stable read
			RecoverSeconds: recoverFor,
			MaxSteps:       *maxSteps,
			Notes:          "control: alert is spurious by construction",
		})
	}

	sort.SliceStable(lib.Scenarios, func(i, j int) bool {
		return lib.Scenarios[i].ID < lib.Scenarios[j].ID
	})

	if err := lib.Validate(); err != nil {
		die(err)
	}
	if err := os.MkdirAll(filepath.Dir(outPath), 0o755); err != nil {
		die(err)
	}
	if err := lib.Save(outPath); err != nil {
		die(err)
	}

	fmt.Printf("wrote %s\n%s\n", outPath, lib.Summary())
	fmt.Printf("estimated wall clock per arm: ~%s plus agent time\n",
		lib.EstimatedDuration().Round(time.Minute))

	if len(skipped) > 0 {
		fmt.Printf("\nexcluded %d unverified fault(s):\n", len(skipped))
		for _, s := range skipped {
			fmt.Printf("  %s\n", s)
		}
		fmt.Println("\nThese are excluded by design. A scenario built on an unverified")
		fmt.Println("fault would score the agent against a healthy system, and nothing")
		fmt.Println("in the run would report a failure.")
	}
}

// describe writes the human-facing line. It names the root cause on purpose --
// this text is for whoever reads the library, and it is never sent to the agent.
// The agent only ever receives the generated alert.
func describe(flag, variant, cause, symptom string) string {
	if cause == symptom {
		return fmt.Sprintf("%s=%s. Root cause and symptom are both %s.", flag, variant, cause)
	}
	return fmt.Sprintf("%s=%s. Root cause is %s; the symptom surfaces on %s.",
		flag, variant, cause, symptom)
}

// rotateService varies which service a control's spurious alert points at, so a
// control cannot be recognised by always naming the same one.
func rotateService(i int) string {
	candidates := []string{"frontend", "checkout", "cart", "product-catalog"}
	return candidates[(i-1)%len(candidates)]
}

func shortStatus(status string) string {
	if i := strings.Index(status, ":"); i > 0 {
		return status[:i]
	}
	return status
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

func abs(root, path string) string {
	if filepath.IsAbs(path) {
		return path
	}
	return filepath.Join(root, path)
}

func die(err error) {
	fmt.Fprintln(os.Stderr, "scenarios:", err)
	os.Exit(1)
}

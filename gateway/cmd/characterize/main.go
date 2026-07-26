// Command characterize measures, for every injectable fault, whether it
// actually fires and what it looks like in metrics.
//
//	go run ./cmd/characterize                       # all faults
//	go run ./cmd/characterize -only cartFailure     # one, for iterating
//	go run ./cmd/characterize -settle 60s -recover 30s   # quick and unreliable
//
// Output goes to results/characterization.{json,md}.
//
// Why this exists: a fault that fails to take effect yields a scenario that
// looks injected but is not, and that scenario then scores the agent against a
// healthy system. Nothing anywhere fails visibly; the numbers are simply wrong.
// Only faults that pass both tiers here are allowed into the scenario library.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/kernel0627/medic/gateway/internal/dockerlog"
	"github.com/kernel0627/medic/gateway/internal/inject"
	"github.com/kernel0627/medic/gateway/internal/promq"
	"github.com/kernel0627/medic/gateway/internal/signals"
)

const defaultFlagPath = "sut/opentelemetry-demo/src/flagd/demo.flagd.json"

func main() {
	var (
		promURL = flag.String("prom", env("PROM_URL", "http://localhost:9090"), "Prometheus base URL")
		flagFn  = flag.String("file", env("MEDIC_FLAGD_FILE", ""), "path to demo.flagd.json")
		outDir  = flag.String("out", "results", "directory for JSON and Markdown output")
		settle  = flag.Duration("settle", inject.DefaultSettle, "wait after injecting before measuring")
		recover = flag.Duration("recover", inject.DefaultRecover, "wait after reverting before the next fault")
		only    = flag.String("only", "", "comma-separated flag names to characterise")
	)
	flag.Parse()

	root, flagPath, err := locate(*flagFn)
	if err != nil {
		die(err)
	}

	prom := promq.New(*promURL, 15*time.Second)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := prom.Ready(ctx); err != nil {
		die(fmt.Errorf("prometheus at %s not answering: %w\nrun `make sut-up` first", *promURL, err))
	}
	// Tier-1 verification reads flagd's container log, so the docker CLI has to
	// be reachable. Checked here rather than discovered 50 minutes into a run.
	if err := dockerlog.Available(ctx); err != nil {
		die(err)
	}

	sigs, err := signals.Load(filepath.Join(root, signals.DefaultPath))
	if err != nil {
		die(err)
	}

	inj, err := inject.New(flagPath)
	if err != nil {
		die(err)
	}

	specs := inject.Catalog()
	if *only != "" {
		specs = filterSpecs(specs, strings.Split(*only, ","))
		if len(specs) == 0 {
			die(fmt.Errorf("-only %q matched no fault in the catalog", *only))
		}
	}

	perFault := *settle + *recover
	fmt.Printf("characterising %d fault(s), settle %s + recover %s each\n",
		len(specs), *settle, *recover)
	fmt.Printf("estimated wall clock: ~%s\n\n", (time.Duration(len(specs)) * perFault).Round(time.Minute))

	verifier := &inject.Verifier{
		Inj: inj, Prom: prom, Signals: sigs,
		Settle: *settle, Recover: *recover,
	}

	verdicts := verifier.VerifyAll(ctx, specs, func(i, n int, v inject.Verdict) {
		mark := "ok  "
		if !v.Valid() {
			mark = "FAIL"
		}
		fmt.Printf("[%2d/%d] %s %-28s %-6s before=%-10.4g after=%-10.4g delta=%-+10.4g %s\n",
			i, n, mark, v.Spec.Flag, v.Spec.Class, v.Before, v.After, v.Delta, v.Status())
	})

	// Leave the SUT clean no matter how the run ended.
	if err := inj.Reset(); err != nil {
		fmt.Fprintf(os.Stderr, "\nwarning: could not reset flags: %v\n", err)
	}

	if err := writeResults(filepath.Join(root, *outDir), verdicts, *settle, *recover); err != nil {
		die(err)
	}
	summarise(verdicts)

	if ctx.Err() != nil {
		fmt.Fprintln(os.Stderr, "\ninterrupted; results above are partial")
		os.Exit(130)
	}
	for _, v := range verdicts {
		// Faults excluded by design are not failures; everything else is.
		if !v.Valid() && v.Spec.SyntheticLoadReaches {
			// A non-zero exit makes an incomplete catalog impossible to
			// overlook when this runs from a script.
			os.Exit(1)
		}
	}
}

func summarise(verdicts []inject.Verdict) {
	var valid, inert, dead, suspect, errored, unreachable []string
	for _, v := range verdicts {
		switch {
		case v.Err != nil:
			errored = append(errored, v.Spec.Flag)
		case !v.Spec.SyntheticLoadReaches:
			unreachable = append(unreachable, v.Spec.Flag)
		case v.Valid():
			valid = append(valid, v.Spec.Flag)
		case !v.Tier1Pass && !v.Tier2Pass:
			dead = append(dead, v.Spec.Flag)
		case !v.Tier1Pass:
			suspect = append(suspect, v.Spec.Flag)
		default:
			inert = append(inert, v.Spec.Flag)
		}
	}

	fmt.Printf("\n%d/%d faults usable\n", len(valid), len(verdicts))
	report := func(label string, names []string) {
		if len(names) > 0 {
			fmt.Printf("  %-9s %s\n", label, strings.Join(names, " "))
		}
	}
	report("usable", valid)
	report("unreach", unreachable)
	report("inert", inert)
	report("suspect", suspect)
	report("dead", dead)
	report("errored", errored)

	if len(valid) < len(verdicts) {
		fmt.Println("\nOnly faults marked usable may enter the scenario library.")
		fmt.Println("  unreach  no synthetic traffic drives the code path; excluded by design")
		fmt.Println("  inert    flagd took the file but nothing moved -- wrong signal, or the")
		fmt.Println("           fault's ceiling is below the threshold in queries/signals.yaml")
		fmt.Println("  suspect  something moved without flagd reloading; probably noise")
		fmt.Println("Fix the signal or drop the fault. Do not build scenarios on unverified faults.")
	}
}

type record struct {
	Flag                 string   `json:"flag"`
	Variant              string   `json:"variant"`
	Class                string   `json:"class"`
	RootCause            string   `json:"root_cause"`
	SymptomAt            string   `json:"symptom_at"`
	Difficulty           string   `json:"difficulty"`
	Site                 string   `json:"source_site"`
	SyntheticLoadReaches bool     `json:"synthetic_load_reaches"`
	ReloadObserved       bool     `json:"reload_observed"`
	EvalRate             float64  `json:"eval_rate"`
	EvaluatingServices   []string `json:"evaluating_services"`
	Tier1Pass            bool     `json:"tier1_pass"`
	Signal               string   `json:"signal"`
	Unit                 string   `json:"unit"`
	Query                string   `json:"query"`
	Before               float64  `json:"before"`
	After                float64  `json:"after"`
	Delta                float64  `json:"delta"`
	MinDelta             float64  `json:"min_delta"`
	Tier2Pass            bool     `json:"tier2_pass"`
	Valid                bool     `json:"valid"`
	Status               string   `json:"status"`
	Note                 string   `json:"note,omitempty"`
	ElapsedSeconds       float64  `json:"elapsed_seconds"`
	MeasuredAt           string   `json:"measured_at"`
}

// mergeWithExisting overlays fresh records onto whatever was measured before,
// keyed by flag, and returns the union sorted by flag.
func mergeWithExisting(path string, fresh []record) []record {
	byFlag := map[string]record{}

	if raw, err := os.ReadFile(path); err == nil {
		var previous struct {
			Faults []record `json:"faults"`
		}
		if json.Unmarshal(raw, &previous) == nil {
			for _, r := range previous.Faults {
				byFlag[r.Flag] = r
			}
		}
	}
	for _, r := range fresh {
		byFlag[r.Flag] = r
	}

	out := make([]record, 0, len(byFlag))
	for _, r := range byFlag {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Flag < out[j].Flag })
	return out
}

// writeResults merges into any existing output rather than replacing it.
//
// Characterisation takes the better part of an hour for all thirteen faults, and
// fixing one fault's signal should not mean re-measuring the other twelve. A
// `-only` run therefore updates just the faults it measured and leaves the rest
// as they were.
//
// The risk of merging is a stale entry passing itself off as current, so each
// record carries the time it was measured.
func writeResults(dir string, verdicts []inject.Verdict, settle, recover time.Duration) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}

	measuredAt := time.Now().UTC().Format(time.RFC3339)
	recs := make([]record, 0, len(verdicts))
	for _, v := range verdicts {
		svcs := v.EvaluatingServices
		sort.Strings(svcs)
		recs = append(recs, record{
			Flag: v.Spec.Flag, Variant: v.Spec.Variant,
			Class: string(v.Spec.Class), RootCause: v.Spec.RootCause,
			SymptomAt: v.Spec.SymptomAt, Difficulty: string(v.Spec.Difficulty),
			Site: v.Spec.Site, SyntheticLoadReaches: v.Spec.SyntheticLoadReaches,
			ReloadObserved: v.ReloadObserved,
			EvalRate:       v.EvalRate, EvaluatingServices: svcs, Tier1Pass: v.Tier1Pass,
			Signal: v.Signal, Unit: v.Unit,
			Query:  strings.Join(strings.Fields(v.Query), " "),
			Before: v.Before, After: v.After, Delta: v.Delta,
			MinDelta: v.MinDelta, Tier2Pass: v.Tier2Pass,
			Valid: v.Valid(), Status: v.Status(),
			Note: v.Spec.Note, ElapsedSeconds: v.Elapsed.Seconds(),
			MeasuredAt: measuredAt,
		})
	}

	merged := mergeWithExisting(filepath.Join(dir, "characterization.json"), recs)

	payload := map[string]any{
		"settle_seconds":  settle.Seconds(),
		"recover_seconds": recover.Seconds(),
		"faults":          merged,
	}
	buf, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return err
	}
	jsonPath := filepath.Join(dir, "characterization.json")
	if err := os.WriteFile(jsonPath, append(buf, '\n'), 0o644); err != nil {
		return err
	}

	mdPath := filepath.Join(dir, "characterization.md")
	if err := os.WriteFile(mdPath, []byte(renderMarkdown(merged, settle, recover)), 0o644); err != nil {
		return err
	}
	fmt.Printf("\nwrote %s\n      %s\n", jsonPath, mdPath)
	return nil
}

func renderMarkdown(recs []record, settle, recover time.Duration) string {
	var b strings.Builder
	b.WriteString("# 故障生效校验结果\n\n")
	b.WriteString(fmt.Sprintf("settle `%s` / recover `%s`\n\n", settle, recover))
	b.WriteString("两级判据：**一级**该 flag 是否有服务在评估（静态属性，非前后差值）；" +
		"**二级**预期信号是否发生偏移。两级都过才可进场景库。\n\n")
	b.WriteString("| flag | 类别 | 根因 | 症状服务 | 难度 | 信号 | before | after | delta | 阈值 | 单位 | reload | 结论 |\n")
	b.WriteString("|---|---|---|---|---|---|---:|---:|---:|---:|---|:-:|---|\n")
	for _, r := range recs {
		verdict := "❌ " + r.Status
		if r.Valid {
			verdict = "✅ 可用"
		}
		reload := "✗"
		if r.ReloadObserved {
			reload = "✓"
		}
		b.WriteString(fmt.Sprintf("| `%s` | %s | %s | %s | %s | `%s` | %.4g | %.4g | %+.4g | %.4g | %s | %s | %s |\n",
			r.Flag, r.Class, r.RootCause, r.SymptomAt, r.Difficulty, r.Signal,
			r.Before, r.After, r.Delta, r.MinDelta, r.Unit, reload, verdict))
	}

	b.WriteString("\n## 源码作用点\n\n")
	b.WriteString("爆炸半径由这些位置的代码决定，不是由 flag 名字决定。\n\n")
	for _, r := range recs {
		b.WriteString(fmt.Sprintf("- **`%s`** → `%s`\n", r.Flag, r.Site))
	}

	b.WriteString("\n## 备注\n\n")
	for _, r := range recs {
		if r.Note != "" {
			b.WriteString(fmt.Sprintf("- **`%s`** — %s\n", r.Flag, r.Note))
		}
	}

	b.WriteString("\n## 判据查询\n\n")
	b.WriteString("定义在 `queries/signals.yaml`，Go 校验器与 Python 工具层共用。\n\n")
	for _, r := range recs {
		b.WriteString(fmt.Sprintf("- **`%s`** (signal `%s`)\n  ```promql\n  %s\n  ```\n",
			r.Flag, r.Signal, r.Query))
	}
	return b.String()
}

func filterSpecs(specs []inject.Spec, want []string) []inject.Spec {
	keep := map[string]bool{}
	for _, w := range want {
		keep[strings.TrimSpace(w)] = true
	}
	var out []inject.Spec
	for _, s := range specs {
		if keep[s.Flag] {
			out = append(out, s)
		}
	}
	return out
}

// locate walks up from the working directory to find the repo root, so this
// runs from anywhere inside it.
func locate(explicit string) (root, flagPath string, err error) {
	if explicit != "" {
		if _, err := os.Stat(explicit); err != nil {
			return "", "", fmt.Errorf("flag file %s: %w", explicit, err)
		}
		wd, _ := os.Getwd()
		return wd, explicit, nil
	}
	dir, err := os.Getwd()
	if err != nil {
		return "", "", err
	}
	for {
		candidate := filepath.Join(dir, defaultFlagPath)
		if _, err := os.Stat(candidate); err == nil {
			return dir, candidate, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", "", fmt.Errorf(
				"could not find %s walking up from the working directory; "+
					"run `make sut-fetch` or pass -file", defaultFlagPath)
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
	fmt.Fprintln(os.Stderr, "characterize:", err)
	os.Exit(1)
}

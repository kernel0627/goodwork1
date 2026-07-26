package inject

import (
	"context"
	"fmt"
	"math"
	"time"

	"github.com/kernel0627/medic/gateway/internal/dockerlog"
	"github.com/kernel0627/medic/gateway/internal/promq"
	"github.com/kernel0627/medic/gateway/internal/signals"
)

// Verdict is the outcome of characterising one fault.
type Verdict struct {
	Spec Spec

	// Tier 1 -- did flagd take the injection?
	ReloadObserved bool
	ReloadEvidence []string
	// EvalRate and EvaluatingServices are supporting evidence, not a criterion.
	// flagd's OpenTelemetry flag-evaluation metric is emitted by only one
	// service in this SUT, so its absence proves nothing.
	EvalRate           float64
	EvaluatingServices []string
	Tier1Pass          bool

	// Tier 2 -- did the expected signal actually move?
	Signal    string
	Unit      string
	Query     string
	Before    float64
	After     float64
	Delta     float64
	MinDelta  float64
	Tier2Pass bool

	Elapsed time.Duration
	Err     error
}

// Valid reports whether the fault is fit to build scenarios on.
//
// Both tiers must hold. Tier 1 alone means flagd took the file but nothing
// downstream changed. Tier 2 alone means the signal moved for some other reason
// -- noise, or residue from a previous scenario -- and attributing it to this
// fault would be wrong.
//
// Faults synthetic load cannot reach are excluded regardless: no traffic would
// ever trigger them, so however they verify is beside the point.
func (v Verdict) Valid() bool {
	return v.Err == nil && v.Tier1Pass && v.Tier2Pass && v.Spec.SyntheticLoadReaches
}

// Status renders a short reason, which is the part worth reading when a fault
// turns out not to work.
func (v Verdict) Status() string {
	switch {
	case v.Err != nil:
		return "ERROR: " + v.Err.Error()
	case !v.Spec.SyntheticLoadReaches:
		return "UNREACHABLE: synthetic load cannot drive this code path"
	case !v.Tier1Pass && !v.Tier2Pass:
		return "DEAD: flagd never saw the file change, and no signal moved"
	case !v.Tier1Pass:
		return "SUSPECT: signal moved but flagd logged no reload (likely noise)"
	case !v.Tier2Pass:
		return "INERT: flagd took the file but the expected signal did not move"
	default:
		return "OK"
	}
}

// Verifier characterises faults against the live SUT.
type Verifier struct {
	Inj     *Injector
	Prom    *promq.Client
	Signals *signals.Catalog

	// Settle is how long to wait after injecting before measuring. It must
	// cover flagd's file reload plus the collector's export interval plus the
	// query's own rate() window, or a working fault will look inert.
	Settle time.Duration

	// Recover is how long to wait after reverting before touching the next
	// fault. Without it, a previous fault's tail contaminates the next
	// fault's "before" reading.
	Recover time.Duration
}

// DefaultSettle and DefaultRecover are deliberately generous. The rate()
// windows in signals.yaml are 2m, so a shorter settle would measure a period
// that is mostly pre-injection and systematically under-report every fault.
const (
	DefaultSettle  = 150 * time.Second
	DefaultRecover = 90 * time.Second
)

// Verify characterises a single fault: clean the slate, measure, inject, wait,
// measure again, then revert.
func (v *Verifier) Verify(ctx context.Context, spec Spec) Verdict {
	start := time.Now()
	out := Verdict{Spec: spec, Signal: spec.Signal}
	defer func() { out.Elapsed = time.Since(start) }()

	sig, ok := v.Signals.Get(spec.Signal)
	if !ok {
		out.Err = fmt.Errorf("signal %q not in queries/signals.yaml", spec.Signal)
		return out
	}
	out.Unit = sig.Unit
	out.MinDelta = sig.MinDelta
	if spec.MinDeltaOverride > 0 {
		out.MinDelta = spec.MinDeltaOverride
	}
	if out.MinDelta <= 0 {
		out.Err = fmt.Errorf("signal %q has no usable threshold; any noise would pass", spec.Signal)
		return out
	}

	query, err := v.Signals.Query(spec.Signal, spec.Subs)
	if err != nil {
		out.Err = err
		return out
	}
	out.Query = query

	// Supporting evidence, gathered before anything is disturbed.
	if rate, err := v.Prom.Instant(ctx, v.Signals.MustQuery(
		"flag_evaluations", map[string]string{"flag": spec.Flag})); err == nil {
		out.EvalRate = zeroIfNoData(rate)
	}
	if svcs, err := v.Prom.LabelValues(ctx, "service_name",
		fmt.Sprintf(`feature_flag_evaluation_requests_total{feature_flag_key=%q}`, spec.Flag),
	); err == nil {
		out.EvaluatingServices = svcs
	}

	// Start from a known-clean state so `before` is a true baseline.
	if err := v.Inj.Reset(); err != nil {
		out.Err = fmt.Errorf("reset before measuring: %w", err)
		return out
	}
	if err := sleepCtx(ctx, v.Recover); err != nil {
		out.Err = err
		return out
	}

	before, err := v.Prom.Instant(ctx, query)
	if err != nil {
		out.Err = fmt.Errorf("baseline measurement: %w", err)
		return out
	}
	out.Before = zeroIfNoData(before)

	// Mark the log position just before writing, so the reload check cannot be
	// satisfied by an event from an earlier scenario.
	injectAt := time.Now().Add(-2 * time.Second)

	v.Inj.ClearAll()
	if err := v.Inj.Set(spec.Flag, spec.Variant); err != nil {
		out.Err = fmt.Errorf("stage fault: %w", err)
		return out
	}
	if err := v.Inj.Commit(); err != nil {
		out.Err = fmt.Errorf("commit fault: %w", err)
		return out
	}

	// Always revert, even if measurement below fails. Leaving a fault injected
	// would poison every subsequent measurement in the run.
	defer func() {
		if err := v.Inj.Reset(); err != nil && out.Err == nil {
			out.Err = fmt.Errorf("revert after measuring: %w", err)
		}
	}()

	// Some faults need longer than the default before they are visible; see
	// Spec.SettleSeconds.
	settle := v.Settle
	if spec.SettleSeconds > 0 {
		settle = time.Duration(spec.SettleSeconds) * time.Second
	}
	if err := sleepCtx(ctx, settle); err != nil {
		out.Err = err
		return out
	}

	reloaded, evidence, err := dockerlog.FlagdReloaded(ctx, injectAt)
	if err != nil {
		out.Err = fmt.Errorf("tier-1 reload check: %w", err)
		return out
	}
	out.ReloadObserved = reloaded
	out.ReloadEvidence = evidence
	out.Tier1Pass = reloaded

	after, err := v.Prom.Instant(ctx, query)
	if err != nil {
		out.Err = fmt.Errorf("post-injection measurement: %w", err)
		return out
	}
	out.After = zeroIfNoData(after)
	out.Delta = out.After - out.Before

	if spec.SignalRises {
		out.Tier2Pass = out.Delta >= out.MinDelta
	} else {
		out.Tier2Pass = -out.Delta >= out.MinDelta
	}
	return out
}

// VerifyAll characterises every fault in order, reporting each verdict as it
// lands so a long run is observable rather than silent.
//
// Faults are done one at a time on purpose: two injected at once would make it
// impossible to attribute a moved signal to either.
func (v *Verifier) VerifyAll(ctx context.Context, specs []Spec, onResult func(int, int, Verdict)) []Verdict {
	out := make([]Verdict, 0, len(specs))
	for i, spec := range specs {
		verdict := v.Verify(ctx, spec)
		out = append(out, verdict)
		if onResult != nil {
			onResult(i+1, len(specs), verdict)
		}
		if ctx.Err() != nil {
			break
		}
	}
	return out
}

// zeroIfNoData folds an empty result set to zero.
//
// Distinguishing the two matters when writing a query, and promq keeps them
// apart for that reason. By the time a fault is being characterised the query
// is already known to be well formed, and "no series" and "zero" mean the same
// thing: nothing is happening.
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

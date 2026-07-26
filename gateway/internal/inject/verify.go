package inject

import (
	"context"
	"fmt"
	"math"
	"time"

	"github.com/kernel0627/medic/gateway/internal/promq"
)

// Verdict is the outcome of characterising one fault.
type Verdict struct {
	Spec Spec

	// Tier 1 -- can this fault fire at all?
	EvalRate           float64
	EvaluatingServices []string
	Tier1Pass          bool

	// Tier 2 -- did the expected signal actually move?
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
// Both tiers must hold. Tier 1 alone means the flag is read but changes
// nothing. Tier 2 alone means the signal moved for some other reason -- noise,
// or residue from a previous scenario -- and attributing it to this fault would
// be wrong.
func (v Verdict) Valid() bool { return v.Err == nil && v.Tier1Pass && v.Tier2Pass }

// Status renders a short reason, which is the part worth reading when a fault
// turns out not to work.
func (v Verdict) Status() string {
	switch {
	case v.Err != nil:
		return "ERROR: " + v.Err.Error()
	case !v.Tier1Pass && !v.Tier2Pass:
		return "DEAD: no service evaluates the flag, and no signal moved"
	case !v.Tier1Pass:
		return "SUSPECT: signal moved but no service evaluates the flag (likely noise)"
	case !v.Tier2Pass:
		return "INERT: flag is evaluated but the expected signal did not move"
	default:
		return "OK"
	}
}

// Verifier characterises faults against the live SUT.
type Verifier struct {
	Inj  *Injector
	Prom *promq.Client

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
// windows in the catalog are 2m, so a shorter settle would measure a period
// that is mostly pre-injection and systematically under-report every fault.
const (
	DefaultSettle  = 150 * time.Second
	DefaultRecover = 90 * time.Second
)

// Verify characterises a single fault: clean the slate, measure, inject, wait,
// measure again, then revert.
func (v *Verifier) Verify(ctx context.Context, spec Spec) Verdict {
	start := time.Now()
	out := Verdict{Spec: spec, MinDelta: thresholdFor(spec)}
	defer func() { out.Elapsed = time.Since(start) }()

	// Tier 1 first: if nothing evaluates the flag, injecting proves nothing
	// and there is no reason to spend minutes waiting.
	rate, err := v.Prom.Instant(ctx, EvaluationQuery(spec.Flag))
	if err != nil {
		out.Err = fmt.Errorf("tier-1 query: %w", err)
		return out
	}
	out.EvalRate = zeroIfNoData(rate)
	out.Tier1Pass = out.EvalRate > 0

	if svcs, err := v.Prom.LabelValues(ctx, "service_name",
		EvaluatingServicesQuery(spec.Flag)); err == nil {
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

	before, err := v.Prom.Instant(ctx, spec.Effect)
	if err != nil {
		out.Err = fmt.Errorf("baseline measurement: %w", err)
		return out
	}
	out.Before = zeroIfNoData(before)

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

	if err := sleepCtx(ctx, v.Settle); err != nil {
		out.Err = err
		return out
	}

	after, err := v.Prom.Instant(ctx, spec.Effect)
	if err != nil {
		out.Err = fmt.Errorf("post-injection measurement: %w", err)
		return out
	}
	out.After = zeroIfNoData(after)
	out.Delta = out.After - out.Before

	if spec.EffectRises {
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

// thresholdFor resolves the tier-2 threshold, preferring a fault's own override.
//
// Overrides exist because a class floor can sit above what a fault is capable
// of producing. cartFailure only affects EmptyCart, which synthetic traffic
// calls about once every 30 seconds, so its ceiling is roughly 0.03 failures
// per second against a 0.05 class floor: with the default it reads as inert no
// matter how completely it is failing.
func thresholdFor(s Spec) float64 {
	if s.MinDeltaOverride > 0 {
		return s.MinDeltaOverride
	}
	return minDelta(s.Class)
}

// minDelta is the smallest change worth calling an effect, per fault class.
//
// A pure relative threshold does not work here: error rates sit at zero when
// healthy, so any relative test either divides by zero or fires on noise.
// These are absolute floors in each metric's own units.
func minDelta(c Class) float64 {
	switch c {
	case ClassError, ClassConnectivity, ClassHealth:
		return 0.05 // failed requests per second
	case ClassLatency, ClassCache:
		return 50 // milliseconds at p99
	case ClassResource:
		return 0.05 // utilisation ratio, i.e. 5 percentage points
	case ClassQueue:
		return 1 // records of consumer lag
	default:
		return 0
	}
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

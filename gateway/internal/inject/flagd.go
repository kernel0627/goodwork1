// Package inject injects and withdraws faults in the System Under Test.
//
// The OpenTelemetry Demo runs flagd with `--uri file:./etc/flagd/demo.flagd.json`
// over a bind mount, so rewriting that file on the host is the entire injection
// mechanism: no API, no exec into containers, no service restarts.
//
// # Drift is the enemy
//
// A benchmark harness rewrites this file hundreds of times. If each write were
// applied to the previous write's output, small errors would accumulate and
// silently corrupt later scenarios. So the file is never mutated in place.
// Instead a pristine baseline is snapshotted once, and every commit is computed
// as baseline + the currently active fault set. Reset is therefore exact, and
// scenario N is byte-identical to scenario 1 given the same fault set.
package inject

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// BaselineSuffix names the pristine snapshot kept beside the live flag file.
const BaselineSuffix = ".medic-baseline"

// OffVariant is the conventional "no fault" variant used by every demo flag.
const OffVariant = "off"

// flagSet mirrors demo.flagd.json. Field order matches the upstream file so a
// no-op round trip is byte-stable; see TestRoundTripIsByteStable.
type flagSet struct {
	Schema string           `json:"$schema,omitempty"`
	Flags  map[string]*flag `json:"flags"`
}

type flag struct {
	DefaultVariant string                     `json:"defaultVariant"`
	Description    string                     `json:"description,omitempty"`
	State          string                     `json:"state"`
	Targeting      json.RawMessage            `json:"targeting,omitempty"`
	Variants       map[string]json.RawMessage `json:"variants"`
}

// Info describes one flag as declared in the baseline.
type Info struct {
	Name           string
	Description    string
	DefaultVariant string
	Variants       []string
	// HasTargeting reports whether a targeting rule overrides defaultVariant.
	// Such flags need the rule rewritten, not just the default flipped.
	HasTargeting bool
}

// Injector owns the flag file and the active fault set.
//
// Not safe for concurrent use. The runner drives scenarios sequentially by
// design: two faults injected at once would make root-cause labels ambiguous.
type Injector struct {
	path     string
	baseline []byte
	decoded  *flagSet          // parsed baseline, never mutated
	active   map[string]string // flag name -> variant
}

// New loads the flag file at path and establishes the baseline snapshot,
// creating it from the current file contents on first use.
func New(path string) (*Injector, error) {
	basePath := path + BaselineSuffix

	baseline, err := os.ReadFile(basePath)
	if errors.Is(err, os.ErrNotExist) {
		if baseline, err = os.ReadFile(path); err != nil {
			return nil, fmt.Errorf("read flag file %s: %w", path, err)
		}
		if err := writeAtomic(basePath, baseline); err != nil {
			return nil, fmt.Errorf("snapshot baseline: %w", err)
		}
	} else if err != nil {
		return nil, fmt.Errorf("read baseline %s: %w", basePath, err)
	}

	var fs flagSet
	if err := json.Unmarshal(baseline, &fs); err != nil {
		return nil, fmt.Errorf("parse baseline: %w", err)
	}
	if len(fs.Flags) == 0 {
		return nil, fmt.Errorf("baseline %s declares no flags", basePath)
	}

	return &Injector{
		path:     path,
		baseline: baseline,
		decoded:  &fs,
		active:   map[string]string{},
	}, nil
}

// List returns every declared flag, sorted by name.
func (in *Injector) List() []Info {
	out := make([]Info, 0, len(in.decoded.Flags))
	for name, f := range in.decoded.Flags {
		variants := make([]string, 0, len(f.Variants))
		for v := range f.Variants {
			variants = append(variants, v)
		}
		sort.Strings(variants)
		out = append(out, Info{
			Name:           name,
			Description:    f.Description,
			DefaultVariant: f.DefaultVariant,
			Variants:       variants,
			HasTargeting:   len(f.Targeting) > 0,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Set stages a fault. It validates against the baseline rather than trusting
// the caller: a typo'd variant that silently no-ops would produce a scenario
// that looks injected but is not, which is the single worst failure mode for
// a benchmark.
func (in *Injector) Set(name, variant string) error {
	f, ok := in.decoded.Flags[name]
	if !ok {
		return fmt.Errorf("unknown flag %q", name)
	}
	if _, ok := f.Variants[variant]; !ok {
		have := make([]string, 0, len(f.Variants))
		for v := range f.Variants {
			have = append(have, v)
		}
		sort.Strings(have)
		return fmt.Errorf("flag %q has no variant %q (have %v)", name, variant, have)
	}
	if variant == OffVariant {
		delete(in.active, name)
		return nil
	}
	in.active[name] = variant
	return nil
}

// Clear removes a staged fault. ClearAll removes every staged fault.
func (in *Injector) Clear(name string) { delete(in.active, name) }
func (in *Injector) ClearAll()         { in.active = map[string]string{} }

// Active returns a copy of the staged fault set.
func (in *Injector) Active() map[string]string {
	out := make(map[string]string, len(in.active))
	for k, v := range in.active {
		out[k] = v
	}
	return out
}

// Commit writes baseline + active faults to the flag file atomically.
//
// With no active faults this restores the baseline exactly, so Commit doubles
// as the scenario teardown path.
func (in *Injector) Commit() error {
	var fs flagSet
	if err := json.Unmarshal(in.baseline, &fs); err != nil {
		return fmt.Errorf("re-parse baseline: %w", err)
	}

	for name, variant := range in.active {
		f := fs.Flags[name]
		f.DefaultVariant = variant

		// A targeting rule wins over defaultVariant in flagd, so flipping the
		// default alone would be a no-op. Rewriting the rule's outcomes keeps
		// the flag's intended scope (e.g. "fail only product X") while making
		// it actually fire.
		if len(f.Targeting) > 0 {
			rewritten, err := rewriteTargeting(f.Targeting, variant)
			if err != nil {
				return fmt.Errorf("flag %q: %w", name, err)
			}
			f.Targeting = rewritten
		}
	}

	out, err := marshalLikeUpstream(&fs)
	if err != nil {
		return err
	}
	return writeAtomic(in.path, out)
}

// Reset clears all staged faults and restores the pristine file.
func (in *Injector) Reset() error {
	in.ClearAll()
	return writeAtomic(in.path, in.baseline)
}

// LiveVariants reads the flag file as it currently stands on disk and returns
// each flag's effective defaultVariant.
//
// This deliberately reads the live file rather than the baseline: it answers
// "what is the SUT actually configured with right now", which is how a run
// detects state left behind by a crashed scenario.
func LiveVariants(path string) (map[string]string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var fs flagSet
	if err := json.Unmarshal(b, &fs); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	out := make(map[string]string, len(fs.Flags))
	for name, f := range fs.Flags {
		out[name] = f.DefaultVariant
	}
	return out, nil
}

// rewriteTargeting replaces the outcome branches of a JsonLogic `if` rule with
// variant, leaving the condition untouched.
//
// Upstream shape:
//
//	{"if": [ <condition>, <then>, <else> ]}
//
// Only <then> is set to variant; <else> stays "off" so the rule remains scoped
// to whatever the condition selects.
func rewriteTargeting(raw json.RawMessage, variant string) (json.RawMessage, error) {
	var rule map[string]json.RawMessage
	if err := json.Unmarshal(raw, &rule); err != nil {
		return nil, fmt.Errorf("parse targeting: %w", err)
	}
	ifRaw, ok := rule["if"]
	if !ok {
		return nil, fmt.Errorf("unsupported targeting rule: no `if` key; handle it explicitly rather than guessing")
	}
	var arms []json.RawMessage
	if err := json.Unmarshal(ifRaw, &arms); err != nil {
		return nil, fmt.Errorf("parse targeting `if`: %w", err)
	}
	if len(arms) < 2 {
		return nil, fmt.Errorf("targeting `if` has %d arms, need at least condition+then", len(arms))
	}

	thenArm, err := json.Marshal(variant)
	if err != nil {
		return nil, err
	}
	arms[1] = thenArm
	if len(arms) < 3 {
		off, _ := json.Marshal(OffVariant)
		arms = append(arms, off)
	}

	newIf, err := json.Marshal(arms)
	if err != nil {
		return nil, err
	}
	rule["if"] = newIf
	return json.Marshal(rule)
}

// marshalLikeUpstream encodes with the same 2-space indent and trailing
// newline the upstream file uses, so diffs stay readable and round trips are
// byte-stable.
func marshalLikeUpstream(fs *flagSet) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	if err := enc.Encode(fs); err != nil {
		return nil, fmt.Errorf("encode flag file: %w", err)
	}
	return buf.Bytes(), nil
}

// writeAtomic writes via a temp file in the same directory then renames, so a
// reader (flagd) never observes a partially written file.
func writeAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("create temp in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once the rename succeeds

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("write temp: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("sync temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp: %w", err)
	}
	if err := os.Chmod(tmpName, 0o644); err != nil {
		return fmt.Errorf("chmod temp: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("rename onto %s: %w", path, err)
	}
	return nil
}

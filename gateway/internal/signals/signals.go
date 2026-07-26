// Package signals loads the shared signal-query catalog from queries/signals.yaml.
//
// The queries live in a data file rather than in Go because the Python tool
// layer needs the same ones. Measuring an error rate on this SUT means covering
// four metric families at once, and getting it wrong yields a confident zero
// rather than an error: a failing service reads as healthy. Writing that logic
// twice means getting it wrong twice, and the second victim would be the agent,
// whose competence would then be measured against a bug in its own instruments.
package signals

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// DefaultPath is where the catalog lives relative to the repository root.
const DefaultPath = "queries/signals.yaml"

// Signal is one named measurement.
type Signal struct {
	Name    string `yaml:"-"`
	Summary string `yaml:"summary"`
	Unit    string `yaml:"unit"`
	PromQL  string `yaml:"promql"`

	// MinDelta is the smallest change that counts as a real effect, in this
	// signal's own units.
	//
	// It lives beside the query because it is a property of the units, and
	// units here are not guessable. Measured: container_memory_percent_ratio
	// returns 93.76, so it is 0..100 rather than the 0..1 its name implies;
	// container_cpu_utilization_ratio reached 1.586, so it counts cores rather
	// than a fraction. A threshold chosen per fault *class* rather than per
	// signal was off by a factor of 100 for memory.
	MinDelta float64 `yaml:"min_delta"`

	// MultiSeries marks queries that intentionally return many series and so
	// cannot be evaluated as a scalar.
	MultiSeries bool `yaml:"multi_series"`
}

// Catalog is the parsed contents of signals.yaml.
type Catalog struct {
	defaults map[string]string
	signals  map[string]Signal
}

type rawCatalog struct {
	Defaults map[string]string `yaml:"defaults"`
	Signals  map[string]Signal `yaml:"signals"`
}

// Load reads the catalog from path.
func Load(path string) (*Catalog, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read signal catalog %s: %w", path, err)
	}
	var raw rawCatalog
	if err := yaml.Unmarshal(b, &raw); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if len(raw.Signals) == 0 {
		return nil, fmt.Errorf("%s declares no signals", path)
	}
	for name, s := range raw.Signals {
		if strings.TrimSpace(s.PromQL) == "" {
			return nil, fmt.Errorf("%s: signal %q has no promql", path, name)
		}
		s.Name = name
		raw.Signals[name] = s
	}
	return &Catalog{defaults: raw.Defaults, signals: raw.Signals}, nil
}

// LoadFromRepo walks up from the working directory to find the catalog, so
// commands work from anywhere inside the repository.
func LoadFromRepo() (*Catalog, error) {
	dir, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	for {
		candidate := filepath.Join(dir, DefaultPath)
		if _, err := os.Stat(candidate); err == nil {
			return Load(candidate)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return nil, fmt.Errorf("could not find %s walking up from the working directory", DefaultPath)
		}
		dir = parent
	}
}

// Names lists every signal, sorted.
func (c *Catalog) Names() []string {
	out := make([]string, 0, len(c.signals))
	for n := range c.signals {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// Get returns a signal by name.
func (c *Catalog) Get(name string) (Signal, bool) {
	s, ok := c.signals[name]
	return s, ok
}

// Query renders a signal's PromQL with the given substitutions.
//
// Placeholders are %SERVICE%, %CONTAINER%, %FLAG% and %WINDOW%; keys are given
// without the percent signs. %WINDOW% falls back to the catalog default.
//
// An unsubstituted placeholder is an error rather than a query sent as-is.
// PromQL containing a literal %SERVICE% would not fail -- it would match
// nothing and return zero, which is indistinguishable from a healthy service.
func (c *Catalog) Query(name string, subs map[string]string) (string, error) {
	s, ok := c.signals[name]
	if !ok {
		return "", fmt.Errorf("unknown signal %q (have %v)", name, c.Names())
	}

	merged := map[string]string{}
	for k, v := range c.defaults {
		merged[strings.ToUpper(k)] = v
	}
	for k, v := range subs {
		merged[strings.ToUpper(k)] = v
	}

	q := s.PromQL
	for k, v := range merged {
		q = strings.ReplaceAll(q, "%"+k+"%", v)
	}
	q = strings.TrimSpace(q)

	if i := strings.Index(q, "%"); i >= 0 {
		if j := strings.Index(q[i+1:], "%"); j >= 0 {
			return "", fmt.Errorf(
				"signal %q still contains placeholder %s after substitution; "+
					"an unsubstituted placeholder matches nothing and returns zero, "+
					"which looks exactly like a healthy service",
				name, q[i:i+j+2])
		}
	}
	return q, nil
}

// MustQuery is Query for call sites where a failure is a programming error.
func (c *Catalog) MustQuery(name string, subs map[string]string) string {
	q, err := c.Query(name, subs)
	if err != nil {
		panic(err)
	}
	return q
}

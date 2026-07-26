// Command injector stages and withdraws faults in the System Under Test.
//
//	injector list                              # every flag, its variants, what is live
//	injector set cartFailure=50% adHighCpu=on  # stage and commit a fault set
//	injector reset                             # restore the pristine baseline
//	injector status                            # what differs from baseline right now
//
// Faults are written by rewriting flagd's demo.flagd.json, which the SUT mounts
// as a bind mount. See docs/sut.md §3.
//
// `set` is absolute, not additive: the committed state is exactly the fault set
// named on the command line. Anything omitted is reverted to baseline. A
// harness that accumulated faults across scenarios would silently contaminate
// every run after the first.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/kernel0627/medic/gateway/internal/inject"
)

const defaultFlagPath = "sut/opentelemetry-demo/src/flagd/demo.flagd.json"

func main() {
	fs := flag.NewFlagSet("injector", flag.ExitOnError)
	path := fs.String("file", env("MEDIC_FLAGD_FILE", ""), "path to demo.flagd.json")
	fs.Usage = usage
	// Split the subcommand off before parsing so flags may follow it.
	args := os.Args[1:]
	var sub string
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		sub, args = args[0], args[1:]
	}
	_ = fs.Parse(args)

	if sub == "" {
		usage()
		os.Exit(2)
	}

	resolved, err := resolveFlagPath(*path)
	if err != nil {
		die(err)
	}
	in, err := inject.New(resolved)
	if err != nil {
		die(err)
	}

	switch sub {
	case "list":
		runList(in)
	case "set":
		runSet(in, fs.Args())
	case "reset":
		if err := in.Reset(); err != nil {
			die(err)
		}
		fmt.Println("reset to baseline")
	case "status":
		runStatus(in, resolved)
	default:
		fmt.Fprintf(os.Stderr, "unknown subcommand %q\n\n", sub)
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `injector — stage and withdraw faults in the SUT

usage:
  injector list                                 list flags, variants, and what is live
  injector set <flag>=<variant> [...]           commit exactly this fault set
  injector set                                  commit an empty fault set (= reset)
  injector reset                                restore the pristine baseline
  injector status                               show what differs from baseline

flags:
  -file PATH    path to demo.flagd.json
                (default: $MEDIC_FLAGD_FILE, else `+defaultFlagPath+` relative to repo root)

note:
  `+"`set`"+` is absolute, not additive. Faults not named are reverted.
`)
}

func runList(in *inject.Injector) {
	infos := in.List()
	faults, knobs := 0, 0
	for _, i := range infos {
		if isKnob(i.Name) {
			knobs++
		} else {
			faults++
		}
	}
	fmt.Printf("%d flags — %d faults, %d experiment knobs\n\n", len(infos), faults, knobs)

	for _, i := range infos {
		kind := "fault"
		if isKnob(i.Name) {
			kind = "knob "
		}
		mark := ""
		if i.HasTargeting {
			// Worth surfacing: a targeting rule overrides defaultVariant, so
			// this flag needs its rule rewritten rather than the default
			// flipped. Getting that wrong is a silently ineffective injection.
			mark = "  [targeting]"
		}
		fmt.Printf("  %s %-28s %s%s\n", kind, i.Name, strings.Join(i.Variants, " "), mark)
		if i.Description != "" {
			fmt.Printf("          %s\n", i.Description)
		}
	}

	if active := in.Active(); len(active) > 0 {
		fmt.Printf("\nstaged: %s\n", formatSet(active))
	}
}

func runSet(in *inject.Injector, assignments []string) {
	in.ClearAll()
	for _, a := range assignments {
		name, variant, ok := strings.Cut(a, "=")
		if !ok {
			die(fmt.Errorf("bad assignment %q, want flag=variant", a))
		}
		if err := in.Set(strings.TrimSpace(name), strings.TrimSpace(variant)); err != nil {
			die(err)
		}
	}
	if err := in.Commit(); err != nil {
		die(err)
	}

	active := in.Active()
	if len(active) == 0 {
		fmt.Println("committed empty fault set (baseline)")
		return
	}
	fmt.Printf("injected: %s\n", formatSet(active))
	fmt.Println("\nflagd hot-reloads the file, but reload is neither instant nor guaranteed.")
	fmt.Println("Verify the fault is visible in metrics before trusting a scenario.")
}

func runStatus(in *inject.Injector, path string) {
	live, err := inject.LiveVariants(path)
	if err != nil {
		die(err)
	}
	base := map[string]string{}
	for _, i := range in.List() {
		base[i.Name] = i.DefaultVariant
	}

	var drift []string
	for name, v := range live {
		if b, ok := base[name]; ok && b != v {
			drift = append(drift, fmt.Sprintf("%s=%s (baseline %s)", name, v, b))
		}
	}
	sort.Strings(drift)

	fmt.Printf("file: %s\n", path)
	if len(drift) == 0 {
		fmt.Println("state: clean (matches baseline)")
		return
	}
	fmt.Printf("state: %d flag(s) differ from baseline\n", len(drift))
	for _, d := range drift {
		fmt.Printf("  %s\n", d)
	}
}

// isKnob distinguishes experiment variables from faults. Load knobs change the
// metric baseline, so they must be held fixed across control arms rather than
// treated as injectable faults.
func isKnob(name string) bool {
	return strings.HasPrefix(name, "loadGenerator")
}

func formatSet(m map[string]string) string {
	parts := make([]string, 0, len(m))
	for k, v := range m {
		parts = append(parts, k+"="+v)
	}
	sort.Strings(parts)
	return strings.Join(parts, " ")
}

// resolveFlagPath finds demo.flagd.json, walking up from the working directory
// so the command works from anywhere in the repo.
func resolveFlagPath(explicit string) (string, error) {
	if explicit != "" {
		if _, err := os.Stat(explicit); err != nil {
			return "", fmt.Errorf("flag file %s: %w", explicit, err)
		}
		return explicit, nil
	}
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		candidate := filepath.Join(dir, defaultFlagPath)
		if _, err := os.Stat(candidate); err == nil {
			return candidate, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf(
				"could not find %s walking up from working directory; "+
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
	fmt.Fprintln(os.Stderr, "injector:", err)
	os.Exit(1)
}

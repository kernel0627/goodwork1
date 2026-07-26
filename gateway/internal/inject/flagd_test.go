package inject

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const fixture = "testdata/demo.flagd.json"

// stage copies the fixture into a temp dir so tests never touch the real SUT.
func stage(t *testing.T) string {
	t.Helper()
	src, err := os.ReadFile(fixture)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	dst := filepath.Join(t.TempDir(), "demo.flagd.json")
	if err := os.WriteFile(dst, src, 0o644); err != nil {
		t.Fatalf("stage fixture: %v", err)
	}
	return dst
}

func load(t *testing.T, path string) flagSet {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var fs flagSet
	if err := json.Unmarshal(b, &fs); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	return fs
}

// TestRoundTripIsByteStable is the load-bearing test for this package.
//
// A benchmark harness rewrites the flag file hundreds of times. If a no-op
// commit did not reproduce the input exactly, every run would drift from the
// last and later scenarios would silently differ from earlier ones.
func TestRoundTripIsByteStable(t *testing.T) {
	path := stage(t)
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	in, err := New(path)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := in.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(original) {
		t.Errorf("no-op commit changed the file.\n--- want %d bytes ---\n%s\n--- got %d bytes ---\n%s",
			len(original), original, len(got), got)
	}
}

func TestBaselineSnapshotIsCreatedOnce(t *testing.T) {
	path := stage(t)
	basePath := path + BaselineSuffix

	if _, err := os.Stat(basePath); !os.IsNotExist(err) {
		t.Fatalf("baseline should not exist before New, stat err = %v", err)
	}
	if _, err := New(path); err != nil {
		t.Fatalf("New: %v", err)
	}
	snap, err := os.ReadFile(basePath)
	if err != nil {
		t.Fatalf("baseline not created: %v", err)
	}

	// A later New must reuse the pristine snapshot even if the live file has
	// been left dirty by a crashed run.
	if err := os.WriteFile(path, []byte(`{"flags":{"bogus":{"defaultVariant":"x","state":"ENABLED","variants":{"x":1}}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	in2, err := New(path)
	if err != nil {
		t.Fatalf("New on dirty file: %v", err)
	}
	if _, ok := in2.decoded.Flags["cartFailure"]; !ok {
		t.Error("second New parsed the dirty live file instead of the baseline")
	}
	if again, _ := os.ReadFile(basePath); string(again) != string(snap) {
		t.Error("baseline was overwritten by a dirty live file")
	}
}

func TestSetSimpleFlag(t *testing.T) {
	path := stage(t)
	in, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := in.Set("cartFailure", "50%"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := in.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	fs := load(t, path)
	if got := fs.Flags["cartFailure"].DefaultVariant; got != "50%" {
		t.Errorf("cartFailure defaultVariant = %q, want %q", got, "50%")
	}
	if got := fs.Flags["paymentFailure"].DefaultVariant; got != "off" {
		t.Errorf("unrelated flag was modified: paymentFailure = %q", got)
	}
}

// TestSetTargetedFlagRewritesRule guards the one flag whose targeting rule
// overrides defaultVariant. Flipping only the default would look like a
// successful injection while the fault never fires.
func TestSetTargetedFlagRewritesRule(t *testing.T) {
	path := stage(t)
	in, err := New(path)
	if err != nil {
		t.Fatal(err)
	}

	before := in.decoded.Flags["productCatalogFailure"]
	if len(before.Targeting) == 0 {
		t.Skip("fixture no longer has targeting on productCatalogFailure; revisit docs/faults.md")
	}

	if err := in.Set("productCatalogFailure", "on"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := in.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	fs := load(t, path)
	f := fs.Flags["productCatalogFailure"]
	if f.DefaultVariant != "on" {
		t.Errorf("defaultVariant = %q, want %q", f.DefaultVariant, "on")
	}

	var rule map[string][]json.RawMessage
	if err := json.Unmarshal(f.Targeting, &rule); err != nil {
		t.Fatalf("parse rewritten targeting: %v", err)
	}
	arms := rule["if"]
	if len(arms) != 3 {
		t.Fatalf("targeting `if` has %d arms, want 3", len(arms))
	}
	if got := string(arms[1]); got != `"on"` {
		t.Errorf("then-arm = %s, want \"on\"", got)
	}
	if got := string(arms[2]); got != `"off"` {
		t.Errorf("else-arm = %s, want \"off\" (rule must stay scoped)", got)
	}
	// The condition must survive untouched, or the fault changes scope.
	if !strings.Contains(string(arms[0]), "product_id") {
		t.Errorf("condition was altered: %s", arms[0])
	}
}

func TestResetRestoresBaselineExactly(t *testing.T) {
	path := stage(t)
	original, _ := os.ReadFile(path)

	in, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range [][2]string{
		{"cartFailure", "100%"},
		{"paymentFailure", "75%"},
		{"productCatalogFailure", "on"},
		{"emailMemoryLeak", "1000x"},
	} {
		if err := in.Set(f[0], f[1]); err != nil {
			t.Fatalf("Set %v: %v", f, err)
		}
	}
	if err := in.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := in.Reset(); err != nil {
		t.Fatalf("Reset: %v", err)
	}

	got, _ := os.ReadFile(path)
	if string(got) != string(original) {
		t.Error("Reset did not restore the file byte-for-byte")
	}
	if len(in.Active()) != 0 {
		t.Errorf("Reset left %d active faults", len(in.Active()))
	}
}

// TestCommitIsIdempotentAcrossRuns simulates the harness looping over
// scenarios: the same fault set must always produce identical bytes, no matter
// what ran before it.
func TestCommitIsIdempotentAcrossRuns(t *testing.T) {
	path := stage(t)
	in, err := New(path)
	if err != nil {
		t.Fatal(err)
	}

	apply := func(pairs ...[2]string) []byte {
		in.ClearAll()
		for _, p := range pairs {
			if err := in.Set(p[0], p[1]); err != nil {
				t.Fatalf("Set %v: %v", p, err)
			}
		}
		if err := in.Commit(); err != nil {
			t.Fatal(err)
		}
		b, _ := os.ReadFile(path)
		return b
	}

	first := apply([2]string{"cartFailure", "25%"})
	apply([2]string{"paymentFailure", "100%"}, [2]string{"adHighCpu", "on"})
	apply([2]string{"emailMemoryLeak", "10000x"})
	again := apply([2]string{"cartFailure", "25%"})

	if string(first) != string(again) {
		t.Error("same fault set produced different bytes after intervening scenarios (drift)")
	}
}

func TestSetOffClearsFault(t *testing.T) {
	path := stage(t)
	in, _ := New(path)
	if err := in.Set("cartFailure", "50%"); err != nil {
		t.Fatal(err)
	}
	if err := in.Set("cartFailure", OffVariant); err != nil {
		t.Fatal(err)
	}
	if len(in.Active()) != 0 {
		t.Errorf(`Set(..., "off") should clear the fault, active = %v`, in.Active())
	}
}

// TestSetRejectsBadInput matters more than it looks: a typo that silently
// no-ops yields a scenario that appears injected but is not, which corrupts
// results without any visible failure.
func TestSetRejectsBadInput(t *testing.T) {
	path := stage(t)
	in, _ := New(path)

	for _, tc := range []struct{ name, flag, variant string }{
		{"unknown flag", "cartFailur", "50%"},
		{"unknown variant", "cartFailure", "60%"},
		{"empty variant", "cartFailure", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := in.Set(tc.flag, tc.variant); err == nil {
				t.Errorf("Set(%q, %q) succeeded, want error", tc.flag, tc.variant)
			}
		})
	}
}

func TestListCoversFixture(t *testing.T) {
	path := stage(t)
	in, _ := New(path)
	got := in.List()

	if len(got) < 14 {
		t.Errorf("List returned %d flags, expected the fixture's 15", len(got))
	}
	byName := map[string]Info{}
	for _, i := range got {
		byName[i.Name] = i
	}
	if _, ok := byName["cartFailure"]; !ok {
		t.Fatal("cartFailure missing from List")
	}
	if n := len(byName["cartFailure"].Variants); n != 7 {
		t.Errorf("cartFailure has %d variants, want 7", n)
	}
	if !byName["productCatalogFailure"].HasTargeting {
		t.Error("productCatalogFailure should be flagged as having targeting")
	}
	if byName["cartFailure"].HasTargeting {
		t.Error("cartFailure should not be flagged as having targeting")
	}
}

// TestWriteAtomicLeavesNoTempFiles keeps the SUT's flagd directory clean; a
// stray *.tmp-* there would be mounted into the container.
func TestWriteAtomicLeavesNoTempFiles(t *testing.T) {
	path := stage(t)
	dir := filepath.Dir(path)
	in, _ := New(path)
	for i := 0; i < 5; i++ {
		_ = in.Set("cartFailure", "50%")
		if err := in.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp-") {
			t.Errorf("left temp file %s", e.Name())
		}
	}
}

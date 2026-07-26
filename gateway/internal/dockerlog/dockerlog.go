// Package dockerlog reads container logs from the SUT.
//
// The demo's compose files set the json-file logging driver, so every
// container's stdout is retrievable through `docker logs`. That is the whole
// mechanism: no log backend is required, which is why opensearch is not a
// dependency of anything here.
package dockerlog

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// FlagdContainer is the SUT's flagd container. Overridable because a compose
// project prefix or a rename would change it.
const FlagdContainer = "flagd"

// reloadMarker is what flagd logs when it notices the flag file change.
//
// Observed form:
//
//	2026-07-26T14:57:14.512Z  info  file/filepath_sync.go:110
//	  filepath event: ./etc/flagd/demo.flagd.json WRITE  {"component":"sync",...}
//
// Note "sync": "fileinfo" in the structured tail: flagd is polling file info
// rather than using inotify. That matters because the injector writes via
// temp-file-and-rename, and a watcher keyed on the inode could have missed it.
// Polling means it does not.
const reloadMarker = "filepath event"

// Tail returns a container's log lines emitted since the given time.
func Tail(ctx context.Context, container string, since time.Time) ([]string, error) {
	cmd := exec.CommandContext(ctx, "docker", "logs", container,
		"--since", since.UTC().Format(time.RFC3339))

	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("docker logs %s: %w: %s",
			container, err, strings.TrimSpace(errb.String()))
	}

	// docker interleaves stdout and stderr of the container; flagd writes to
	// stderr, so both streams have to be considered.
	combined := out.String() + errb.String()
	var lines []string
	for _, l := range strings.Split(combined, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			lines = append(lines, l)
		}
	}
	return lines, nil
}

// FlagdReloaded reports whether flagd noticed a flag-file change since the
// given time, and returns the matching log lines as evidence.
//
// This is tier-1 fault verification. It replaced an earlier check that counted
// flagd's OpenTelemetry flag-evaluation metric, which turned out to be emitted
// by only one service in the SUT -- the .NET cart -- so eleven of the thirteen
// faults were reported as never evaluated. Every one of those was a false
// negative, and adFailure was dismissed on that basis despite its signal having
// visibly moved.
//
// A log line is weaker evidence than a metric: it proves flagd read the file,
// not that any service received the new value. It is used anyway because it is
// the only signal that holds for all thirteen faults, and pairing it with a
// source-verified read site covers what it cannot show on its own.
func FlagdReloaded(ctx context.Context, since time.Time) (bool, []string, error) {
	lines, err := Tail(ctx, FlagdContainer, since)
	if err != nil {
		return false, nil, err
	}
	var evidence []string
	for _, l := range lines {
		if strings.Contains(l, reloadMarker) {
			evidence = append(evidence, l)
		}
	}
	return len(evidence) > 0, evidence, nil
}

// GrepErrors returns log lines from a container that look like failures.
//
// Deliberately crude: it matches on level tokens rather than parsing, because
// the SUT is polyglot and its services share no log format. Six languages here
// mean six shapes of log line, and a parser for all of them would be a project
// of its own.
func GrepErrors(ctx context.Context, container string, since time.Time, limit int) ([]string, error) {
	lines, err := Tail(ctx, container, since)
	if err != nil {
		return nil, err
	}
	needles := []string{
		"error", "ERROR", "Error",
		"exception", "Exception", "EXCEPTION",
		"fail", "FAIL", "Fail",
		"panic", "fatal", "FATAL",
		"warn", "WARN",
	}
	var hits []string
	for _, l := range lines {
		for _, n := range needles {
			if strings.Contains(l, n) {
				hits = append(hits, l)
				break
			}
		}
		if limit > 0 && len(hits) >= limit {
			break
		}
	}
	return hits, nil
}

// Available reports whether the docker CLI can be reached, so a caller can
// degrade gracefully rather than failing every scenario.
func Available(ctx context.Context) error {
	cmd := exec.CommandContext(ctx, "docker", "info", "--format", "{{.ServerVersion}}")
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("docker unavailable: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

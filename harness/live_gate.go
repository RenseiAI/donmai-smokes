package harness

// live_gate.go — the one gate every live smoke passes through.
//
// Before this file, each live smoke open-coded its own precondition block,
// and every copy shared the same defect:
//
//	if err != nil {
//	    if strings.Contains(err.Error(), "resolve ../") ||
//	        strings.Contains(err.Error(), "no such file") ||
//	        strings.Contains(err.Error(), "executable file not found") {
//	        t.Skipf("donmai binary unavailable: %v", err)   // <- swallows the failure
//	    }
//	    t.Fatalf("build donmai binary: %v", err)
//	}
//
// Nine copies of it (step3, step6-install, step11, step16, step17, step18,
// step19, step20, live_daemon.go). String-matching an error message to decide
// between "skip" and "fail" is guessing, and it guessed wrong twice over:
//
//   - a MISSING checkout ("source dir ... does not exist: ... no such file or
//     directory") became a skip, which is how the whole live suite reported
//     `ok` in 1.2s from a linked worktree; and
//   - a genuine COMPILE ERROR in donmai would too, whenever the compiler
//     mentions a missing file — "no such file" is a substring of a great many
//     real failures.
//
// The helpers here replace all nine copies. They separate the two cases
// structurally rather than lexically:
//
//	SkipIfShort / SkipIfKnob   an operator opted out    -> skip, recorded
//	DeclineLive                SUT lacks the surface    -> skip, recorded
//	RequireDonmaiSource        SUT cannot be located    -> FAIL, loudly
//	RequireDonmaiBinary        SUT will not build       -> FAIL, loudly
//
// Every one of them files into the liveness ledger, so the skip and its
// bookkeeping are a single call and cannot drift apart.

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// SkipIfShort skips and records an explicit `-short` opt-out.
func SkipIfShort(t *testing.T, reason string) {
	t.Helper()
	if !testing.Short() {
		return
	}
	RecordLive(t.Name(), LiveOptedOut, "-short: "+reason)
	t.Skip(reason + "; skipped under -short")
}

// SkipIfKnob skips and records an explicit operator opt-out when env is set
// to "1". env must be one of the documented DONMAI_SMOKES_SKIP_* knobs — the
// CI-operator contract in AGENTS.md.
func SkipIfKnob(t *testing.T, env, reason string) {
	t.Helper()
	if os.Getenv(env) != "1" {
		return
	}
	RecordLive(t.Name(), LiveOptedOut, env+"=1: "+reason)
	t.Skipf("%s=1 — %s", env, reason)
}

// SkipIfToolMissing skips when tool is not on PATH, recording the skip as a
// DECLINE rather than an opt-out.
//
// The classification is the point: nobody asked for this skip, and a machine
// without git or node cannot produce evidence about donmai. Filing it as a
// decline means a run where EVERY live smoke skipped for want of a tool is
// red, while a machine that merely lacks node (so the pi/opencode lanes skip
// but the daemon lanes run) stays green on the strength of what did execute.
func SkipIfToolMissing(t *testing.T, tool, why string) {
	t.Helper()
	if _, err := exec.LookPath(tool); err == nil {
		return
	}
	detail := fmt.Sprintf("%s not on PATH — %s", tool, why)
	RecordLive(t.Name(), LiveDeclined, detail)
	t.Skip(detail)
}

// DeclineLive records that the SUT was located but does not carry the surface
// this smoke asserts against (a checkout predating the feature), then skips.
//
// Use it ONLY after the SUT has been positively located — that is the whole
// distinction this file draws. Calling it on an unlocatable checkout is what
// made a missing sibling masquerade as an old one: "predates the feature" and
// "there is no checkout at all" produced the identical `--- SKIP`, and the
// second is not a precondition, it is a failure.
//
// A declined smoke is honest but is NOT evidence: a run in which every live
// smoke declines is red (see CheckLiveness).
func DeclineLive(t *testing.T, format string, args ...any) {
	t.Helper()
	detail := fmt.Sprintf(format, args...)
	RecordLive(t.Name(), LiveDeclined, detail)
	t.Skip(detail)
}

// RequireDonmaiSource returns the donmai checkout under test, or FAILS the
// test with an actionable message naming everywhere it looked.
//
// It does not skip. A smoke that cannot find the binary it exists to drive has
// not established a precondition, it has hit an error — and reporting `ok` for
// it is how this suite came to certify a system nobody had run.
func RequireDonmaiSource(t *testing.T) string {
	t.Helper()
	dir, err := LocateDonmaiSource()
	if err != nil {
		t.Fatalf("%s cannot run: %v", t.Name(), err)
	}
	return dir
}

// RequireDonmaiSourceAt validates an explicitly-named checkout — a per-step
// in-flight source override such as DONMAI_ARCH_SOURCE_DIR, or a feature
// worktree — and FAILS if it is not a donmai checkout. An empty dir delegates
// to RequireDonmaiSource.
//
// Naming a path is a statement of intent, so a bad path is an error here too:
// falling back to the canonical sibling would quietly test a different commit
// than the operator asked for.
func RequireDonmaiSourceAt(t *testing.T, dir string) string {
	t.Helper()
	if dir == "" {
		return RequireDonmaiSource(t)
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		t.Fatalf("%s cannot run: resolve donmai source %q: %v", t.Name(), dir, err)
	}
	if reason := describeNonDonmaiCheckout(abs); reason != "" {
		t.Fatalf("%s cannot run: donmai source %q is not a donmai checkout: %s\n"+
			"(a donmai checkout declares module %s in go.mod and ships cmd/donmai)",
			t.Name(), abs, reason, donmaiModulePath)
	}
	return abs
}

// LiveBinaryOptions configures RequireDonmaiBinary.
type LiveBinaryOptions struct {
	// SourceDir is the donmai checkout to build. Empty auto-locates it via
	// RequireDonmaiSource; non-empty is validated by RequireDonmaiSourceAt.
	SourceDir string

	// OutputPath is where to write the binary. Empty means
	// filepath.Join(t.TempDir(), "donmai").
	OutputPath string

	// Env is the build environment. Nil means os.Environ() with GOWORK=off
	// appended, which decouples the build from any org-root go.work overlay
	// (matching `make test`'s GOWORK=off discipline).
	Env []string

	// LogSink, when non-nil, receives build output live.
	LogSink io.Writer

	// Timeout caps the build. Zero means 3 minutes — generous against the
	// 60-90s cold build.
	Timeout time.Duration
}

// RequireDonmaiBinary locates the donmai checkout, builds the binary, records
// the smoke as having EXERCISED the SUT, and returns the binary path plus the
// source directory it came from.
//
// Every failure here is fatal. There is deliberately no error-string
// classification: a located checkout that will not compile is a broken SUT,
// which is exactly what this suite is for.
func RequireDonmaiBinary(t *testing.T, opts LiveBinaryOptions) (binary, sourceDir string) {
	t.Helper()

	sourceDir = RequireDonmaiSourceAt(t, opts.SourceDir)

	outputPath := opts.OutputPath
	if outputPath == "" {
		outputPath = filepath.Join(t.TempDir(), "donmai")
	}
	env := opts.Env
	if env == nil {
		env = append(os.Environ(), "GOWORK=off")
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = 3 * time.Minute
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	binary, err := BuildBinary(ctx, BuildOptions{
		SourceDir:  sourceDir,
		EntryPoint: "./cmd/donmai",
		OutputPath: outputPath,
		Env:        env,
		LogSink:    opts.LogSink,
		Timeout:    timeout,
	})
	if err != nil {
		t.Fatalf("build the donmai binary under test from %s: %v", sourceDir, err)
	}

	RecordLive(t.Name(), LiveExercised, "built donmai from "+sourceDir)
	return binary, sourceDir
}

package smokes

// liveness_test.go — the gate that turns "the suite silently tested nothing"
// from a green run into a red one.
//
// `go test` exits 0 for a SKIP exactly as it does for a PASS, so a suite whose
// every live smoke skipped prints `ok` and stops there. That is how this
// harness once reported `ok github.com/RenseiAI/donmai-smokes 1.251s` from a
// linked worktree — the sibling donmai checkout was one directory further up
// than the hard-coded "../donmai", every build failed, every failure was
// string-matched into a t.Skip, and two RED CONTROLS "passed" against it
// before the timing tell (a cold donmai build alone is 60-90s) was noticed.
//
// The fix has two halves and this is the second:
//
//   - harness/sut.go + live_gate.go make an unlocatable or unbuildable SUT a
//     FAILURE rather than a skip, so the specific bug cannot recur silently;
//   - this TestMain asserts the outcome regardless of cause — a run that
//     selected live smokes and exercised none is red even if every individual
//     skip was, in isolation, defensible.
//
// The second half is the durable one. Any guard written against a known cause
// only catches that cause; this one catches the next one too.

import (
	"fmt"
	"os"
	"testing"

	afh "github.com/RenseiAI/donmai-smokes/harness"
)

// TestMain runs the suite, then refuses to report success for a run that
// proved nothing about the binary under test.
//
// It never converts a failing run into a passing one — the liveness check can
// only raise the exit code, never lower it.
func TestMain(m *testing.M) {
	code := m.Run()

	verdict := afh.CheckLiveness()
	// Print the tally unconditionally. `go test` surfaces it under -v or on
	// failure, which is exactly when someone is asking what actually ran —
	// the question a bare `ok` erases. CI already tees -v output, so the
	// line lands in the job log next to the PASS/FAIL/SKIP table.
	fmt.Fprintln(os.Stderr, verdict.Report)

	if code == 0 && !verdict.OK {
		code = 1
	}
	os.Exit(code)
}

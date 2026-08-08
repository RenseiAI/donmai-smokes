package harness

// liveness.go — the positive assertion that the suite actually RAN.
//
// `go test` exits 0 whether a test PASSED or SKIPPED. That is the whole
// problem: a suite whose every live smoke skips reports `ok`, and `ok` reads
// as "the daemon lifecycle works" when it means "nobody looked". A skip that
// reports ok is worse than a failure, because it launders absence of evidence
// into evidence of absence.
//
// Fixing the one bug that caused the last all-skip run (a fixed "../donmai"
// that misses from a linked worktree — see sut.go) does not fix the CLASS. The
// next cause will be a renamed capability-probe file, a skip knob left set in
// a CI matrix leg, or an install step that half-failed. So this file asserts
// the property we actually care about, independent of cause:
//
//	a run that SELECTED live smokes must EXERCISE the binary under test,
//	unless the operator explicitly opted out.
//
// Live smokes report into this ledger; TestMain in package smokes turns a
// violation into a non-zero exit (liveness_test.go). Three outcomes are
// recorded, and the distinction between them is the whole point:
//
//   - OPTED OUT — `-short`, or a DONMAI_SMOKES_SKIP_* knob. A human said
//     "don't". Legitimate; the skip knobs are the CI-operator contract.
//   - EXERCISED — the donmai binary under test was located and compiled, and
//     the smoke drove it. This is evidence.
//   - DECLINED — the SUT was located, but a capability probe found it lacks
//     the surface under test (a checkout that predates the feature). Honest,
//     but NOT evidence — a run that only declines has proven nothing.
//
// Nobody asked for a run that only declines, so the ledger reds it.

import (
	"fmt"
	"sort"
	"strings"
	"sync"
)

// SkipLiveDaemonEnv is the operator opt-out honored by every live smoke:
// set it to "1" to skip everything that spawns a real donmai process. It is
// part of the CI-operator contract (see AGENTS.md) — never repurpose it.
const SkipLiveDaemonEnv = "DONMAI_SMOKES_SKIP_LIVE_DAEMON"

// LiveOutcome is how one live smoke resolved its precondition gate.
type LiveOutcome string

const (
	// LiveOptedOut — a human asked for the skip (-short, or a skip knob).
	LiveOptedOut LiveOutcome = "opted-out"
	// LiveExercised — the SUT was located, built, and driven. Evidence.
	LiveExercised LiveOutcome = "exercised"
	// LiveDeclined — the SUT was located but lacks the surface under test.
	LiveDeclined LiveOutcome = "declined"
)

// LiveRecord is one ledger entry.
type LiveRecord struct {
	Test    string
	Outcome LiveOutcome
	Detail  string
}

var liveLedger = struct {
	mu      sync.Mutex
	records map[string]LiveRecord // keyed by test name; last write wins
}{records: map[string]LiveRecord{}}

// RecordLive files a live smoke's gate outcome. Safe for concurrent use;
// re-recording the same test name overwrites, so a smoke that first selects
// and later exercises ends up counted once, at its final outcome.
func RecordLive(test string, outcome LiveOutcome, detail string) {
	liveLedger.mu.Lock()
	defer liveLedger.mu.Unlock()
	liveLedger.records[test] = LiveRecord{Test: test, Outcome: outcome, Detail: detail}
}

// LiveLedger returns a snapshot of every recorded outcome, sorted by test
// name.
func LiveLedger() []LiveRecord {
	liveLedger.mu.Lock()
	defer liveLedger.mu.Unlock()
	out := make([]LiveRecord, 0, len(liveLedger.records))
	for _, r := range liveLedger.records {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Test < out[j].Test })
	return out
}

// ResetLiveLedger clears the ledger. Tests of the ledger itself use it; the
// suite does not.
func ResetLiveLedger() {
	liveLedger.mu.Lock()
	defer liveLedger.mu.Unlock()
	liveLedger.records = map[string]LiveRecord{}
}

// LivenessVerdict is the answer to "did this run prove anything?".
type LivenessVerdict struct {
	// OK is false when the run selected live smokes, exercised none, and
	// nobody opted out — i.e. the suite silently tested nothing.
	OK bool
	// Report is a human-readable tally, suitable for stderr either way.
	Report string
	Selected,
	Exercised,
	OptedOut,
	Declined int
}

// CheckLiveness evaluates the ledger.
//
// A run is RED exactly when live smokes DECLINED and none EXERCISED: the SUT
// was located, the suite decided it was too old to assert against, and so
// reported `ok` having proven nothing. Nobody asked for that.
//
// Deliberately NOT red:
//
//   - nothing selected (a -run filter, or a package with no live smokes) —
//     there is nothing to be silent about;
//   - everything opted out (-short, a DONMAI_SMOKES_SKIP_* knob) — a human
//     chose this, and the skip knobs are the CI-operator contract. Note a
//     mixed run (some opted out, the rest declined) IS red: the opt-out
//     covers the smokes it names, not the ones that quietly found nothing.
//
// The other half of the guarantee is not here: a SUT that cannot be located
// or built fails the test outright (live_gate.go), so it never reaches this
// tally. This check covers what survives that — the skips taken after the
// SUT was successfully found.
func CheckLiveness() LivenessVerdict {
	records := LiveLedger()

	v := LivenessVerdict{Selected: len(records)}
	var declined []LiveRecord
	for _, r := range records {
		switch r.Outcome {
		case LiveExercised:
			v.Exercised++
		case LiveOptedOut:
			v.OptedOut++
		case LiveDeclined:
			v.Declined++
			declined = append(declined, r)
		}
	}

	var b strings.Builder
	fmt.Fprintf(&b, "live-smoke liveness: selected=%d exercised=%d opted-out=%d declined=%d",
		v.Selected, v.Exercised, v.OptedOut, v.Declined)

	switch {
	case v.Selected == 0:
		v.OK = true
		b.WriteString(" (no live smoke selected in this run)")
	case v.Exercised > 0:
		v.OK = true
	case v.Declined == 0:
		v.OK = true
		b.WriteString(" (every selected live smoke was explicitly opted out)")
	default:
		v.OK = false
		b.WriteString("\n\nThe suite reported no failures but exercised the donmai binary ZERO times.\n" +
			"`go test` exits 0 for a skip, so this run would otherwise have reported `ok` while\n" +
			"proving nothing about the system under test.\n\n" +
			"Live smokes that declined (SUT located, but it lacks the surface under test):\n")
		for _, r := range declined {
			fmt.Fprintf(&b, "  - %s: %s\n", r.Test, r.Detail)
		}
		fmt.Fprintf(&b,
			"\nEither point the suite at a donmai checkout that has these surfaces\n"+
				"(%s=/path/to/donmai), or opt out explicitly (%s=1, or `go test -short`).\n",
			DonmaiSourceDirEnv, SkipLiveDaemonEnv)
	}

	v.Report = b.String()
	return v
}

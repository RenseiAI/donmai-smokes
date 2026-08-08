package harness

import (
	"strings"
	"testing"
)

// TestCheckLiveness_Verdicts is the truth table for the gate that turns a
// silent all-skip run red. Each row is a shape a real run can take.
//
// These cases must not be run in parallel: the ledger is process-global,
// exactly so that a TestMain at the end of the run can read what the whole
// binary did.
func TestCheckLiveness_Verdicts(t *testing.T) {
	for _, tc := range []struct {
		name    string
		records []LiveRecord
		wantOK  bool
		why     string
	}{
		{
			name:    "nothing selected",
			records: nil,
			wantOK:  true,
			why:     "a -run filter, or a package with no live smokes — nothing to be silent about",
		},
		{
			name: "the trap: everything declined, nothing exercised",
			records: []LiveRecord{
				{Test: "TestA", Outcome: LiveDeclined, Detail: "predates the feature"},
				{Test: "TestB", Outcome: LiveDeclined, Detail: "predates the feature"},
			},
			wantOK: false,
			why:    "this is the run that used to report ok in 1.2s having proven nothing",
		},
		{
			name: "one exercised is enough",
			records: []LiveRecord{
				{Test: "TestA", Outcome: LiveExercised},
				{Test: "TestB", Outcome: LiveDeclined, Detail: "predates the feature"},
			},
			wantOK: true,
			why:    "the suite proved something about the SUT",
		},
		{
			name: "operator opted everything out",
			records: []LiveRecord{
				{Test: "TestA", Outcome: LiveOptedOut, Detail: "-short"},
				{Test: "TestB", Outcome: LiveOptedOut, Detail: "DONMAI_SMOKES_SKIP_LIVE_DAEMON=1"},
			},
			wantOK: true,
			why:    "a human asked for this; the skip knobs are the CI-operator contract",
		},
		{
			name: "opted out AND declined, nothing exercised",
			records: []LiveRecord{
				{Test: "TestA", Outcome: LiveOptedOut, Detail: "-short"},
				{Test: "TestB", Outcome: LiveDeclined, Detail: "predates the feature"},
			},
			wantOK: false,
			why:    "the opt-out covers the smokes it names, not the one that quietly found nothing",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ResetLiveLedger()
			t.Cleanup(ResetLiveLedger)
			for _, r := range tc.records {
				RecordLive(r.Test, r.Outcome, r.Detail)
			}

			got := CheckLiveness()
			if got.OK != tc.wantOK {
				t.Errorf("OK = %v, want %v (%s)\nreport:\n%s", got.OK, tc.wantOK, tc.why, got.Report)
			}
			if got.Selected != len(tc.records) {
				t.Errorf("Selected = %d, want %d", got.Selected, len(tc.records))
			}
			if got.Report == "" {
				t.Error("Report is empty; the tally is the line that distinguishes ok from nobody-looked")
			}
			// A red verdict must say which smokes declined, or the operator
			// has to re-run with -v to learn anything.
			if !got.OK {
				for _, r := range tc.records {
					if r.Outcome == LiveDeclined && !strings.Contains(got.Report, r.Test) {
						t.Errorf("report omits declined smoke %s:\n%s", r.Test, got.Report)
					}
				}
			}
		})
	}
}

// TestRecordLive_LastOutcomeWins pins the overwrite semantics the ordering
// depends on: step16 builds the binary (EXERCISED) and only then probes it for
// the surface under test (DECLINED). The final outcome is the true one.
func TestRecordLive_LastOutcomeWins(t *testing.T) {
	ResetLiveLedger()
	t.Cleanup(ResetLiveLedger)

	RecordLive("TestArch", LiveExercised, "built donmai")
	RecordLive("TestArch", LiveDeclined, "binary predates the native arch-intel port")

	ledger := LiveLedger()
	if len(ledger) != 1 {
		t.Fatalf("ledger has %d entries, want 1 (same test recorded twice)", len(ledger))
	}
	if ledger[0].Outcome != LiveDeclined {
		t.Errorf("outcome = %q, want %q — the later record is the true one",
			ledger[0].Outcome, LiveDeclined)
	}

	v := CheckLiveness()
	if v.OK {
		t.Errorf("a lone build-then-decline must be red; report:\n%s", v.Report)
	}
}

// TestLiveLedger_Sorted keeps the report stable run to run: an operator
// diffing two failures should not have to sort the output first.
func TestLiveLedger_Sorted(t *testing.T) {
	ResetLiveLedger()
	t.Cleanup(ResetLiveLedger)

	for _, name := range []string{"TestZ", "TestA", "TestM"} {
		RecordLive(name, LiveDeclined, "x")
	}
	got := LiveLedger()
	want := []string{"TestA", "TestM", "TestZ"}
	for i, w := range want {
		if got[i].Test != w {
			t.Fatalf("ledger[%d] = %s, want %s (ledger must be sorted)", i, got[i].Test, w)
		}
	}
}

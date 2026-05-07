package harness

import (
	"fmt"
	"strings"
	"testing"
)

// ── IsUnknownSubcommand ───────────────────────────────────────────────────────

func TestIsUnknownSubcommand(t *testing.T) {
	cases := []struct {
		err  error
		want bool
	}{
		{fmt.Errorf("unknown command"), true},
		{fmt.Errorf("no such command xyz"), true},
		{fmt.Errorf("unknown flag --bogus"), true},
		{fmt.Errorf("some other error"), false},
		{nil, false},
	}
	for _, tc := range cases {
		if got := IsUnknownSubcommand(tc.err); got != tc.want {
			t.Errorf("IsUnknownSubcommand(%v) = %v, want %v", tc.err, got, tc.want)
		}
	}
}

// ── OutputLooksLikeUnknownSubcommand ──────────────────────────────────────────

func TestOutputLooksLikeUnknownSubcommand(t *testing.T) {
	cases := []struct {
		out  string
		want bool
	}{
		{"Error: unknown command \"capacity\"", true},
		{"unknown flag --bogus", true},
		{"no such command for parent", true},
		{"all good output", false},
		{"", false},
	}
	for _, tc := range cases {
		if got := OutputLooksLikeUnknownSubcommand(tc.out); got != tc.want {
			t.Errorf("OutputLooksLikeUnknownSubcommand(%q) = %v, want %v", tc.out, got, tc.want)
		}
	}
}

// ── WrapStep / StepError ──────────────────────────────────────────────────────

func TestWrapStep_Wraps(t *testing.T) {
	inner := fmt.Errorf("inner cause")
	err := WrapStep("outer context", inner)
	if err == nil {
		t.Fatal("WrapStep returned nil")
	}
	if !strings.Contains(err.Error(), "outer context") {
		t.Errorf("error missing context: %v", err)
	}
	if !strings.Contains(err.Error(), "inner cause") {
		t.Errorf("error missing inner cause: %v", err)
	}
}

func TestWrapStep_NilErr(t *testing.T) {
	if err := WrapStep("ctx", nil); err != nil {
		t.Errorf("WrapStep(nil) should return nil, got: %v", err)
	}
}

func TestStepError_Unwrap(t *testing.T) {
	inner := fmt.Errorf("root")
	wrapped := WrapStep("ctx", inner)
	se, ok := wrapped.(*StepError)
	if !ok {
		t.Fatalf("expected *StepError, got %T", wrapped)
	}
	if se.Unwrap() != inner {
		t.Errorf("Unwrap returned wrong cause: %v", se.Unwrap())
	}
}

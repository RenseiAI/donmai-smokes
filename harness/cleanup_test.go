package harness

import (
	"fmt"
	"strings"
	"testing"
)

// TestRunCleanups_NoErrors verifies that a sequence of all-passing
// hooks returns nil.
func TestRunCleanups_NoErrors(t *testing.T) {
	calls := 0
	hooks := []CleanupHook{
		FuncCleanupHook{HookName: "a", Fn: func() error { calls++; return nil }},
		FuncCleanupHook{HookName: "b", Fn: func() error { calls++; return nil }},
		FuncCleanupHook{HookName: "c", Fn: func() error { calls++; return nil }},
	}
	if err := RunCleanups(hooks); err != nil {
		t.Fatalf("RunCleanups: %v", err)
	}
	if calls != 3 {
		t.Errorf("expected 3 calls, got %d", calls)
	}
}

// TestRunCleanups_AggregatesErrors verifies that hooks that fail are
// aggregated into a single error and the sequence does NOT short-circuit
// — every hook runs regardless.
func TestRunCleanups_AggregatesErrors(t *testing.T) {
	calls := 0
	hooks := []CleanupHook{
		FuncCleanupHook{HookName: "a", Fn: func() error { calls++; return fmt.Errorf("oops-a") }},
		FuncCleanupHook{HookName: "b", Fn: func() error { calls++; return nil }},
		FuncCleanupHook{HookName: "c", Fn: func() error { calls++; return fmt.Errorf("oops-c") }},
	}
	err := RunCleanups(hooks)
	if err == nil {
		t.Fatal("expected aggregated error")
	}
	if calls != 3 {
		t.Errorf("expected all 3 hooks to run, got %d", calls)
	}
	if !strings.Contains(err.Error(), "a:") || !strings.Contains(err.Error(), "oops-a") {
		t.Errorf("aggregated error missing hook a: %v", err)
	}
	if !strings.Contains(err.Error(), "c:") || !strings.Contains(err.Error(), "oops-c") {
		t.Errorf("aggregated error missing hook c: %v", err)
	}
	if !strings.Contains(err.Error(), "2 error") {
		t.Errorf("aggregated error missing count: %v", err)
	}
}

// TestRunCleanups_NilHookSkipped verifies that nil entries in the slice
// are skipped without panicking — useful for callers that build their
// hook list conditionally.
func TestRunCleanups_NilHookSkipped(t *testing.T) {
	calls := 0
	hooks := []CleanupHook{
		nil,
		FuncCleanupHook{HookName: "a", Fn: func() error { calls++; return nil }},
		nil,
	}
	if err := RunCleanups(hooks); err != nil {
		t.Fatalf("RunCleanups: %v", err)
	}
	if calls != 1 {
		t.Errorf("expected 1 call, got %d", calls)
	}
}

// TestRunCleanups_Idempotent verifies that calling RunCleanups twice on
// the same idempotent hook list returns nil both times — the canonical
// re-invoke pattern (e.g., --cleanup-only retry).
func TestRunCleanups_Idempotent(t *testing.T) {
	calls := 0
	hooks := []CleanupHook{
		FuncCleanupHook{HookName: "a", Fn: func() error { calls++; return nil }},
	}
	if err := RunCleanups(hooks); err != nil {
		t.Fatalf("first run: %v", err)
	}
	if err := RunCleanups(hooks); err != nil {
		t.Fatalf("second run: %v", err)
	}
	if calls != 2 {
		t.Errorf("expected 2 calls across two runs, got %d", calls)
	}
}

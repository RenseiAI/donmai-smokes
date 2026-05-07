package harness

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
	"time"
)

// ── Runner.Run tests ─────────────────────────────────────────────────────────

func TestRunner_DryRun_NoExec(t *testing.T) {
	r := NewRunner(RunnerConfig{DryRun: true, Timeout: 5 * time.Second})
	out, err := r.Run("should-not-exist-binary", "--boom")
	if err != nil {
		t.Fatalf("dry-run should not fail on missing binary, got: %v", err)
	}
	if out != "" {
		t.Errorf("dry-run expected empty output, got: %q", out)
	}
}

func TestRunner_Run_Echo(t *testing.T) {
	r := NewRunner(RunnerConfig{Timeout: 5 * time.Second})
	out, err := r.Run("echo", "hello-smoke")
	if err != nil {
		t.Fatalf("echo failed: %v", err)
	}
	if !strings.Contains(out, "hello-smoke") {
		t.Errorf("expected 'hello-smoke' in output, got: %q", out)
	}
}

func TestRunner_Run_Failure(t *testing.T) {
	r := NewRunner(RunnerConfig{Timeout: 5 * time.Second})
	_, err := r.Run("false") // exits 1
	if err == nil {
		t.Fatal("expected error from 'false', got nil")
	}
}

func TestRunner_Run_NoArgs(t *testing.T) {
	r := NewRunner(RunnerConfig{Timeout: 5 * time.Second})
	_, err := r.Run()
	if err == nil {
		t.Fatal("expected error with no args")
	}
}

func TestRunner_Run_Timeout(t *testing.T) {
	r := NewRunner(RunnerConfig{Timeout: 100 * time.Millisecond})
	_, err := r.Run("sleep", "10")
	if err == nil {
		t.Fatal("expected timeout error from long-running sleep")
	}
}

func TestRunner_Run_Verbose(t *testing.T) {
	// Verbose mode should not change the return value.
	r := NewRunner(RunnerConfig{Timeout: 5 * time.Second, Verbose: true})
	out, err := r.Run("echo", "verbose-ok")
	if err != nil {
		t.Fatalf("verbose run failed: %v", err)
	}
	if !strings.Contains(out, "verbose-ok") {
		t.Errorf("expected 'verbose-ok' in output, got %q", out)
	}
}

// ── RunWithInput ──────────────────────────────────────────────────────────────

func TestRunner_RunWithInput(t *testing.T) {
	r := NewRunner(RunnerConfig{Timeout: 5 * time.Second})
	out, err := r.RunWithInput(strings.NewReader("hello from stdin\n"), "cat")
	if err != nil {
		t.Fatalf("RunWithInput cat failed: %v", err)
	}
	if !strings.Contains(out, "hello from stdin") {
		t.Errorf("expected 'hello from stdin' in output, got %q", out)
	}
}

func TestRunner_RunWithInput_DryRun(t *testing.T) {
	r := NewRunner(RunnerConfig{DryRun: true, Timeout: 5 * time.Second})
	var buf bytes.Buffer
	out, err := r.RunWithInput(&buf, "should-not-run")
	if err != nil {
		t.Fatalf("dry-run should not fail: %v", err)
	}
	if out != "" {
		t.Errorf("dry-run expected empty output, got %q", out)
	}
}

// ── assert helpers ────────────────────────────────────────────────────────────

func TestAssert_Pass(t *testing.T) {
	if err := Assert(true, "should not fail"); err != nil {
		t.Fatalf("Assert(true) returned error: %v", err)
	}
}

func TestAssert_Fail(t *testing.T) {
	err := Assert(false, "expected failure %s", "here")
	if err == nil {
		t.Fatal("Assert(false) should return error")
	}
	if !strings.Contains(err.Error(), "expected failure here") {
		t.Errorf("unexpected error message: %v", err)
	}
}

func TestAssertContains_Pass(t *testing.T) {
	if err := AssertContains("hello world", "world", "ctx"); err != nil {
		t.Fatalf("AssertContains should pass: %v", err)
	}
}

func TestAssertContains_Fail(t *testing.T) {
	err := AssertContains("hello world", "missing", "ctx")
	if err == nil {
		t.Fatal("AssertContains should fail when needle is absent")
	}
	if !strings.Contains(err.Error(), "missing") {
		t.Errorf("unexpected error: %v", err)
	}
}

// ── WaitFor ───────────────────────────────────────────────────────────────────

func TestRunner_WaitFor_ImmediateSuccess(t *testing.T) {
	r := NewRunner(RunnerConfig{})
	calls := 0
	err := r.WaitFor(2*time.Second, 50*time.Millisecond, func() error {
		calls++
		return nil
	})
	if err != nil {
		t.Fatalf("expected immediate success, got: %v", err)
	}
	if calls != 1 {
		t.Errorf("expected 1 call, got %d", calls)
	}
}

func TestRunner_WaitFor_EventualSuccess(t *testing.T) {
	r := NewRunner(RunnerConfig{})
	calls := 0
	err := r.WaitFor(2*time.Second, 200*time.Millisecond, func() error {
		calls++
		if calls < 3 {
			return fmt.Errorf("not yet")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("expected eventual success, got: %v", err)
	}
	if calls < 3 {
		t.Errorf("expected >= 3 calls, got %d", calls)
	}
}

func TestRunner_WaitFor_Timeout(t *testing.T) {
	r := NewRunner(RunnerConfig{})
	err := r.WaitFor(time.Second, 200*time.Millisecond, func() error {
		return fmt.Errorf("always failing")
	})
	if err == nil {
		t.Fatal("expected timeout error")
	}
}

// ── resolveBinary / injectGlobalFlags ─────────────────────────────────────────

func TestRunner_ResolveBinary_Override(t *testing.T) {
	r := NewRunner(RunnerConfig{
		BinaryOverride:       "/tmp/custom/af",
		BinaryOverrideSource: "flag",
		OverrideTarget:       "af",
	})
	if got := r.resolveBinary("af"); got != "/tmp/custom/af" {
		t.Errorf("resolveBinary(\"af\") with override = %q, want %q", got, "/tmp/custom/af")
	}
}

func TestRunner_ResolveBinary_PathDefault(t *testing.T) {
	r := NewRunner(RunnerConfig{OverrideTarget: "af"})
	if got := r.resolveBinary("af"); got != "af" {
		t.Errorf("resolveBinary(\"af\") with empty override = %q, want %q", got, "af")
	}
}

func TestRunner_ResolveBinary_NonTargetUnaffected(t *testing.T) {
	r := NewRunner(RunnerConfig{
		BinaryOverride: "/tmp/custom/af",
		OverrideTarget: "af",
	})
	for _, name := range []string{"gh", "git", "codesign", "launchctl", "systemctl", "rensei-daemon"} {
		if got := r.resolveBinary(name); got != name {
			t.Errorf("resolveBinary(%q) = %q, want %q (override should not affect non-target binaries)", name, got, name)
		}
	}
}

func TestRunner_InjectGlobalFlags_OnTarget(t *testing.T) {
	r := NewRunner(RunnerConfig{
		OverrideTarget: "af",
		GlobalFlags:    []string{"--url", "http://127.0.0.1:7734"},
	})
	got := r.injectGlobalFlags([]string{"af", "provider", "list"})
	want := []string{"af", "--url", "http://127.0.0.1:7734", "provider", "list"}
	if len(got) != len(want) {
		t.Fatalf("len: got %v, want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("[%d]: got %q, want %q", i, got[i], want[i])
		}
	}
}

func TestRunner_InjectGlobalFlags_NotTarget(t *testing.T) {
	r := NewRunner(RunnerConfig{
		OverrideTarget: "af",
		GlobalFlags:    []string{"--url", "http://127.0.0.1:7734"},
	})
	got := r.injectGlobalFlags([]string{"git", "status"})
	want := []string{"git", "status"}
	if len(got) != len(want) {
		t.Fatalf("len: got %v, want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("[%d]: got %q, want %q", i, got[i], want[i])
		}
	}
}

func TestRunner_InjectGlobalFlags_NoFlags(t *testing.T) {
	r := NewRunner(RunnerConfig{OverrideTarget: "af"})
	got := r.injectGlobalFlags([]string{"af", "provider", "list"})
	want := []string{"af", "provider", "list"}
	if len(got) != len(want) {
		t.Fatalf("len: got %v, want %v", got, want)
	}
}

// ── JSONUnmarshal ─────────────────────────────────────────────────────────────

func TestJSONUnmarshal(t *testing.T) {
	var got map[string]any
	if err := JSONUnmarshal(`{"k":"v","n":42}`, &got); err != nil {
		t.Fatalf("JSONUnmarshal: %v", err)
	}
	if got["k"] != "v" {
		t.Errorf("got[k] = %v, want \"v\"", got["k"])
	}
}

func TestJSONUnmarshal_Garbage(t *testing.T) {
	var got map[string]any
	if err := JSONUnmarshal("not json", &got); err == nil {
		t.Error("expected error from garbage input")
	}
}

package harness

import (
	"strings"
	"testing"
)

// TestLogTail_SmallWrites verifies a sequence of small writes that
// fit within cap accumulate in chronological order.
func TestLogTail_SmallWrites(t *testing.T) {
	tail := NewLogTail(64)
	if _, err := tail.Write([]byte("hello ")); err != nil {
		t.Fatalf("write 1: %v", err)
	}
	if _, err := tail.Write([]byte("world")); err != nil {
		t.Fatalf("write 2: %v", err)
	}
	if got := tail.String(); got != "hello world" {
		t.Errorf("String() = %q, want %q", got, "hello world")
	}
}

// TestLogTail_OverflowDisplacesOldest verifies that a write past cap
// displaces the oldest bytes, so the buffer always holds the trailing
// cap bytes of the input.
func TestLogTail_OverflowDisplacesOldest(t *testing.T) {
	tail := NewLogTail(8)
	if _, err := tail.Write([]byte("12345678")); err != nil {
		t.Fatalf("write 1: %v", err)
	}
	if got := tail.String(); got != "12345678" {
		t.Errorf("after fill String() = %q, want %q", got, "12345678")
	}
	// Push two more bytes; first two of the original should drop.
	if _, err := tail.Write([]byte("90")); err != nil {
		t.Fatalf("write 2: %v", err)
	}
	if got := tail.String(); got != "34567890" {
		t.Errorf("after overflow String() = %q, want %q", got, "34567890")
	}
}

// TestLogTail_HugeWriteOnlyKeepsTail verifies that a single write
// larger than cap retains only the trailing cap bytes.
func TestLogTail_HugeWriteOnlyKeepsTail(t *testing.T) {
	tail := NewLogTail(8)
	huge := strings.Repeat("X", 100) + "01234567"
	if _, err := tail.Write([]byte(huge)); err != nil {
		t.Fatalf("write: %v", err)
	}
	if got := tail.String(); got != "01234567" {
		t.Errorf("String() = %q, want %q", got, "01234567")
	}
}

// TestLogTail_EmptyWrite is a no-op.
func TestLogTail_EmptyWrite(t *testing.T) {
	tail := NewLogTail(8)
	n, err := tail.Write(nil)
	if err != nil {
		t.Fatalf("Write(nil): %v", err)
	}
	if n != 0 {
		t.Errorf("n = %d, want 0", n)
	}
	if tail.String() != "" {
		t.Errorf("String() = %q, want empty", tail.String())
	}
}

// TestNewLogTail_DefaultCap verifies a non-positive cap defaults to 8 KiB.
func TestNewLogTail_DefaultCap(t *testing.T) {
	tail := NewLogTail(0)
	if tail.cap != 8*1024 {
		t.Errorf("cap = %d, want 8192", tail.cap)
	}
}

package interactive

import (
	"context"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/attachclient/viewertest"
	"github.com/RenseiAI/donmai/attachwire"
	"github.com/RenseiAI/donmai/ptyhost"
)

// snapshotUntil polls Session.Snapshot() (which emits nothing, §12.1) until pred
// holds or the deadline passes. It returns the last screen it saw for diagnostics.
func snapshotUntil(t *testing.T, sess *ptyhost.Session, timeout time.Duration, pred func(attachwire.Screen) bool) attachwire.Screen {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last attachwire.Screen
	for time.Now().Before(deadline) {
		scr, _, err := sess.Snapshot()
		if err != nil {
			t.Fatalf("Snapshot: %v", err)
		}
		last = scr
		if pred(scr) {
			return scr
		}
		time.Sleep(15 * time.Millisecond)
	}
	t.Fatalf("snapshot predicate never held within %s\n%s", timeout, viewertest.Dump(last))
	return last
}

// TestPtyhostSessionLifecycle drives a standalone ptyhost.Session end to end with
// NO relay and NO platform: Spawn a real PTY process → observe its paint via
// Snapshot → apply a Resize (geometry reflected in the next snapshot) → Stop and
// assert the terminal Exit payload. This is the platform-free lifecycle floor the
// attach path builds on.
func TestPtyhostSessionLifecycle(t *testing.T) {
	// A shell that paints a deterministic banner then blocks alive on `sleep`,
	// so the session is observable and Stop's SIGTERM has a live process group.
	sess, err := ptyhost.Spawn(ptyhost.Spec{
		Command: []string{"sh", "-c", "printf 'PTYHOST-READY'; exec sleep 30"},
		Cols:    80,
		Rows:    24,
		Epoch:   7,
		Env:     []string{"TERM=xterm-256color"},
		Logger:  discardLogger(),
	})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = sess.Stop(ctx)
	})

	// The banner paints on the primary buffer; the epoch rides every snapshot.
	scr := snapshotUntil(t, sess, 10*time.Second, func(s attachwire.Screen) bool {
		return viewertest.RowText(s, 0) == "PTYHOST-READY"
	})
	if viewertest.IsAltScreen(scr) {
		t.Errorf("fresh shell should be on the primary buffer, got alt\n%s", viewertest.Dump(scr))
	}
	if scr.Epoch != 7 {
		t.Errorf("snapshot Epoch = %d, want 7 (Spec.Epoch is stamped verbatim)", scr.Epoch)
	}
	if got := viewertest.CellText(scr, 0, 0); got != "P" {
		t.Errorf("CellText(0,0) = %q, want %q", got, "P")
	}

	// Resize is applied verbatim to the PTY + VT; the next snapshot reflects the
	// new geometry.
	if err := sess.Resize(100, 40, 0, 0); err != nil {
		t.Fatalf("Resize: %v", err)
	}
	resized := snapshotUntil(t, sess, 5*time.Second, func(s attachwire.Screen) bool {
		return s.Cols == 100 && s.Rows == 40
	})
	if resized.Cols != 100 || resized.Rows != 40 {
		t.Errorf("post-resize geometry = %dx%d, want 100x40", resized.Cols, resized.Rows)
	}

	// A zero dimension is a framing error (§8), rejected before touching the PTY.
	if err := sess.Resize(0, 40, 0, 0); err == nil {
		t.Error("Resize(0,40) should return a framing error")
	}

	// Stop → SIGTERM the group → drain to EOF → Exit frame. sleep dies on the
	// signal, so the payload is a signal exit.
	stopCtx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	if err := sess.Stop(stopCtx); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	select {
	case <-sess.Done():
	case <-time.After(8 * time.Second):
		t.Fatal("Done never closed after Stop")
	}
	exit, ok := sess.Exit()
	if !ok {
		t.Fatal("Exit ok=false after Done closed")
	}
	// SIGTERM-killed child: signal exit, code 128+signum (§12.2).
	if exit.ExitCode < 128 {
		t.Errorf("expected signal-death exit code (>=128), got %d (%+v)", exit.ExitCode, exit)
	}

	// Post-Exit Snapshot keeps returning the final screen (§12.2), never errors.
	if _, _, err := sess.Snapshot(); err != nil {
		t.Errorf("post-Exit Snapshot errored: %v", err)
	}
	// WriteInput after exit is rejected, not a panic.
	if _, err := sess.WriteInput([]byte("x")); err == nil {
		t.Error("WriteInput after exit should error")
	}
}

// TestPtyhostInputRoundTrip proves WriteInput reaches the child and the child's
// resulting output is observable via Snapshot — the input half of the interactive
// surface, still relay-free. `cat` echoes its stdin, so a written line appears on
// the grid.
func TestPtyhostInputRoundTrip(t *testing.T) {
	sess, err := ptyhost.Spawn(ptyhost.Spec{
		Command: []string{"cat"},
		Cols:    80,
		Rows:    24,
		Epoch:   1,
		Env:     []string{"TERM=xterm-256color"},
		Logger:  discardLogger(),
	})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = sess.Stop(ctx)
	})

	if _, err := sess.WriteInput([]byte("SMOKE-INPUT\n")); err != nil {
		t.Fatalf("WriteInput: %v", err)
	}
	// cat writes the line back out; the VT paints it on row 0.
	snapshotUntil(t, sess, 10*time.Second, func(s attachwire.Screen) bool {
		return viewertest.RowText(s, 0) == "SMOKE-INPUT"
	})
}

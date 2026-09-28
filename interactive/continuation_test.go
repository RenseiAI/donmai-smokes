package interactive

import (
	"bytes"
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/attachclient/viewertest"
	"github.com/RenseiAI/donmai/attachwire"
	"github.com/RenseiAI/donmai/ptyhost"
	"golang.org/x/term"
)

// TestContinuationFixtureProcess is launched as a child under a real PTY by
// TestContinuationCheckpointPTYParity. Each read is a phase gate: the parent
// cannot accidentally checkpoint after the next suffix has already arrived.
func TestContinuationFixtureProcess(t *testing.T) {
	if os.Getenv("DONMAI_CONTINUATION_FIXTURE_CHILD") != "1" {
		return
	}
	if _, err := term.MakeRaw(int(os.Stdin.Fd())); err != nil {
		os.Exit(2)
	}
	write := func(s string) {
		if _, err := os.Stdout.WriteString(s); err != nil {
			os.Exit(2)
		}
	}
	read := func(want byte) {
		var b [1]byte
		if _, err := os.Stdin.Read(b[:]); err != nil || b[0] != want {
			os.Exit(2)
		}
	}
	// Title payload is intentionally unfinished. It must not enter the grid.
	write("\x1b[?1049h\x1b[2J\x1b[HBASE\x1b]0;secr")
	read('a')
	// Finish the title and stop halfway through a red SGR command.
	write("et\x07T\x1b[31")
	read('b')
	write("mRED\x1b[0m")
	read('q')
	write("\x1b[?1049l")
	os.Exit(0)
}

func TestContinuationCheckpointPTYParity(t *testing.T) {
	sess, err := ptyhost.Spawn(ptyhost.Spec{
		Command: []string{os.Args[0], "-test.run=^TestContinuationFixtureProcess$"},
		Env:     []string{"DONMAI_CONTINUATION_FIXTURE_CHILD=1"},
		Cols:    80,
		Rows:    24,
		Epoch:   19,
		Logger:  discardLogger(),
	})
	if err != nil {
		t.Fatalf("spawn continuation fixture: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = sess.Stop(ctx)
	})

	initial, err := sess.Subscribe(0)
	if err != nil {
		t.Fatalf("subscribe to fixture prefix: %v", err)
	}
	waitContinuationOutput(t, initial, nil, []byte("\x1b]0;secr"))
	_ = initial.Close()

	first, firstTail := openSmokeContinuation(t, sess)
	t.Cleanup(func() {
		if err := firstTail.Close(); err != nil {
			t.Errorf("close first tail: %v", err)
		}
	})
	if first.Epoch != 19 || first.AtSeq == 0 {
		t.Fatalf("first boundary epoch=%d atSeq=%d", first.Epoch, first.AtSeq)
	}
	firstMirror := restoreSmokeContinuation(t, first)
	t.Cleanup(func() {
		if err := firstMirror.Close(); err != nil {
			t.Errorf("close first mirror: %v", err)
		}
	})
	assertContinuationScreen(t, "title-prefix checkpoint", sess, firstMirror, "BASE")

	// The public local attach is the standalone pen authority. A second viewer
	// cannot write, while the driver can advance the real child through phases.
	driver, err := sess.AttachLocal(ptyhost.LocalAttachOptions{FromSeq: first.AtSeq})
	if err != nil {
		t.Fatalf("attach local driver: %v", err)
	}
	t.Cleanup(func() {
		if err := driver.Close(); err != nil {
			t.Errorf("close driver: %v", err)
		}
	})
	go drainContinuationViewer(driver.Frames())
	viewer, err := sess.AttachLocal(ptyhost.LocalAttachOptions{FromSeq: first.AtSeq})
	if err != nil {
		t.Fatalf("attach local viewer: %v", err)
	}
	t.Cleanup(func() {
		if err := viewer.Close(); err != nil {
			t.Errorf("close viewer: %v", err)
		}
	})
	go drainContinuationViewer(viewer.Frames())
	if !driver.CanDrive() || viewer.CanDrive() {
		t.Fatalf("pen allocation driver=%t viewer=%t", driver.CanDrive(), viewer.CanDrive())
	}
	if _, err := viewer.WriteInput([]byte{'a'}); !errors.Is(err, ptyhost.ErrLocalReadOnly) {
		t.Fatalf("read-only viewer write error=%v, want ErrLocalReadOnly", err)
	}
	if _, err := driver.WriteInput([]byte{'a'}); err != nil {
		t.Fatalf("driver phase a: %v", err)
	}
	waitContinuationOutput(t, firstTail, firstMirror, []byte("et\x07T\x1b[31"))
	assertContinuationScreen(t, "title closed and CSI pending", sess, firstMirror, "BASET")

	second, secondTail := openSmokeContinuation(t, sess)
	t.Cleanup(func() {
		if err := secondTail.Close(); err != nil {
			t.Errorf("close second tail: %v", err)
		}
	})
	if second.Epoch != first.Epoch || second.AtSeq <= first.AtSeq {
		t.Fatalf("second boundary epoch=%d atSeq=%d after first %d/%d", second.Epoch, second.AtSeq, first.Epoch, first.AtSeq)
	}
	secondMirror := restoreSmokeContinuation(t, second)
	t.Cleanup(func() {
		if err := secondMirror.Close(); err != nil {
			t.Errorf("close second mirror: %v", err)
		}
	})
	assertContinuationScreen(t, "partial CSI checkpoint", sess, secondMirror, "BASET")

	if _, err := driver.WriteInput([]byte{'b'}); err != nil {
		t.Fatalf("driver phase b: %v", err)
	}
	waitContinuationOutput(t, firstTail, firstMirror, []byte("mRED\x1b[0m"))
	waitContinuationOutput(t, secondTail, secondMirror, []byte("mRED\x1b[0m"))
	assertContinuationScreen(t, "first checkpoint after red suffix", sess, firstMirror, "BASETRED")
	assertContinuationScreen(t, "second checkpoint after red suffix", sess, secondMirror, "BASETRED")
	for name, mirror := range map[string]*ptyhost.ContinuationTerminal{"first": firstMirror, "second": secondMirror} {
		screen := mirror.Screen()
		if screen.ActiveBuffer != attachwire.BufferAlt || len(screen.Alt) < 8 {
			t.Fatalf("%s mirror missing alternate grid", name)
		}
		for col := 5; col < 8; col++ {
			cell := screen.Alt[col]
			if cell.FG.Mode != attachwire.ColorIndexed || cell.FG.Idx != 1 {
				t.Fatalf("%s mirror red cell %d has foreground %+v", name, col, cell.FG)
			}
		}
	}

	if _, err := driver.WriteInput([]byte{'q'}); err != nil {
		t.Fatalf("driver phase q: %v", err)
	}
	waitContinuationExit(t, firstTail, firstMirror)
	waitContinuationExit(t, secondTail, secondMirror)
	assertContinuationScreen(t, "final screen", sess, firstMirror, "")
	assertContinuationScreen(t, "final screen second mirror", sess, secondMirror, "")
}

func drainContinuationViewer(frames <-chan attachwire.Frame) {
	for range frames {
	}
}

func openSmokeContinuation(t *testing.T, sess *ptyhost.Session) (attachwire.ContinuationCheckpoint, agent.InteractiveSubscription) {
	t.Helper()
	checkpoint, tail, err := sess.OpenContinuation(attachwire.ContinuationSchema)
	if err != nil {
		t.Fatalf("open continuation: %v", err)
	}
	return checkpoint, tail
}

func restoreSmokeContinuation(t *testing.T, checkpoint attachwire.ContinuationCheckpoint) *ptyhost.ContinuationTerminal {
	t.Helper()
	encoded, err := checkpoint.Encode(attachwire.ContinuationSchema)
	if err != nil {
		t.Fatalf("encode checkpoint: %v", err)
	}
	decoded, err := attachwire.DecodeContinuationCheckpoint(encoded, attachwire.ContinuationSchema)
	if err != nil {
		t.Fatalf("decode checkpoint: %v", err)
	}
	mirror, err := ptyhost.RestoreContinuation(decoded)
	if err != nil {
		t.Fatalf("restore checkpoint: %v", err)
	}
	return mirror
}

func waitContinuationOutput(t *testing.T, tail agent.InteractiveSubscription, mirror *ptyhost.ContinuationTerminal, marker []byte) {
	t.Helper()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	var raw []byte
	for {
		select {
		case frame, ok := <-tail.Frames():
			if !ok {
				t.Fatalf("tail ended before marker %q", marker)
			}
			if mirror != nil {
				applySmokeRawFrame(t, mirror, frame)
			}
			if frame.Type == attachwire.TypeOutput {
				raw = append(raw, attachwire.DecodeOutput(frame.Payload).Data...)
				if bytes.Contains(raw, marker) {
					return
				}
			}
		case <-deadline.C:
			t.Fatalf("timed out waiting for raw marker %q; saw %q", marker, raw)
		}
	}
}

func waitContinuationExit(t *testing.T, tail agent.InteractiveSubscription, mirror *ptyhost.ContinuationTerminal) {
	t.Helper()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	for {
		select {
		case frame, ok := <-tail.Frames():
			if !ok {
				t.Fatal("tail ended before Exit")
			}
			applySmokeRawFrame(t, mirror, frame)
			if frame.Type == attachwire.TypeExit {
				if _, ok := mirror.Exit(); !ok {
					t.Fatal("mirror did not retain Exit")
				}
				return
			}
		case <-deadline.C:
			t.Fatal("timed out waiting for Exit")
		}
	}
}

func applySmokeRawFrame(t *testing.T, mirror *ptyhost.ContinuationTerminal, frame attachwire.Frame) {
	t.Helper()
	raw := frame.Encode()
	decoded, err := attachwire.DecodeFrame(raw)
	if err != nil || !bytes.Equal(decoded.Encode(), raw) {
		t.Fatalf("canonical host frame %d did not round-trip: %v", frame.Seq, err)
	}
	if err := mirror.ApplyRawFrame(decoded); err != nil {
		t.Fatalf("apply raw host frame %d: %v", frame.Seq, err)
	}
}

func assertContinuationScreen(t *testing.T, stage string, sess *ptyhost.Session, mirror *ptyhost.ContinuationTerminal, row string) {
	t.Helper()
	live, _, err := sess.Snapshot()
	if err != nil {
		t.Fatalf("%s live snapshot: %v", stage, err)
	}
	want, err := live.Encode()
	if err != nil {
		t.Fatalf("%s encode live screen: %v", stage, err)
	}
	got, err := mirror.Screen().Encode()
	if err != nil {
		t.Fatalf("%s encode mirror screen: %v", stage, err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("%s screen mismatch (live echo=%d, mirror echo=%d)\nlive: %s\nmirror: %s", stage, live.EchoMode, mirror.Screen().EchoMode, viewertest.Dump(live), viewertest.Dump(mirror.Screen()))
	}
	if row != "" && viewertest.RowText(mirror.Screen(), 0) != row {
		t.Errorf("%s row 0 = %q, want %q", stage, viewertest.RowText(mirror.Screen(), 0), row)
	}
}

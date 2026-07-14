package interactive

import (
	"context"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/attachclient/attachtest"
	"github.com/RenseiAI/donmai/attachclient/viewertest"
	"github.com/RenseiAI/donmai/attachwire"
	"github.com/RenseiAI/donmai/ptyhost"
)

// TestAttachVtfixtureSnapshotLoop is the CRITICAL-1 payoff: a headless VT
// screen-assert loop that CAN fail on garbled alt-screen output, where a
// byte-grep smoke would falsely pass.
//
// Local attach, platform-free: the deterministic `vtfixture` TUI runs in a real
// ptyhost.Session (80x24); the attachclient host leg forwards its frames to the
// attachtest STUB relay; a `driver`-role viewer attaches and drives input. At
// each stage we decode the authoritative Snapshot frame off the wire and assert
// exact screen state (alt-screen flag, cell text, row text, cursor position) per
// the vtfixture contract — the redraw-proof signal, not a substring match on a
// byte log.
func TestAttachVtfixtureSnapshotLoop(t *testing.T) {
	bin := buildFixture(t)

	// 1. Real PTY session running the deterministic fixture at 80x24, epoch 1.
	sess, err := ptyhost.Spawn(ptyhost.Spec{
		Command: []string{bin},
		Cols:    80,
		Rows:    24,
		Epoch:   1,
		Logger:  discardLogger(),
	})
	if err != nil {
		t.Fatalf("spawn fixture: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = sess.Stop(ctx)
	})

	// 2. Stub relay (the abstract "attach to a relay URL with a bearer" contract;
	// no closed relay service involved).
	relay := attachtest.New(attachtest.Config{RoomID: "room-1"})
	if err := relay.Start(); err != nil {
		t.Fatalf("relay start: %v", err)
	}
	t.Cleanup(func() { _ = relay.Close() })

	// 3. Host leg: forward the live session's frames to the relay.
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	startHostLeg(t, ctx, relay, sess, "sess-1")

	// 4. Driver viewer (driver role → holds the pen so input reaches the host).
	viewerTok := mkViewerToken("sess-1", "user-drv", "vjti-drv", "driver")
	v, err := attachtest.AttachViewer(ctx, relay.BaseWSURL(), viewerTok, attachwire.RoleDriver, nil)
	if err != nil {
		t.Fatalf("attach viewer: %v", err)
	}
	t.Cleanup(func() { _ = v.Close() })
	drv := viewertest.NewDriver(v)

	// ---- ON START: primary screen contract ---------------------------------
	joinCtx, jc := context.WithTimeout(ctx, 10*time.Second)
	defer jc()
	start, err := drv.SnapshotUntil(joinCtx, func(s attachwire.Screen) bool {
		return !viewertest.IsAltScreen(s) && viewertest.RowText(s, 0) == "FIXTURE-PRIMARY"
	})
	if err != nil {
		t.Fatalf("await primary screen: %v", err)
	}
	assertScreen(t, "start", start, screenExpect{
		alt:       false,
		row0:      "FIXTURE-PRIMARY",
		cell:      map[[2]int]string{{0, 0}: "F", {2, 4}: "R"},
		rowAt:     map[int]string{2: "    READY"},
		cursorRow: 5, cursorCol: 10,
	})

	// ---- AFTER 'a': alt-screen enter contract ------------------------------
	altCtx, ac := context.WithTimeout(ctx, 10*time.Second)
	defer ac()
	alt, err := drv.SendInputAndAwait(altCtx, []byte{'a'}, func(s attachwire.Screen) bool {
		return viewertest.IsAltScreen(s) && viewertest.CellText(s, 1, 2) == "A"
	})
	if err != nil {
		t.Fatalf("await alt screen after 'a': %v", err)
	}
	assertScreen(t, "alt", alt, screenExpect{
		alt:       true,
		row0:      "FIXTURE-ALT",
		cell:      map[[2]int]string{{0, 0}: "F", {1, 2}: "A"},
		rowAt:     map[int]string{1: "  ALPHA"},
		cursorRow: 7, cursorCol: 3,
	})

	// ---- AFTER 'q': alt-screen exit contract (primary restored + cursor) ----
	exitCtx, ec := context.WithTimeout(ctx, 10*time.Second)
	defer ec()
	back, err := drv.SendInputAndAwait(exitCtx, []byte{'q'}, func(s attachwire.Screen) bool {
		return !viewertest.IsAltScreen(s) && viewertest.RowText(s, 0) == "FIXTURE-PRIMARY"
	})
	if err != nil {
		t.Fatalf("await primary screen after 'q': %v", err)
	}
	assertScreen(t, "exit", back, screenExpect{
		alt:       false,
		row0:      "FIXTURE-PRIMARY",
		cell:      map[[2]int]string{{2, 4}: "R"},
		cursorRow: 5, cursorCol: 10,
	})
}

type screenExpect struct {
	alt                  bool
	row0                 string
	cell                 map[[2]int]string
	rowAt                map[int]string
	cursorRow, cursorCol int
}

func assertScreen(t *testing.T, stage string, s attachwire.Screen, want screenExpect) {
	t.Helper()
	if got := viewertest.IsAltScreen(s); got != want.alt {
		t.Errorf("[%s] IsAltScreen=%t want %t\n%s", stage, got, want.alt, viewertest.Dump(s))
	}
	if want.row0 != "" {
		if got := viewertest.RowText(s, 0); got != want.row0 {
			t.Errorf("[%s] RowText(0)=%q want %q\n%s", stage, got, want.row0, viewertest.Dump(s))
		}
	}
	for rc, txt := range want.cell {
		if got := viewertest.CellText(s, rc[0], rc[1]); got != txt {
			t.Errorf("[%s] CellText(%d,%d)=%q want %q", stage, rc[0], rc[1], got, txt)
		}
	}
	for row, txt := range want.rowAt {
		if got := viewertest.RowText(s, row); got != txt {
			t.Errorf("[%s] RowText(%d)=%q want %q", stage, row, got, txt)
		}
	}
	if r, c := viewertest.CursorAt(s); r != want.cursorRow || c != want.cursorCol {
		t.Errorf("[%s] CursorAt=(%d,%d) want (%d,%d)\n%s", stage, r, c, want.cursorRow, want.cursorCol, viewertest.Dump(s))
	}
}

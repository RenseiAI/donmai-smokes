package interactive

import (
	"context"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/attachwire"
	"github.com/RenseiAI/donmai/ptyhost"
)

// replayPrefix spawns `head -c <n> <raw>` inside a ptyhost.Session at the fixture
// geometry, so the recorded PTY-master byte stream up to a checkpoint is fed back
// through the REAL public Session path (child stdout → PTY master → VT), then
// returns the settled screen. This exercises the exact production feed path from
// OUTSIDE the module — the platform-free complement to donmai's own in-package
// vt_test.go, which feeds bytes into the unexported vtHost directly.
func replayPrefix(t *testing.T, rawPath string, n int, m fixtureMeta) attachwire.Screen {
	t.Helper()
	cols, rows := m.Cols, m.Rows
	if cols == 0 || rows == 0 {
		cols, rows = 80, 24
	}
	// stty raw -echo puts the slave in the same discipline a real full-screen TUI
	// sets: no OPOST output translation (bytes reach the VT verbatim, matching
	// donmai's in-package feedVT ground truth) and no echo — so the synchronous
	// query responder's replies (DA/DSR/OSC-color), written to the PTY master when
	// the recorded stream contains a probe, are NOT echoed back into the grid.
	// (`head`/`cat` leave the terminal cooked and never drain their stdin, so
	// without this the injected replies would loop back and corrupt the snapshot.)
	sess, err := ptyhost.Spawn(ptyhost.Spec{
		Command: []string{"sh", "-c", `stty raw -echo 2>/dev/null; exec head -c "$1" "$2"`, "sh", strconv.Itoa(n), rawPath},
		Cols:    uint16(cols), //nolint:gosec // G115: fixture geometry is a small terminal size
		Rows:    uint16(rows), //nolint:gosec // G115: fixture geometry is a small terminal size
		Epoch:   1,
		Env:     m.EnvVars,
		Logger:  discardLogger(),
	})
	if err != nil {
		t.Fatalf("spawn head: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = sess.Stop(ctx)
	})

	// head exits after writing n bytes; Done closes after the master drains to EOF
	// with every Output emitted, so the post-Done snapshot reflects all n bytes.
	select {
	case <-sess.Done():
	case <-time.After(15 * time.Second):
		t.Fatal("head replay session never reached Done")
	}
	scr, _, err := sess.Snapshot()
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	return scr
}

// TestRecordedVimAltScreen replays the deterministic vim recording through the
// public Session and asserts the alt-screen enters at `vim_opened` and the
// primary buffer is restored at `shell_after` (:q!). It also asserts the
// name+description+cmd fixture metadata is well-formed.
func TestRecordedVimAltScreen(t *testing.T) {
	moduleDir := donmaiModuleDir(t)
	raw, m := loadRecordedFixture(t, moduleDir, "vim")
	assertFixtureMeta(t, m, "vim")
	rawPath := filepath.Join(moduleDir, "ptyhost", "testdata", "vim.raw")

	openOff, ok := m.offset("vim_opened")
	if !ok {
		t.Fatal("vim_opened checkpoint missing")
	}
	scr := replayPrefix(t, rawPath, openOff, m)
	if scr.ActiveBuffer != attachwire.BufferAlt {
		t.Errorf("at vim_opened: expected alt-screen active, got buffer %d", scr.ActiveBuffer)
	}
	if !scr.AltPresent {
		t.Error("at vim_opened: alt buffer should be present")
	}

	afterOff, ok := m.offset("shell_after")
	if !ok {
		t.Fatal("shell_after checkpoint missing")
	}
	scr2 := replayPrefix(t, rawPath, afterOff, m)
	if scr2.ActiveBuffer != attachwire.BufferPrimary {
		t.Errorf("at shell_after: expected primary buffer after :q!, got buffer %d", scr2.ActiveBuffer)
	}
	_ = raw
}

// TestRecordedTmuxReference replays the tmux recordings up to attached_redraw and
// compares each pane's sub-rectangle against its `tmux capture-pane -p -e` ground
// truth, per-cell text, pass bar = 0 mismatches — the recorded-fixture
// snapshot-correctness gate, driven through the public Session.
func TestRecordedTmuxReference(t *testing.T) {
	moduleDir := donmaiModuleDir(t)
	for _, name := range []string{"tmux_vim", "tmux_split"} {
		t.Run(name, func(t *testing.T) {
			_, m := loadRecordedFixture(t, moduleDir, name)
			assertFixtureMeta(t, m, name)
			if m.Reference == nil || len(m.Reference.Panes) == 0 {
				t.Fatalf("%s: no tmux reference in sidecar", name)
			}
			off, ok := m.offset("attached_redraw")
			if !ok {
				t.Fatalf("%s: attached_redraw checkpoint missing", name)
			}
			rawPath := filepath.Join(moduleDir, "ptyhost", "testdata", name+".raw")
			scr := replayPrefix(t, rawPath, off, m)

			if scr.ActiveBuffer != attachwire.BufferAlt || !scr.AltPresent {
				t.Fatalf("%s: expected alt-active screen with alt grid present, got buffer=%d altPresent=%t",
					name, scr.ActiveBuffer, scr.AltPresent)
			}
			grid := scr.Alt
			cols := int(scr.Cols) //nolint:gosec // G115: VT grid width from a small fixture terminal

			total := 0
			for _, p := range m.Reference.Panes {
				capLines := strings.Split(stripSGR(p.CaptureE), "\n")
				mism := 0
				for j := 0; j < p.Height; j++ {
					got := gridRowText(grid, cols, p.Left, p.Top+j, p.Width)
					want := ""
					if j < len(capLines) {
						want = trimRightSpaces(capLines[j])
					}
					if got != want {
						mism++
						if mism <= 3 {
							t.Errorf("%s pane %s row %d text mismatch:\n  vt  |%s|\n  tmux|%s|", name, p.ID, j, got, want)
						}
					}
				}
				total += mism
				if mism == 0 {
					t.Logf("%s pane %s: %d rows, 0 mismatch", name, p.ID, p.Height)
				}
				if p.Active {
					wantX, wantY := p.Left+p.CursorX, p.Top+p.CursorY
					if int(scr.CursorCol) != wantX || int(scr.CursorRow) != wantY { //nolint:gosec // G115: cursor coords clamped to the VT grid
						t.Errorf("%s active pane %s cursor = (%d,%d), want (%d,%d)",
							name, p.ID, scr.CursorCol, scr.CursorRow, wantX, wantY)
					}
				}
			}
			if total != 0 {
				t.Errorf("%s: %d total text mismatches (pass bar = 0)", name, total)
			}
		})
	}
}

func assertFixtureMeta(t *testing.T, m fixtureMeta, wantName string) {
	t.Helper()
	if m.Name != wantName {
		t.Errorf("fixture name = %q, want %q", m.Name, wantName)
	}
	if strings.TrimSpace(m.Description) == "" {
		t.Errorf("%s: fixture description is empty (name+description+cmd fixture contract)", wantName)
	}
	if len(m.Cmd) == 0 {
		t.Errorf("%s: fixture cmd is empty", wantName)
	}
}

// ---- serialized-grid text extraction (mirrors donmai vt_test.go policy) ------

// gridRowText reconstructs row y over columns [left, left+width), honoring
// wide-glyph continuation cells and right-trimming trailing blanks.
func gridRowText(grid []attachwire.Cell, cols, left, y, width int) string {
	var b strings.Builder
	for x := left; x < left+width && x < cols; x++ {
		idx := y*cols + x
		if idx < 0 || idx >= len(grid) {
			break
		}
		c := grid[idx]
		if c.Style&attachwire.StyleWideContinuation != 0 {
			continue
		}
		if len(c.RuneBytes) == 0 {
			b.WriteByte(' ')
			continue
		}
		b.Write(c.RuneBytes)
	}
	return trimRightSpaces(b.String())
}

// stripSGR removes ANSI SGR/escape sequences from a capture-pane -e string,
// leaving per-cell text.
func stripSGR(s string) string {
	var b strings.Builder
	i := 0
	for i < len(s) {
		if s[i] == 0x1b {
			j := i + 1
			switch {
			case j < len(s) && s[j] == '[':
				j++
				for j < len(s) && (s[j] < 0x40 || s[j] > 0x7e) {
					j++
				}
				j++
			case j < len(s) && s[j] == ']':
				for j < len(s) && s[j] != 0x07 {
					j++
				}
				j++
			default:
				j++
			}
			i = j
			continue
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}

func trimRightSpaces(s string) string { return strings.TrimRight(s, " \t\x00") }

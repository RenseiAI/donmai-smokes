package smokes

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	vt "github.com/charmbracelet/x/vt"
	"github.com/creack/pty"

	afh "github.com/RenseiAI/donmai-smokes/harness"
)

const (
	watchColumns = 120
	watchRows    = 36
)

type watchCapture struct {
	columns int
	mu      sync.Mutex
	raw     bytes.Buffer
	notify  chan struct{}
}

func (c *watchCapture) append(data []byte) {
	c.mu.Lock()
	_, _ = c.raw.Write(data)
	c.mu.Unlock()
	select {
	case c.notify <- struct{}{}:
	default:
	}
}

func (c *watchCapture) snapshot() []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	return bytes.Clone(c.raw.Bytes())
}

// watchScreen interprets the PTY transcript as a terminal does and returns the
// rows of the current screen. Searching the accumulated transcript would let a
// stale frame satisfy a check after a split or selection key. The renderer
// repaints with scroll regions (set region, scroll up/down inside it), so the
// transcript is replayed through a terminal emulator that implements them
// rather than a hand-rolled cursor model, which silently ignores them and
// reads the screen as it was before the scroll.
func watchScreen(raw []byte, columns int) []string {
	terminal := vt.NewEmulator(columns, watchRows)
	readerDone := make(chan struct{})
	go func() {
		_, _ = io.Copy(io.Discard, terminal)
		close(readerDone)
	}()
	_, _ = terminal.Write(raw)
	lines := make([]string, terminal.Height())
	for row := range lines {
		var line strings.Builder
		for column := 0; column < terminal.Width(); column++ {
			cell := terminal.CellAt(column, row)
			if cell == nil {
				line.WriteByte(' ')
			} else if cell.Width > 0 {
				line.WriteString(cell.Content)
			}
		}
		lines[row] = strings.TrimRight(line.String(), " ")
	}
	// Read and Close in this dependency share an unsynchronized closed flag.
	// Stop the pipe reader first, then close the now-single-owner emulator.
	_ = terminal.InputPipe().(io.Closer).Close()
	<-readerDone
	_ = terminal.Close()
	return lines
}

func watchRow(lines []string, marker string) int {
	for row, line := range lines {
		if strings.Contains(line, marker) {
			return row
		}
	}
	return -1
}

func waitWatchScreen(c *watchCapture, predicate func([]string) bool) ([]string, bool) {
	deadline := time.NewTimer(8 * time.Second)
	defer deadline.Stop()
	for {
		lines := watchScreen(c.snapshot(), c.columns)
		if predicate(lines) {
			return lines, true
		}
		select {
		case <-c.notify:
		case <-deadline.C:
			return watchScreen(c.snapshot(), c.columns), false
		}
	}
}

// The identity assertions require the full host and optional project label.
// Size that scenario to fit those labels beside the fixture's counters rather
// than depending on the runner's hostname fitting a fixed 120-column header.
// Hostname bytes conservatively bound display cells without a new dependency.
func watchFixtureColumns(host, scope string) int {
	identityColumns := len(host)
	if scope != "" {
		identityColumns += len(" · " + scope)
	}
	counterColumns := len("1 running   queue 0   slots 2/8   uptime 1m 30s   v0.72.26")
	const paddingAndGap = 3 // one padding cell per side and one gap
	return max(watchColumns, identityColumns+counterColumns+paddingAndGap)
}

// Hostnames and the fixture counters are ASCII, so bytes give a conservative
// display-cell budget without adding a terminal-width dependency. The styled
// header keeps two padding cells and one gap before the counter group.
func watchQueueFitsCompleteHost(host string, columns int) bool {
	const paddingAndGap = 3
	const primaryAndQueue = "2 running   queue 0"
	return len(host)+paddingAndGap+len(primaryAndQueue) <= columns
}

func startWatchPTY(t *testing.T, binary, cwd, home, daemonURL string, columns int, args ...string) (*os.File, *watchCapture, func()) {
	t.Helper()
	cmd := exec.Command(binary, append([]string{"host", "watch"}, args...)...) //nolint:gosec // compiled SUT and fixed local fixture args
	cmd.Dir = cwd
	cmd.Env = []string{
		"HOME=" + home, "DONMAI_STATE_HOME=" + home,
		"DONMAI_DAEMON_URL=" + daemonURL,
		"XDG_CONFIG_HOME=" + filepath.Join(home, ".config"),
		"PATH=/usr/bin:/bin", "LANG=C.UTF-8", "TERM=xterm", "NO_COLOR=1",
	}
	if columns < 1 || columns > 65535 {
		t.Fatalf("invalid host-watch fixture width: %d", columns)
	}
	t.Logf("host-watch PTY fixture columns=%d", columns)
	terminal, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: watchRows, Cols: uint16(columns)})
	if err != nil {
		t.Fatalf("start compiled host watch in PTY: %v", err)
	}
	capture := &watchCapture{columns: columns, notify: make(chan struct{}, 1)}
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		buf := make([]byte, 8192)
		for {
			n, readErr := terminal.Read(buf)
			if n > 0 {
				capture.append(buf[:n])
			}
			if readErr != nil {
				return
			}
		}
	}()
	waitDone := make(chan error, 1)
	go func() { waitDone <- cmd.Wait() }()
	joined := false
	t.Cleanup(func() {
		if !joined {
			if err := cmd.Process.Signal(syscall.SIGTERM); err != nil && !errors.Is(err, os.ErrProcessDone) {
				t.Errorf("signal exact host-watch PID %d: %v", cmd.Process.Pid, err)
			}
			select {
			case <-waitDone:
				joined = true
			case <-time.After(5 * time.Second):
				_ = cmd.Process.Kill() // exact owned child only
				<-waitDone
				t.Error("host watch required exact-PID kill after SIGTERM")
			}
		}
		_ = terminal.Close()
		select {
		case <-readerDone:
		case <-time.After(5 * time.Second):
			t.Error("host-watch PTY reader did not join")
		}
	})
	quit := func() {
		t.Helper()
		if _, err := terminal.Write([]byte("q")); err != nil {
			t.Fatalf("send quit key to host watch: %v", err)
		}
		select {
		case err := <-waitDone:
			joined = true
			if err != nil {
				t.Fatalf("host watch did not exit cleanly on q: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("host watch did not exit on q")
		}
	}
	return terminal, capture, quit
}

// TestHostWatchLayoutFromCompiledCLI verifies the actual OSS command against
// read-only localhost daemon responses. The PTY screen is reconstructed after
// each key so old frames cannot satisfy later layout assertions.
func TestHostWatchLayoutFromCompiledCLI(t *testing.T) {
	afh.SkipIfShort(t, "compiled interactive host-watch layout smoke")
	afh.SkipIfKnob(t, afh.SkipLiveDaemonEnv, "operator opted out of live-process smokes")
	source := afh.RequireDonmaiSourceAt(t, inFlightSourceDir())
	afh.SkipIfToolMissing(t, "git", "default host-watch scope reads the current repository remote")
	binary, source := afh.RequireDonmaiBinary(t, afh.LiveBinaryOptions{SourceDir: source, Timeout: 8 * time.Minute})
	t.Logf("compiled host watch from %s", source)
	root := t.TempDir()
	home := filepath.Join(root, "home")
	cwd := filepath.Join(root, "checkout")
	for _, path := range []string{home, cwd} {
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	git := exec.Command("git", "init", "--quiet", cwd) //nolint:gosec // fixed local fixture directory
	if output, err := git.CombinedOutput(); err != nil {
		t.Fatalf("initialize scope fixture: %v: %s", err, output)
	}
	remote := exec.Command("git", "-C", cwd, "remote", "add", "origin", "https://example.invalid/fixture/alpha.git") //nolint:gosec // no network request
	if output, err := remote.CombinedOutput(); err != nil {
		t.Fatalf("configure scope fixture: %v: %s", err, output)
	}
	type handle struct {
		SessionID    string `json:"sessionId"`
		IssueID      string `json:"issueIdentifier"`
		PID          int    `json:"pid"`
		State        string `json:"state"`
		AcceptedAt   string `json:"acceptedAt"`
		WorktreePath string `json:"worktreePath"`
		ProjectName  string `json:"projectName"`
		Repository   string `json:"repository"`
		Harness      string `json:"harness"`
		Model        string `json:"model"`
		Provider     string `json:"modelProvider"`
		WorkType     string `json:"workType"`
	}
	// Cards are named by issue id, so the fixture sessions carry one.
	const issueA, issueB = "FIX-101", "FIX-202"
	sessions := []handle{
		{SessionID: "watch-a-0001", IssueID: issueA, PID: 9101, State: "running", AcceptedAt: "2026-09-28T00:00:00Z", WorktreePath: filepath.Join(root, "sessions", "watch-a-0001"), ProjectName: "alpha", Repository: "fixture/alpha", Harness: "fx", Model: "mini", Provider: "local", WorkType: "development"},
		{SessionID: "watch-b-0002", IssueID: issueB, PID: 9102, State: "running", AcceptedAt: "2026-09-28T00:00:00Z", WorktreePath: filepath.Join(root, "sessions", "watch-b-0002"), ProjectName: "beta", Repository: "fixture/beta", Harness: "fx", Model: "mini", Provider: "local", WorkType: "qa"},
	}
	for _, session := range sessions {
		if err := os.MkdirAll(filepath.Join(session.WorktreePath, ".agent"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(session.WorktreePath, ".agent", "events.jsonl"), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	var callsMu sync.Mutex
	var calls []string
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callsMu.Lock()
		calls = append(calls, r.Method+" "+r.URL.Path)
		callsMu.Unlock()
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		var body any
		switch r.URL.Path {
		case "/api/daemon/sessions":
			body = sessions
		case "/api/daemon/status":
			body = map[string]any{"status": "ready", "version": "0.72.26", "activeSessions": 2, "maxSessions": 8, "uptimeSeconds": 90, "pid": 12345}
		case "/api/daemon/stats":
			body = map[string]any{"activeSessions": 2, "queueDepth": 0, "capacity": map[string]any{}}
		default:
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(body); err != nil {
			t.Errorf("encode local daemon fixture: %v", err)
		}
	}))
	defer daemon.Close()
	host, err := os.Hostname()
	if err != nil {
		t.Fatal(err)
	}
	hostRunes := []rune(host)
	if len(hostRunes) == 0 {
		hostRunes = []rune("host")
	}
	visibleHostPrefix := string(hostRunes[:min(6, len(hostRunes))])
	t.Run("header fits narrow and wider viewports", func(t *testing.T) {
		tests := []struct {
			name       string
			columns    int
			args       []string
			running    string
			fullHeader bool
		}{
			{name: "narrow_40", columns: 40, args: []string{"--all"}, running: "2 running"},
			{name: "medium_80", columns: 80, args: []string{"--all"}, running: "2 running"},
			{name: "wide_context_fits", columns: watchFixtureColumns(host, "fixture/alpha") + 8, running: "1 running", fullHeader: true},
		}
		for _, tc := range tests {
			t.Run(tc.name, func(t *testing.T) {
				_, capture, quit := startWatchPTY(t, binary, cwd, home, daemon.URL, tc.columns, tc.args...)
				lines, ok := waitWatchScreen(capture, func(lines []string) bool {
					return watchRow(lines, issueA) >= 0 && watchRow(lines, tc.running) >= 0
				})
				if !ok {
					t.Fatalf("host header and session did not render at width %d: %q", tc.columns, lines[:3])
				}
				header := strings.TrimSpace(lines[0])
				if !strings.HasPrefix(header, visibleHostPrefix) {
					t.Errorf("width %d header lost visible host prefix %q: %q", tc.columns, visibleHostPrefix, lines[0])
				}
				if got := len([]rune(lines[0])); got > tc.columns {
					t.Errorf("width %d header occupies %d cells: %q", tc.columns, got, lines[0])
				}
				if watchRow(lines, tc.running) != 0 {
					t.Errorf("width %d header lost the primary running counter from row zero: %q", tc.columns, lines[:3])
				}
				queueRow := watchRow(lines, "queue 0")
				queueFits := watchQueueFitsCompleteHost(host, tc.columns)
				t.Logf("queue capacity witness: host_cells=%d width=%d queue_fits=%t", len(host), tc.columns, queueFits)
				if queueFits && queueRow != 0 {
					t.Errorf("width %d header omitted queue counter despite complete host and primary/queue counters fitting: %q", tc.columns, lines[:3])
				}
				if !queueFits && queueRow >= 0 {
					t.Errorf("width %d header showed queue counter although complete host and primary/queue counters do not fit: row=%d screen=%q", tc.columns, queueRow, lines[:3])
				}
				for row, line := range lines[1:] {
					if strings.Contains(line, "uptime") || strings.Contains(line, "v0.72.26") || strings.Contains(line, "72.26") {
						t.Errorf("width %d header continued onto viewport row %d: %q", tc.columns, row+1, line)
					}
				}
				if tc.fullHeader {
					for _, field := range []string{host, "fixture/alpha", "queue 0", "uptime", "v0.72.26"} {
						if !strings.Contains(lines[0], field) {
							t.Errorf("fitting width %d header omitted %q: %q", tc.columns, field, lines[0])
						}
					}
				}
				quit()
			})
		}
	})
	screenOf := func(c *watchCapture) func() []string {
		return func() []string { return watchScreen(c.snapshot(), c.columns) }
	}
	t.Run("default project scope", func(t *testing.T) {
		terminal, capture, quit := startWatchPTY(t, binary, cwd, home, daemon.URL, watchFixtureColumns(host, "fixture/alpha"))
		lines, ok := waitWatchScreen(capture, func(lines []string) bool {
			_, found := watchCardAt(lines, issueA)
			return found && watchRow(lines, "1 running") >= 0
		})
		if !ok {
			t.Fatalf("default scope never rendered: %q", lines)
		}
		if !strings.Contains(lines[0], host) || !strings.Contains(lines[0], "fixture/alpha") || strings.HasPrefix(strings.TrimSpace(lines[0]), "donmai") {
			t.Fatalf("header did not lead with host and scoped project: %q", lines[0])
		}
		if _, other := watchCardAt(lines, issueB); other || watchRow(lines, "beta · qa") >= 0 {
			t.Fatalf("default scope included other project: %q", lines)
		}
		card, _ := watchCardAt(lines, issueA)
		for row, want := range map[int]string{1: "alpha · development", 2: "mini · fx", 4: "activity not reported"} {
			if card.Rows[row] != want {
				t.Errorf("session card row %d = %q, want %q: %q", row, card.Rows[row], want, card.Rows)
			}
		}
		if !strings.HasPrefix(card.Rows[3], "running") {
			t.Errorf("session card state row = %q, want it to start with running", card.Rows[3])
		}
		// A card keeps to the essentials; the endpoint surface and the tool
		// count are reachable in the selected session's detail.
		if _, err := terminal.Write([]byte("\r")); err != nil {
			t.Fatal(err)
		}
		waitWatchDetail(t, capture, screenOf(capture), issueA,
			field("Harness", "fx"), field("Model", "mini"), field("Endpoint surface", "local"), field("Tools", "not reported"))
		if _, err := terminal.Write([]byte("\x1b")); err != nil {
			t.Fatal(err)
		}
		if closed, ok := waitWatchScreen(capture, func(next []string) bool {
			_, open := watchDetailID(next)
			return !open && watchPaneTitleRow(next) >= 0
		}); !ok {
			t.Fatalf("escape did not return the detail pane to the session stream: %q", closed)
		}
		quit()
	})
	t.Run("fleet grid split and selection", func(t *testing.T) {
		terminal, capture, quit := startWatchPTY(t, binary, cwd, home, daemon.URL, watchFixtureColumns(host, ""), "--all")
		lines, ok := waitWatchScreen(capture, func(lines []string) bool {
			_, foundA := watchCardAt(lines, issueA)
			_, foundB := watchCardAt(lines, issueB)
			return foundA && foundB
		})
		if !ok {
			t.Fatalf("fleet view never rendered: %q", lines)
		}
		alpha, _ := watchCardAt(lines, issueA)
		beta, _ := watchCardAt(lines, issueB)
		if alpha.Row != beta.Row || alpha.Col >= beta.Col {
			t.Fatalf("cards from separate projects did not flow across one grid row: alpha=(%d,%d) beta=(%d,%d) screen=%q", alpha.Row, alpha.Col, beta.Row, beta.Col, lines)
		}
		for _, tc := range []struct {
			name    string
			card    watchCardView
			context string
		}{{issueA, alpha, "alpha · development"}, {issueB, beta, "beta · qa"}} {
			if tc.card.Rows[1] != tc.context || tc.card.Rows[2] != "mini · fx" {
				t.Errorf("card %s project/model rows = %q / %q, want %q / %q", tc.name, tc.card.Rows[1], tc.card.Rows[2], tc.context, "mini · fx")
			}
		}
		if !strings.Contains(lines[0], host) || strings.Contains(lines[0], "all projects") || strings.HasPrefix(strings.TrimSpace(lines[0]), "donmai") {
			t.Fatalf("fleet header did not lead with host: %q", lines[0])
		}
		if watchRow(lines, "2 running") != 0 {
			t.Errorf("fleet header omitted the session count: %q", lines[0])
		}
		if watchRow(lines, "slots 2/8") != 0 {
			t.Errorf("fleet header omitted the host's session slots: %q", lines[0])
		}
		// The help line documents every key this step presses.
		for _, key := range []string{"jk select", "enter detail", "[ ] split", "0 reset", "g tail", "q quit"} {
			if !strings.Contains(lines[watchRows-1], key) {
				t.Errorf("help line missing %q: %q", key, lines[watchRows-1])
			}
		}
		if !alpha.Selected() || !beta.Unselected() {
			t.Fatalf("the first card should open selected, with a heavy frame and ▸ (corner %q marker %q), the second plain (corner %q marker %q)", alpha.Corner, alpha.Marker, beta.Corner, beta.Marker)
		}

		initial := watchPaneTitleRow(lines)
		if initial < 0 {
			t.Fatalf("fleet view has no stream title row: %q", lines)
		}
		press := func(key string) {
			t.Helper()
			if _, err := terminal.Write([]byte(key)); err != nil {
				t.Fatal(err)
			}
		}
		waitPane := func(what string, ok func(row int) bool) {
			t.Helper()
			screen, found := waitWatchScreen(capture, func(next []string) bool {
				row := watchPaneTitleRow(next)
				return row >= 0 && ok(row)
			})
			if !found {
				raw := capture.snapshot()
				if len(raw) > 600 {
					raw = raw[len(raw)-600:]
				}
				t.Fatalf("%s: stream title row is %d, was %d initially; raw tail=%q", what, watchPaneTitleRow(screen), initial, raw)
			}
		}
		// ] gives the grid more rows, so the stream title moves down; [ moves
		// it up; 0 restores the default split.
		press("]")
		waitPane("split key did not move the stream pane down", func(row int) bool { return row > initial })
		press("0")
		waitPane("split reset did not restore the stream pane", func(row int) bool { return row == initial })
		press("[")
		waitPane("split key did not move the stream pane up", func(row int) bool { return row < initial })
		press("0")
		waitPane("split reset did not restore the stream pane", func(row int) bool { return row == initial })

		press("j")
		var movedA, movedB watchCardView
		if screen, ok := waitWatchScreen(capture, func(next []string) bool {
			var foundA, foundB bool
			movedA, foundA = watchCardAt(next, issueA)
			movedB, foundB = watchCardAt(next, issueB)
			return foundA && foundB && movedA.Unselected() && movedB.Selected()
		}); !ok {
			t.Fatalf("heavy frame and ▸ did not move to the second card: %q", screen[1:4])
		}
		// Selection changes the frame, never a card's place.
		if movedA.Row != alpha.Row || movedA.Col != alpha.Col || movedB.Row != beta.Row || movedB.Col != beta.Col {
			t.Errorf("selection moved a card: alpha (%d,%d)->(%d,%d) beta (%d,%d)->(%d,%d)", alpha.Row, alpha.Col, movedA.Row, movedA.Col, beta.Row, beta.Col, movedB.Row, movedB.Col)
		}
		quit()
	})
	callsMu.Lock()
	observed := append([]string(nil), calls...)
	callsMu.Unlock()
	if len(observed) < 6 {
		t.Fatalf("host watch did not poll sessions/status/stats for both views: %v", observed)
	}
	allowed := map[string]bool{"GET /api/daemon/sessions": true, "GET /api/daemon/status": true, "GET /api/daemon/stats": true}
	seen := map[string]bool{}
	for _, call := range observed {
		if !allowed[call] {
			t.Fatalf("host watch made non-read or unexpected daemon request %q", call)
		}
		seen[call] = true
	}
	for call := range allowed {
		if !seen[call] {
			t.Errorf("host watch did not call %s", call)
		}
	}
	t.Logf("compiled CLI rendered current PTY frames and made %d local GET calls", len(observed))
}

package smokes

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

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

// watchScreen applies the terminal's cursor/erase operations to one current
// screen. Searching the accumulated PTY transcript would let a stale frame
// satisfy a check after a split or selection key.
func watchScreen(raw []byte, columns int) []string {
	cells := make([][]rune, watchRows)
	for row := range cells {
		cells[row] = []rune(strings.Repeat(" ", columns))
	}
	x, y := 0, 0
	clamp := func(value, maximum int) int {
		if value < 0 {
			return 0
		}
		if value >= maximum {
			return maximum - 1
		}
		return value
	}
	scroll := func() {
		copy(cells, cells[1:])
		cells[watchRows-1] = []rune(strings.Repeat(" ", columns))
		y = watchRows - 1
	}
	runes := []rune(string(raw))
	for i := 0; i < len(runes); i++ {
		r := runes[i]
		if r == '\x1b' && i+1 < len(runes) {
			i++
			switch runes[i] {
			case '[':
				start := i + 1
				for i+1 < len(runes) && (runes[i+1] < '@' || runes[i+1] > '~') {
					i++
				}
				if i+1 >= len(runes) {
					break
				}
				params := string(runes[start : i+1])
				i++
				parts := strings.Split(params, ";")
				num := func(index, fallback int) int {
					if index >= len(parts) {
						return fallback
					}
					n, err := strconv.Atoi(parts[index])
					if err != nil || n == 0 {
						return fallback
					}
					return n
				}
				switch runes[i] {
				case 'H', 'f':
					y, x = clamp(num(0, 1)-1, watchRows), clamp(num(1, 1)-1, columns)
				case 'd':
					y = clamp(num(0, 1)-1, watchRows)
				case 'G':
					x = clamp(num(0, 1)-1, columns)
				case 'A':
					y = clamp(y-num(0, 1), watchRows)
				case 'B':
					y = clamp(y+num(0, 1), watchRows)
				case 'C':
					x = clamp(x+num(0, 1), columns)
				case 'D':
					x = clamp(x-num(0, 1), columns)
				case 'J':
					if num(0, 0) == 2 {
						for row := range cells {
							cells[row] = []rune(strings.Repeat(" ", columns))
						}
					}
				case 'K':
					if num(0, 0) == 1 {
						for col := 0; col <= x; col++ {
							cells[y][col] = ' '
						}
					} else {
						for col := x; col < columns; col++ {
							cells[y][col] = ' '
						}
					}
				case 'X':
					for col := x; col < x+num(0, 1) && col < columns; col++ {
						cells[y][col] = ' '
					}
				case 'M':
					for range num(0, 1) {
						copy(cells[y:], cells[y+1:])
						cells[watchRows-1] = []rune(strings.Repeat(" ", columns))
					}
				case 'L':
					for range num(0, 1) {
						copy(cells[y+1:], cells[y:watchRows-1])
						cells[y] = []rune(strings.Repeat(" ", columns))
					}
				}
			case ']':
				for i+1 < len(runes) {
					if runes[i+1] == '\a' || (runes[i+1] == '\x1b' && i+2 < len(runes) && runes[i+2] == '\\') {
						break
					}
					i++
				}
				if i+1 < len(runes) && runes[i+1] == '\x1b' {
					i += 2
				} else if i+1 < len(runes) {
					i++
				}
			case 'M':
				y = clamp(y-1, watchRows)
			}
			continue
		}
		switch r {
		case '\r':
			x = 0
		case '\n':
			y++
			if y >= watchRows {
				scroll()
			}
		case '\b':
			x = clamp(x-1, columns)
		default:
			if r < ' ' || r == '\x7f' {
				continue
			}
			if x >= columns {
				x = 0
				y++
				if y >= watchRows {
					scroll()
				}
			}
			cells[y][x] = r
			x++
		}
	}
	lines := make([]string, watchRows)
	for row := range cells {
		lines[row] = strings.TrimRight(string(cells[row]), " ")
	}
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
	counterColumns := len("1 running   queue 0   uptime 1m 30s   v0.72.26")
	const paddingAndGap = 3 // one padding cell per side and one gap
	return max(watchColumns, identityColumns+counterColumns+paddingAndGap)
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
	sessions := []handle{
		{SessionID: "watch-a-0001", PID: 9101, State: "running", AcceptedAt: "2026-09-28T00:00:00Z", WorktreePath: filepath.Join(root, "sessions", "watch-a-0001"), ProjectName: "alpha", Repository: "fixture/alpha", Harness: "fx", Model: "mini", Provider: "local", WorkType: "development"},
		{SessionID: "watch-b-0002", PID: 9102, State: "running", AcceptedAt: "2026-09-28T00:00:00Z", WorktreePath: filepath.Join(root, "sessions", "watch-b-0002"), ProjectName: "beta", Repository: "fixture/beta", Harness: "fx", Model: "mini", Provider: "local", WorkType: "qa"},
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
					return watchRow(lines, "watch-a-") >= 0 && watchRow(lines, tc.running) >= 0
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
				if watchRow(lines, tc.running) != 0 || watchRow(lines, "queue 0") != 0 {
					t.Errorf("width %d header and counters did not stay on one visible row: %q", tc.columns, lines[:3])
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
	t.Run("default project scope", func(t *testing.T) {
		_, capture, quit := startWatchPTY(t, binary, cwd, home, daemon.URL, watchFixtureColumns(host, "fixture/alpha"))
		lines, ok := waitWatchScreen(capture, func(lines []string) bool {
			return watchRow(lines, "watch-a-") >= 0 && watchRow(lines, "1 running") >= 0
		})
		if !ok {
			t.Fatalf("default scope never rendered: %q", lines)
		}
		if !strings.Contains(lines[0], host) || !strings.Contains(lines[0], "fixture/alpha") || strings.HasPrefix(strings.TrimSpace(lines[0]), "donmai") {
			t.Fatalf("header did not lead with host and scoped project: %q", lines[0])
		}
		if watchRow(lines, "project beta") >= 0 || watchRow(lines, "watch-b-") >= 0 {
			t.Fatalf("default scope included other project: %q", lines)
		}
		for _, field := range []string{"project alpha", "harness fx", "model mini", "provider local", "state running", "tools not reported"} {
			if watchRow(lines, field) < 0 {
				t.Errorf("session card missing %q: %q", field, lines)
			}
		}
		quit()
	})
	t.Run("fleet grid split and selection", func(t *testing.T) {
		terminal, capture, quit := startWatchPTY(t, binary, cwd, home, daemon.URL, watchFixtureColumns(host, ""), "--all")
		lines, ok := waitWatchScreen(capture, func(lines []string) bool {
			return watchRow(lines, "watch-a-") >= 0 && watchRow(lines, "watch-b-") >= 0 && watchRow(lines, "FOLLOW") >= 0
		})
		if !ok {
			t.Fatalf("fleet view never rendered: %q", lines)
		}
		alpha, beta := watchRow(lines, "watch-a-"), watchRow(lines, "watch-b-")
		if beta-alpha < -1 || beta-alpha > 1 {
			t.Fatalf("cards from separate projects did not share a grid row: alpha=%d beta=%d screen=%q", alpha, beta, lines)
		}
		if watchRow(lines, "project alpha") < 0 || watchRow(lines, "project beta") < 0 {
			t.Fatalf("fleet cards omitted their project scope: %q", lines)
		}
		if !strings.Contains(lines[0], host) || strings.Contains(lines[0], "all projects") || strings.HasPrefix(strings.TrimSpace(lines[0]), "donmai") {
			t.Fatalf("fleet header did not lead with host: %q", lines[0])
		}
		initialFollow := watchRow(lines, "FOLLOW")
		if _, err := terminal.Write([]byte("]")); err != nil {
			t.Fatal(err)
		}
		split, ok := waitWatchScreen(capture, func(next []string) bool {
			row := watchRow(next, "FOLLOW")
			return row >= 0 && row < initialFollow
		})
		if !ok {
			t.Fatalf("split key did not move stream viewport upward: before=%d after=%d", initialFollow, watchRow(split, "FOLLOW"))
		}
		if _, err := terminal.Write([]byte("0")); err != nil {
			t.Fatal(err)
		}
		reset, ok := waitWatchScreen(capture, func(next []string) bool { return watchRow(next, "FOLLOW") == initialFollow })
		if !ok {
			raw := capture.snapshot()
			if len(raw) > 600 {
				raw = raw[len(raw)-600:]
			}
			t.Fatalf("split reset did not restore stream viewport: before=%d after=%d raw tail=%q", initialFollow, watchRow(reset, "FOLLOW"), raw)
		}
		if _, err := terminal.Write([]byte("j")); err != nil {
			t.Fatal(err)
		}
		selected, ok := waitWatchScreen(capture, func(next []string) bool {
			for _, line := range next[1:4] {
				if col := strings.Index(line, "╭"); col > 20 {
					return true
				}
			}
			return false
		})
		if !ok {
			t.Fatalf("selection border did not move to second card: %q", selected[1:4])
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

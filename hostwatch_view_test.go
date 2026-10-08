package smokes

// hostwatch_view_test.go — readers for the host-watch screen.
//
// The host-watch smokes drive the compiled CLI and read what it paints. The
// view has a fixed shape that these helpers rely on, and nothing else:
//
//   - the first row is the host header (host name, then the session counter);
//   - every session is a card of exactly five rows: status dot and issue id
//     (then the title), repository and work type, model and harness, state
//     and elapsed time (then turns and cost once reported), and the age and
//     text of the last activity;
//   - the selected card has a heavy frame and a `▸` in the cell left of its
//     status dot, so selection reads without color; every other card has the
//     same one-cell rounded frame, and selection never moves a card;
//   - the pane under the grid is titled `session stream` or, while a session's
//     detail is open, `session detail · <id>`, and lists `label  value` rows;
//   - the last row is the help line.
//
// Asserting on those facts, rather than on whole rendered rows, keeps a smoke
// from breaking when spacing, truncation or wording inside a row shifts.

import (
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/charmbracelet/x/ansi"
)

// watchCardRows is the number of rows every card occupies inside its frame.
const watchCardRows = 5

// watchStatusDots are the glyphs a card's status dot cycles through.
const watchStatusDots = "●◉○"

// watchCardView is one session card read back from a screen.
type watchCardView struct {
	// Row and Col locate the issue id on the card's first row, in terminal
	// cells. Two renders of the same card must report the same Row and Col
	// whatever is selected.
	Row, Col int
	// Rows holds the five content rows, trimmed: identity, context
	// (repository and work type), model and harness, state, activity.
	Rows [watchCardRows]string
	// Corner is the frame's top-left corner above the card: `╭` for an
	// unselected card, `┏` for the selected one. Zero when unframed.
	Corner rune
	// Marker is the cell left of the status dot: `▸` when selected.
	Marker rune
}

// Selected reports whether the card carries both selection cues.
func (c watchCardView) Selected() bool { return c.Corner == '┏' && c.Marker == '▸' }

// Unselected reports whether the card carries neither selection cue.
func (c watchCardView) Unselected() bool { return c.Corner == '╭' && c.Marker == ' ' }

func isWatchDot(cell string) bool {
	return cell != "" && utf8.RuneCountInString(cell) == 1 && strings.Contains(watchStatusDots, cell)
}

func isWatchBorder(cell string) bool { return cell == "│" || cell == "┃" }

// watchCells splits a screen row into one entry per terminal cell. A wide
// rune takes two cells, the second empty, so an index into the result is a
// column however many wide neighbours (another card's CJK title, say) share
// the row. Combining marks ride on the cell before them.
func watchCells(line string) []string {
	var cells []string
	for _, r := range line {
		switch width := ansi.StringWidth(string(r)); {
		case width == 0 && len(cells) > 0:
			cells[len(cells)-1] += string(r)
		case width == 2:
			cells = append(cells, string(r), "")
		default:
			cells = append(cells, string(r))
		}
	}
	return cells
}

func firstRune(cell string) rune {
	r, _ := utf8.DecodeRuneInString(cell)
	return r
}

// watchPaneTitleRow is the first row of the pane under the card grid, or -1
// when the screen has none. Rows from it on are stream or detail text, which
// may mention an issue id without being a card.
func watchPaneTitleRow(lines []string) int {
	for row, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "session stream") || strings.HasPrefix(trimmed, "session detail") {
			return row
		}
	}
	return -1
}

// watchCardAt finds the framed card whose first row carries id as a whole
// word right after its status dot. Only rows above the pane title are
// searched. Columns are terminal cells.
func watchCardAt(lines []string, id string) (watchCardView, bool) {
	limit := watchPaneTitleRow(lines)
	if limit < 0 {
		limit = len(lines)
	}
	idCells := len(watchCells(id))
	for row := 0; row < limit && row+watchCardRows <= len(lines); row++ {
		cells := watchCells(lines[row])
		// A cell index is a candidate when the id's text starts right there.
		for col := 4; col+idCells <= len(cells); col++ {
			if strings.Join(cells[col:col+idCells], "") != id {
				continue
			}
			if !isWatchDot(cells[col-2]) || cells[col-1] != " " {
				continue
			}
			if end := col + idCells; end < len(cells) && cells[end] != " " && !isWatchBorder(cells[end]) {
				continue // a longer id that merely starts with this one
			}
			view := watchCardView{Row: row, Col: col, Marker: firstRune(cells[col-3])}
			if row > 0 {
				if above := watchCells(lines[row-1]); col-4 < len(above) {
					view.Corner = firstRune(above[col-4])
				}
			}
			for i := range view.Rows {
				body := watchCells(lines[row+i])
				start := min(col-2, len(body))
				stop := len(body)
				for j := start; j < len(body); j++ {
					if isWatchBorder(body[j]) {
						stop = j
						break
					}
				}
				view.Rows[i] = strings.TrimSpace(strings.Join(body[start:stop], ""))
			}
			return view, true
		}
	}
	return watchCardView{}, false
}

// watchPlainCard reads the freshest card for id from piped (--plain) output,
// which repeats the whole frame on every refresh. Plain rows carry no frame:
// the first starts with the selection marker (`> ` or two spaces), then the
// status dot and the id; the four after it are indented under the id.
func watchPlainCard(output, id string) ([watchCardRows]string, bool) {
	lines := strings.Split(output, "\n")
	for i := len(lines) - watchCardRows; i >= 0; i-- {
		head := strings.TrimPrefix(strings.TrimSpace(lines[i]), "> ")
		dot, size := utf8.DecodeRuneInString(head)
		if size == 0 || !strings.ContainsRune(watchStatusDots, dot) {
			continue
		}
		fields := strings.Fields(head[size:])
		if len(fields) == 0 || fields[0] != id {
			continue
		}
		var rows [watchCardRows]string
		for j := range rows {
			rows[j] = strings.TrimSpace(lines[i+j])
		}
		rows[0] = head
		return rows, true
	}
	return [watchCardRows]string{}, false
}

// watchDetailID returns the session named by the detail pane's title row
// (`session detail · <id>  esc close`).
func watchDetailID(lines []string) (string, bool) {
	for _, line := range lines {
		rest, ok := strings.CutPrefix(strings.TrimSpace(line), "session detail · ")
		if !ok {
			continue
		}
		if fields := strings.Fields(rest); len(fields) > 0 {
			return fields[0], true
		}
	}
	return "", false
}

// watchDetailValue returns the value shown for label in the open detail pane.
// A cell is its label padded to a fixed column, then the value; on a wide
// terminal a second cell follows after a gap of at least three spaces. A
// label only matches when padding follows it, so `Model` never reads the
// `Model identity` row.
func watchDetailValue(lines []string, label string) (string, bool) {
	title := watchPaneTitleRow(lines)
	if title < 0 {
		return "", false
	}
	cell := regexp.MustCompile(`(?:^| {3})` + regexp.QuoteMeta(label) + ` {2,}(\S.*?)(?: {2,}|$)`)
	for _, line := range lines[title+1:] {
		if m := cell.FindStringSubmatch(line); m != nil {
			return strings.TrimSpace(m[1]), true
		}
	}
	return "", false
}

// watchField is one expected `label  value` row of the detail pane.
type watchField struct{ Label, Value string }

func field(label, value string) watchField { return watchField{label, value} }

// watchFieldMismatches lists, as "label: got X, want Y", every expected field
// the detail pane does not show exactly.
func watchFieldMismatches(lines []string, want []watchField) []string {
	var bad []string
	for _, f := range want {
		got, ok := watchDetailValue(lines, f.Label)
		switch {
		case !ok:
			bad = append(bad, f.Label+": missing, want "+f.Value)
		case got != f.Value:
			bad = append(bad, f.Label+": got "+got+", want "+f.Value)
		}
	}
	return bad
}

// waitWatchDetail waits until the open detail pane belongs to id and shows
// every field exactly, then returns that screen.
func waitWatchDetail(t *testing.T, c *watchCapture, read func() []string, id string, want ...watchField) []string {
	t.Helper()
	var last []string
	var problems []string
	deadline := time.NewTimer(8 * time.Second)
	defer deadline.Stop()
	for {
		last = read()
		problems = problems[:0]
		if got, ok := watchDetailID(last); !ok || got != id {
			problems = append(problems, "detail pane is not open for "+id)
		} else {
			problems = append(problems, watchFieldMismatches(last, want)...)
		}
		if len(problems) == 0 {
			return last
		}
		select {
		case <-c.notify:
		case <-deadline.C:
			t.Fatalf("detail for %q never matched: %q\nscreen:\n%s", id, problems, strings.Join(last, "\n"))
		}
	}
}

// waitWatchCard waits for the card named id to render.
func waitWatchCard(t *testing.T, capture *watchCapture, id string) {
	t.Helper()
	if lines, ok := waitWatchScreen(capture, func(lines []string) bool {
		_, found := watchCardAt(lines, id)
		return found
	}); !ok {
		t.Fatalf("card %q never rendered:\n%s", id, strings.Join(lines, "\n"))
	}
}

// waitCardRow waits until row `row` (0-4) of the card named id satisfies
// accept, and fails with that row's last value otherwise.
func waitCardRow(t *testing.T, capture *watchCapture, id string, row int, describe string, accept func(string) bool) {
	t.Helper()
	got := "<card not on screen>"
	if lines, ok := waitWatchScreen(capture, func(lines []string) bool {
		card, found := watchCardAt(lines, id)
		if found {
			got = card.Rows[row]
		}
		return found && accept(card.Rows[row])
	}); !ok {
		t.Fatalf("card %q row %d is %q, want %s:\n%s", id, row, got, describe, strings.Join(lines, "\n"))
	}
}

// cardRowIs waits for an exact card row.
func cardRowIs(t *testing.T, capture *watchCapture, id string, row int, want string) {
	t.Helper()
	waitCardRow(t, capture, id, row, strconv.Quote(want), func(got string) bool { return got == want })
}

// watchTestFrame draws one card as the view frames it: a rounded frame, or a
// heavy one with ▸ in the cell before the first row when selected. inner is the
// content width in cells.
func watchTestFrame(selected bool, inner int, rows [watchCardRows]string) [watchCardRows + 2]string {
	topLeft, topRight, bottomLeft, bottomRight, horizontal, vertical := "╭", "╮", "╰", "╯", "─", "│"
	if selected {
		topLeft, topRight, bottomLeft, bottomRight, horizontal, vertical = "┏", "┓", "┗", "┛", "━", "┃"
	}
	var out [watchCardRows + 2]string
	out[0] = topLeft + strings.Repeat(horizontal, inner+2) + topRight
	for i, row := range rows {
		lead := " "
		if i == 0 && selected {
			lead = "▸"
		}
		out[i+1] = vertical + lead + row + strings.Repeat(" ", max(0, inner-ansi.StringWidth(row))) + " " + vertical
	}
	out[len(out)-1] = bottomLeft + strings.Repeat(horizontal, inner+2) + bottomRight
	return out
}

// watchTestScreen lays frames side by side under a header and above a stream
// pane, as the grid does.
func watchTestScreen(streamLines []string, frames ...[watchCardRows + 2]string) []string {
	lines := []string{" test-host   2 running   queue 0   slots 2/8"}
	for row := 0; row < watchCardRows+2; row++ {
		var parts []string
		for _, frame := range frames {
			parts = append(parts, frame[row])
		}
		lines = append(lines, strings.Join(parts, " "))
	}
	lines = append(lines, "", "session stream  [follow]")
	lines = append(lines, streamLines...)
	return append(lines, "↑↓/jk select   enter detail   [ ] split   0 reset   f follow/pause   g tail   q quit")
}

func TestWatchCardAt(t *testing.T) {
	t.Parallel()
	rowsOf := func(id, title string) [watchCardRows]string {
		return [watchCardRows]string{"● " + id + "  " + title, "web · development", "mini · fx", "running 3m · 2 turns · $0.50", "2s ago · Edit main.go"}
	}
	const inner = 34
	selected := watchTestFrame(true, inner, rowsOf("FIX-1", "first"))
	plain := watchTestFrame(false, inner, rowsOf("FIX-10", "日本語のタイトル"))
	stream := []string{"20:00:00 [FIX-2] tool_use  Edit main.go"}
	screen := watchTestScreen(stream, selected, plain)

	first, ok := watchCardAt(screen, "FIX-1")
	if !ok {
		t.Fatalf("FIX-1 not found:\n%s", strings.Join(screen, "\n"))
	}
	if first.Row != 2 || first.Col != 4 || !first.Selected() || first.Unselected() {
		t.Errorf("FIX-1 = row %d col %d corner %q marker %q, want row 2 col 4, heavy frame and ▸", first.Row, first.Col, first.Corner, first.Marker)
	}
	wantRows := [watchCardRows]string{"● FIX-1  first", "web · development", "mini · fx", "running 3m · 2 turns · $0.50", "2s ago · Edit main.go"}
	if first.Rows != wantRows {
		t.Errorf("FIX-1 rows = %q, want %q", first.Rows, wantRows)
	}

	// FIX-10 sits to the right of a card, with a wide-rune title of its own;
	// its column counts terminal cells, and FIX-1 does not match it.
	second, ok := watchCardAt(screen, "FIX-10")
	if !ok {
		t.Fatalf("FIX-10 not found:\n%s", strings.Join(screen, "\n"))
	}
	if wantCol := first.Col + inner + 4 + 1; second.Row != first.Row || second.Col != wantCol || !second.Unselected() || second.Selected() {
		t.Errorf("FIX-10 = row %d col %d corner %q marker %q, want row %d col %d, rounded frame and no ▸", second.Row, second.Col, second.Corner, second.Marker, first.Row, wantCol)
	}
	if second.Rows[0] != "● FIX-10  日本語のタイトル" || second.Rows[2] != "mini · fx" {
		t.Errorf("FIX-10 rows = %q", second.Rows)
	}

	// A wide-rune title on the LEFT card must not shift the right card's cells.
	wideLeft := watchTestScreen(stream, watchTestFrame(false, inner, rowsOf("FIX-3", "日本語のタイトル")), selected)
	right, ok := watchCardAt(wideLeft, "FIX-1")
	if !ok || right.Col != 4+inner+4+1 || !right.Selected() || right.Rows[1] != "web · development" {
		t.Errorf("card right of a wide-rune title = %+v (found=%t)", right, ok)
	}

	for _, id := range []string{"FIX-2", "FIX", "OTHER-9"} {
		if _, ok := watchCardAt(screen, id); ok {
			t.Errorf("%q matched, but it is only a stream line, a prefix or absent", id)
		}
	}
	if _, ok := watchCardAt(screen[:2], "FIX-1"); ok {
		t.Error("a card cut off before its fifth row matched")
	}
}

func TestWatchPlainCard(t *testing.T) {
	t.Parallel()
	frame := func(selected bool, id, state string) string {
		marker := "  "
		if selected {
			marker = "> "
		}
		return strings.Join([]string{
			" test-host      1 running   queue 0",
			marker + "● " + id + "  a title",
			"    project · development",
			"    mini · fx",
			"    " + state,
			"    activity not reported",
			"session stream  [follow]",
			"20:00:00 [" + id + "] thought  hello",
			"↑↓/jk select   enter detail   [ ] split   0 reset   f follow/pause   g tail   q quit",
		}, "\n") + "\n"
	}
	output := frame(true, "FIX-1", "running 10s") + frame(false, "FIX-1", "running 11s · 1 turns")
	card, ok := watchPlainCard(output, "FIX-1")
	want := [watchCardRows]string{"● FIX-1  a title", "project · development", "mini · fx", "running 11s · 1 turns", "activity not reported"}
	if !ok || card != want {
		t.Errorf("freshest card = %q (found=%t), want %q", card, ok, want)
	}
	selected, ok := watchPlainCard(frame(true, "FIX-2", "running 1s"), "FIX-2")
	if !ok || selected[0] != "● FIX-2  a title" {
		t.Errorf("selected card = %q (found=%t)", selected, ok)
	}
	for _, id := range []string{"FIX", "FIX-3"} {
		if _, ok := watchPlainCard(output, id); ok {
			t.Errorf("%q matched a plain card", id)
		}
	}
	if _, ok := watchPlainCard("[FIX-1] thought  hello\n", "FIX-1"); ok {
		t.Error("a stream line matched a plain card")
	}
}

func TestWatchDetail(t *testing.T) {
	t.Parallel()
	screen := []string{
		" test-host   1 running",
		"╭──╮",
		"session detail · FIX-1  esc close",
		"Issue              FIX-1                                     Endpoint operator  unknown",
		"Title              not reported                              Protocol           openai-chat",
		"Model              request-alias                             Cost               $1.25",
		"Model identity     unknown                                   Agent card         Local Reviewer",
		"Worktree           /tmp/some  path                           Turns              4",
		"",
		"↑↓/jk select   enter detail   [ ] split   0 reset   f follow/pause   g tail   q quit",
	}
	if id, ok := watchDetailID(screen); !ok || id != "FIX-1" {
		t.Errorf("detail id = %q (found=%t), want FIX-1", id, ok)
	}
	for label, want := range map[string]string{
		"Issue": "FIX-1", "Title": "not reported", "Model": "request-alias", "Model identity": "unknown",
		"Endpoint operator": "unknown", "Protocol": "openai-chat", "Cost": "$1.25", "Agent card": "Local Reviewer",
		"Turns": "4",
	} {
		if got, ok := watchDetailValue(screen, label); !ok || got != want {
			t.Errorf("%s = %q (found=%t), want %q", label, got, ok, want)
		}
	}
	for _, label := range []string{"Card ID", "Actual provider", "identity", "Endpoint"} {
		if got, ok := watchDetailValue(screen, label); ok {
			t.Errorf("%s matched %q, but no such row", label, got)
		}
	}
	if _, ok := watchDetailValue(screen[:2], "Issue"); ok {
		t.Error("a value was read without a detail pane")
	}
	if _, ok := watchDetailValue([]string{"Issue              FIX-1"}, "Issue"); ok {
		t.Error("a value was read from a screen without a pane title")
	}
	if _, ok := watchDetailID([]string{"session stream  [follow]"}); ok {
		t.Error("the stream title was read as a detail pane")
	}

	bad := watchFieldMismatches(screen, []watchField{field("Issue", "FIX-1"), field("Cost", "$2.00"), field("Card ID", "x")})
	if len(bad) != 2 || !strings.HasPrefix(bad[0], "Cost: got $1.25") || !strings.HasPrefix(bad[1], "Card ID: missing") {
		t.Errorf("mismatches = %q", bad)
	}
}

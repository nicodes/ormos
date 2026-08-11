package system

import (
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/nicodes/ormos/relay"
)

// sized builds a model and drives it through a resize and a tick, which is the
// state every real render is in: a View at width 0 exercises none of the
// clipping or the pinning.
func sized(t *testing.T, d *system, w, h int) model {
	t.Helper()
	m, _ := newModel(d).Update(tea.WindowSizeMsg{Width: w, Height: h})
	m, _ = m.(model).Update(tickMsg{})
	return m.(model)
}

// In TUI mode the log ring is the only place the agent's own errors go: nothing
// is echoed to stderr because the dashboard owns the screen. Before the
// ACTIVITY pane existed, "tunnel error: ..." was written to a buffer nothing
// rendered, and a machine that could not connect showed "offline" with no
// reason anywhere on the screen.
func TestDashboardRendersTheLogRing(t *testing.T) {
	withTempConfigDir(t)
	d := newSystem(systemConfig{})
	d.logf("tunnel error: dial tcp: connection refused (retry in 1s)")

	view := sized(t, d, 80, 24).View()
	if !strings.Contains(view, "ACTIVITY") {
		t.Error("the dashboard has no activity pane")
	}
	if !strings.Contains(view, "connection refused") {
		t.Errorf("the tunnel error never reached the screen:\n%s", view)
	}
}

// The pane is the tail of the ring, not the ring: 200 lines would push the
// dashboard off the screen, and the copy is made every render tick.
func TestDashboardShowsOnlyTheTailOfTheRing(t *testing.T) {
	withTempConfigDir(t)
	d := newSystem(systemConfig{})
	for _, line := range []string{"oldest", "second", "third", "fourth", "fifth", "sixth", "seventh", "newest"} {
		d.logf("%s", line)
	}

	view := sized(t, d, 80, 24).View()
	if strings.Contains(view, "oldest") {
		t.Error("the pane is showing more than the last few lines")
	}
	if !strings.Contains(view, "newest") {
		t.Error("the pane is not showing the most recent line")
	}
	if got := len(d.RecentLogs(activityLines)); got != activityLines {
		t.Errorf("RecentLogs(%d) returned %d lines", activityLines, got)
	}
}

// A ring holding fewer lines than the pane can show must not over-read.
func TestRecentLogsHandlesAShortRing(t *testing.T) {
	withTempConfigDir(t)
	d := newSystem(systemConfig{})
	if got := d.RecentLogs(activityLines); len(got) != 0 {
		t.Fatalf("a fresh agent has logged nothing, got %v", got)
	}
	d.logf("only line")
	got := d.RecentLogs(activityLines)
	if len(got) != 1 || !strings.HasSuffix(got[0], "only line") {
		t.Fatalf("RecentLogs = %v, want the one line logged", got)
	}
	if got := d.RecentLogs(0); got != nil {
		t.Errorf("RecentLogs(0) = %v, want nil", got)
	}
}

// A long line is clipped rather than wrapped: wrapping would take several rows
// and change the pane's height from one tick to the next.
func TestClipMarksTheCut(t *testing.T) {
	for _, tc := range []struct {
		in   string
		w    int
		want string
	}{
		{"short", 20, "short"},
		{"exactly ten", 11, "exactly ten"},
		{"far too long to fit", 10, "far too l…"},
		{"unclipped when width is unknown", 0, "unclipped when width is unknown"},
		{"x", -5, "x"},
		// At a single column the marker is all that fits, which still says
		// "there is more here" — better than an arbitrary first character.
		{"abc", 1, "…"},
		// Columns, not bytes. Ten arrows are 30 bytes: slicing by length
		// truncated a line that fitted, and cut the last rune in half.
		{"→→→→→→→→→→", 20, "→→→→→→→→→→"},
		{"→abc", 4, "→abc"},
	} {
		if got := clip(tc.in, tc.w); got != tc.want {
			t.Errorf("clip(%q, %d) = %q, want %q", tc.in, tc.w, got, tc.want)
		}
	}
	// Whatever the width, the result must still be text a terminal can print.
	for _, in := range []string{"→abc", "日本語のテキストです", "café ☕ résumé"} {
		for w := 1; w < 24; w++ {
			got := clip(in, w)
			if !utf8.ValidString(got) {
				t.Errorf("clip(%q, %d) = %q, which is not valid UTF-8", in, w, got)
			}
			if lipgloss.Width(got) > w {
				t.Errorf("clip(%q, %d) = %q, which is %d columns wide", in, w, got, lipgloss.Width(got))
			}
		}
	}
}

// pad lays out columns, so it has to count columns too — and it is applied to
// project names and root directories, which are the user's own text.
func TestPadCountsColumnsNotBytes(t *testing.T) {
	for _, in := range []string{"app", "日本語", "→→→→→→→→→→", ""} {
		for _, w := range []int{1, 4, 8, 18, 32} {
			got := pad(in, w)
			if !utf8.ValidString(got) {
				t.Errorf("pad(%q, %d) = %q, which is not valid UTF-8", in, w, got)
			}
			if lipgloss.Width(got) != w {
				t.Errorf("pad(%q, %d) is %d columns, want exactly %d", in, w, lipgloss.Width(got), w)
			}
		}
	}
}

// Bubble Tea's renderer keeps the LAST height rows of an over-tall view, so a
// pane that outgrows the screen deletes the header rather than scrolling the
// log — and the header is where the connection state, the account and the
// sealing fingerprint live, the last being the out-of-band check against a
// relay that swapped the key.
//
// The property asserted is that the pane is BUDGETED: it may never make the
// view taller than the screen, and when the body alone already overflows (a
// long project list, which this change does not address and never did) the pane
// must add nothing at all.
func TestActivityPaneNeverPushesTheViewOffScreen(t *testing.T) {
	withTempConfigDir(t)
	d := newSystem(systemConfig{RelayURL: "wss://relay.example", Email: "you@example.test"})
	for i := range 40 {
		d.logf("line %d", i)
	}

	for _, projects := range []int{0, 1, 2, 4, 8, 20} {
		for _, wh := range [][2]int{{80, 10}, {80, 16}, {80, 24}, {80, 40}, {20, 24}, {40, 16}, {2, 24}} {
			w, h := wh[0], wh[1]
			m := sized(t, d, w, h)
			m.projects = make([]relay.ProjectInfo, projects)
			for i := range m.projects {
				m.projects[i] = relay.ProjectInfo{
					Name: "project", RootDir: "/home/you/code/project",
					Ports: []relay.PortEntry{{Port: 3000 + i}},
				}
			}
			m.rebuildRows()

			bare := m
			bare.logs = nil
			bareH := lipgloss.Height(bare.View())
			view := m.View()
			withH := lipgloss.Height(view)

			// Every row must fit the width, or lipgloss.Place pads the whole
			// block out to the widest one and the height above stops describing
			// the screen.
			for _, line := range strings.Split(view, "\n") {
				if lipgloss.Width(line) > w {
					t.Fatalf("%d projects at %dx%d: a row is %d columns wide: %q",
						projects, w, h, lipgloss.Width(line), line)
				}
			}

			switch {
			case bareH > h:
				// The body alone does not fit. The pane must cost nothing.
				if withH != bareH {
					t.Errorf("%d projects at %dx%d: the pane added %d rows to a view that already overflowed",
						projects, w, h, withH-bareH)
				}
			case withH > h:
				t.Errorf("%d projects at %dx%d rendered %d rows; the header is dropped",
					projects, w, h, withH)
			}
			// The footer must survive the renderer's truncation, not merely be
			// present in the string: Bubble Tea keeps the LAST height rows, so
			// what matters is that the footer is inside that window. Asserted
			// on the divider rather than the hint text, because at 20 or 2
			// columns the hints are legitimately clipped away.
			if !strings.Contains(lastRows(view, h), "─") {
				t.Errorf("%d projects at %dx%d: the footer is outside the last %d rows", projects, w, h, h)
			}
		}
	}
}

// The ring is relay-influenced: an HTTP reason phrase comes back verbatim in
// "relay returned %s". Painted raw into the alt screen, a relay could set the
// window title, clear the display, or paint a line of its own — from the one
// pane whose purpose is telling the operator the truth about their machine.
func TestDashboardStripsTerminalEscapes(t *testing.T) {
	withTempConfigDir(t)
	d := newSystem(systemConfig{})
	d.logf("ports poll failed: relay returned 403 \x1b]0;OWNED\a\x1b[2Jclick https://evil.example.test to fix")

	view := sized(t, d, 80, 24).View()
	// The agent's own styling emits ESC sequences, so this looks for the shapes
	// only the relay's text could produce: an OSC introducer, a BEL, and an
	// erase-display. What survives is the payload as inert characters, which is
	// the point — the text is still readable, it just cannot act.
	for _, seq := range []string{"\x1b]", "\a", "\x1b[2J"} {
		if strings.Contains(view, seq) {
			t.Errorf("a relay-supplied %q reached the screen", seq)
		}
	}
	if !strings.Contains(view, "relay returned 403") {
		t.Error("sanitising removed the message along with the escapes")
	}

	// Nothing unprintable may survive at all, and a newline least of all: it
	// would add a full-width row and make the pane's height depend on what the
	// relay sent.
	for _, in := range []string{"a\x1b[31mb", "two\nrows", "bell\a", "null\x00byte", "del\x7f"} {
		for _, r := range sanitize(in) {
			if !unicode.IsPrint(r) {
				t.Errorf("sanitize(%q) kept %q", in, r)
			}
		}
	}
}

// The pane has to follow the ring, not the ring as it was when the model was
// built: the tunnel error that matters arrives seconds into a session.
func TestDashboardFollowsTheRingOnEveryTick(t *testing.T) {
	withTempConfigDir(t)
	d := newSystem(systemConfig{})
	m := sized(t, d, 80, 24)

	d.logf("tunnel error: connection refused")
	if strings.Contains(m.View(), "connection refused") {
		t.Fatal("the model saw a line logged after it was built, without a tick")
	}
	next, _ := m.Update(tickMsg{})
	if !strings.Contains(next.(model).View(), "connection refused") {
		t.Error("a tick did not bring the new line to the screen")
	}
}

// Status carries nothing the dashboard does not render. It used to carry two
// fields no non-test code ever read, copied on every 500ms tick; these pin the
// remaining ones to something on screen so they cannot quietly become the same
// thing.
func TestDashboardRendersEveryStatusField(t *testing.T) {
	withTempConfigDir(t)
	d := newSystem(systemConfig{RelayURL: "wss://relay.example"})
	d.setConnected(true)
	d.addSession(2)

	view := sized(t, d, 80, 24).View()
	for _, want := range []string{"connected", "2 sessions", "wss://relay.example"} {
		if !strings.Contains(view, want) {
			t.Errorf("the dashboard never shows %q:\n%s", want, view)
		}
	}
}

// lastRows returns the final n rows of a view — what Bubble Tea's renderer
// actually keeps when the view is taller than the terminal.
func lastRows(view string, n int) string {
	lines := strings.Split(view, "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// Almost everything on this screen is text the relay chose: the system name,
// project names, root directories and port labels all come off its JSON. The
// activity pane was sanitised in round 1 and these were not — so the same
// capability was still reachable, through a field that is on screen the whole
// time rather than only when something is logged.
//
// The column-aware clip made it worse before it made it better: byte-slicing
// used to cap a hostile name at 18 bytes, while ansi.Truncate deliberately
// re-emits escape sequences and charges them nothing against the width.
func TestDashboardStripsEscapesFromRelayText(t *testing.T) {
	withTempConfigDir(t)
	d := newSystem(systemConfig{})
	m := sized(t, d, 80, 24)

	// Arriving the way the relay delivers it, so the boundary is what is tested
	// rather than the render sites.
	info := &relay.SystemInfo{Name: "sys\x1b]0;OWNED\a"}
	next, _ := m.Update(systemInfoMsg{info: info})
	next, _ = next.(model).Update(projectsMsg{projects: []relay.ProjectInfo{{
		Name:    "proj\x1b]0;PWN\a",
		RootDir: "/r\x1b[2Joot",
		Ports:   []relay.PortEntry{{Port: 3000, Label: "lbl\x1b]0;NOPE\a"}},
	}}})
	view := next.(model).View()

	for _, seq := range []string{"\x1b]", "\a", "\x1b[2J"} {
		if strings.Contains(view, seq) {
			t.Errorf("a relay-supplied %q reached the screen", seq)
		}
	}
	// The readable text still has to survive.
	for _, want := range []string{"sys", "proj", "oot", "lbl"} {
		if !strings.Contains(view, want) {
			t.Errorf("sanitising removed %q along with the escapes", want)
		}
	}
}

// A relay-chosen string is also a length, and a long one used to inflate the
// whole block: lipgloss.Place pads every row out to the widest, so one 219-
// column label turned a 24-line view into 41 rows and pushed the sealing
// fingerprint off the top.
func TestLongRelayTextCannotPushTheHeaderOffScreen(t *testing.T) {
	withTempConfigDir(t)
	d := newSystem(systemConfig{})
	m := sized(t, d, 80, 24)

	next, _ := m.Update(projectsMsg{projects: []relay.ProjectInfo{{
		Name:    strings.Repeat("N", 200),
		RootDir: strings.Repeat("R", 200),
		Ports:   []relay.PortEntry{{Port: 3000, Label: strings.Repeat("L", 200)}},
	}}})
	view := next.(model).View()

	if h := lipgloss.Height(view); h > 24 {
		t.Errorf("a long relay string produced %d rows on a 24-row screen", h)
	}
	for _, line := range strings.Split(view, "\n") {
		if lipgloss.Width(line) > 80 {
			t.Errorf("a row is %d columns on an 80-column screen: %q", lipgloss.Width(line), line)
		}
	}
	// The fingerprint is the out-of-band check against a swapped key; it is the
	// thing that must not be what falls off.
	if !strings.Contains(lastRows(view, 24), "sealing") {
		t.Error("the sealing fingerprint was pushed off the screen")
	}
}

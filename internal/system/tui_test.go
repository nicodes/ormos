package system

import (
	"strings"
	"testing"
)

// In TUI mode the log ring is the only place the agent's own errors go: nothing
// is echoed to stderr because the dashboard owns the screen. Before the
// ACTIVITY pane existed, "tunnel error: ..." was written to a buffer nothing
// rendered, and a machine that could not connect showed "offline" with no
// reason anywhere on the screen.
func TestDashboardRendersTheLogRing(t *testing.T) {
	withTempConfigDir(t)
	d := newSystem(systemConfig{})
	d.logf("tunnel error: dial tcp: connection refused (retry in 1s)")

	view := newModel(d).View()
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

	view := newModel(d).View()
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
		{"abc", 1, "a"},
	} {
		if got := clip(tc.in, tc.w); got != tc.want {
			t.Errorf("clip(%q, %d) = %q, want %q", tc.in, tc.w, got, tc.want)
		}
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

	view := newModel(d).View()
	for _, want := range []string{"connected", "2 sessions", "wss://relay.example"} {
		if !strings.Contains(view, want) {
			t.Errorf("the dashboard never shows %q:\n%s", want, view)
		}
	}
}

//go:build (linux && !android) || (darwin && !ios)

package system

import (
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/nicodes/ormos/relay"
)

type fakeTerminalAttachment struct {
	output    chan []byte
	done      chan struct{}
	writes    [][]byte
	writeErrs []error
	resizes   [][2]int
	resizeErr error
	detached  int
	mu        sync.Mutex
}

func newFakeTerminalAttachment() *fakeTerminalAttachment {
	return &fakeTerminalAttachment{output: make(chan []byte, 8), done: make(chan struct{})}
}

func (f *fakeTerminalAttachment) Output() <-chan []byte { return f.output }
func (f *fakeTerminalAttachment) Done() <-chan struct{} { return f.done }
func (f *fakeTerminalAttachment) Write(input []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.writes = append(f.writes, append([]byte(nil), input...))
	if len(f.writeErrs) == 0 {
		return nil
	}
	err := f.writeErrs[0]
	f.writeErrs = f.writeErrs[1:]
	return err
}
func (f *fakeTerminalAttachment) Resize(cols, rows int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resizes = append(f.resizes, [2]int{cols, rows})
	return f.resizeErr
}
func (f *fakeTerminalAttachment) Detach() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.detached++
}
func (f *fakeTerminalAttachment) snapshot() ([][]byte, [][2]int, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	writes := make([][]byte, len(f.writes))
	for i := range f.writes {
		writes[i] = append([]byte(nil), f.writes[i]...)
	}
	return writes, append([][2]int(nil), f.resizes...), f.detached
}

func openTestTerminal(t *testing.T) (model, *fakeTerminalAttachment, tea.Cmd) {
	t.Helper()
	m := terminalDashboard(t)
	client := newFakeTerminalAttachment()
	oldAttach := attachTerminalScreen
	t.Cleanup(func() { attachTerminalScreen = oldAttach })
	attachTerminalScreen = func(_ *system, root string, info relay.TerminalSessionInfo, cols, rows int) (terminalAttachment, error) {
		if root != "/alpha" || info.SessionID != "a-tab" || info.State != relay.TerminalStateRunning || info.Generation != 1 || cols != 80 || rows != 27 {
			t.Fatalf("attach root=%q info=%+v size=%dx%d", root, info, cols, rows)
		}
		return client, nil
	}

	m.cursor = rowIndex(m.rows, rowTerminal, "project-a", "a-tab")
	next, command := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = next.(model)
	if m.mode != modeTerminal || !strings.Contains(m.View(), "connecting") {
		t.Fatalf("enter mode=%v view=%q", m.mode, m.View())
	}
	commandMsg := command()
	batch, ok := commandMsg.(tea.BatchMsg)
	if !ok {
		t.Fatalf("enter command = %T", commandMsg)
	}
	var attached terminalAttachedMsg
	for _, cmd := range batch {
		if msg, ok := cmd().(terminalAttachedMsg); ok {
			attached = msg
		}
	}
	if attached.client == nil || attached.err != nil {
		t.Fatalf("attached = %#v", attached)
	}
	next, wait := m.Update(attached)
	return next.(model), client, wait
}

func TestTerminalScreenRendersOutputAndSerializesInput(t *testing.T) {
	m, client, wait := openTestTerminal(t)
	client.output <- []byte("\x1b]0;owned\a\x1b[2J\x1b[Hprompt$ ")
	next, wait := m.Update(wait())
	m = next.(model)
	view := m.View()
	if !strings.Contains(view, "ormos terminal") || !strings.Contains(view, "prompt$ ") {
		t.Fatalf("terminal view missing chrome or output:\n%s", view)
	}
	if strings.Contains(view, "\x1b]0;owned") || strings.Contains(view, "\x1b[2J") {
		t.Fatalf("raw terminal control escaped emulator: %q", view)
	}
	if lines := strings.Count(view, "\n") + 1; lines != 30 {
		t.Fatalf("terminal view lines = %d, want 30", lines)
	}

	next, firstWrite := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("echo")})
	m = next.(model)
	next, secondWrite := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = next.(model)
	if secondWrite != nil {
		t.Fatal("second input started before the first completed")
	}
	next, secondWrite = m.Update(firstWrite())
	m = next.(model)
	if secondWrite == nil {
		t.Fatal("queued enter was not scheduled after the first input")
	}
	next, _ = m.Update(secondWrite())
	m = next.(model)
	writes, _, _ := client.snapshot()
	if len(writes) != 2 || string(writes[0]) != "echo" || string(writes[1]) != "\r" {
		t.Fatalf("writes = %q", writes)
	}

	oldTerminals := terminalSessionsCommand
	t.Cleanup(func() { terminalSessionsCommand = oldTerminals })
	terminalSessionsCommand = func(*system) tea.Cmd { return func() tea.Msg { return terminalsMsg{} } }
	next, leave := m.Update(tea.KeyMsg{Type: tea.KeyCtrlG})
	m = next.(model)
	if m.mode != modeNormal {
		t.Fatalf("Ctrl-G left mode %v", m.mode)
	}
	writes, _, detached := client.snapshot()
	if detached != 1 {
		t.Fatalf("detach count = %d", detached)
	}
	for _, write := range writes {
		if strings.ContainsRune(string(write), rune(0x07)) {
			t.Fatalf("Ctrl-G reached shared terminal in %q", write)
		}
	}
	wantMouse := reflect.TypeOf(tea.EnableMouseCellMotion())
	foundMouse := false
	for _, cmd := range leave().(tea.BatchMsg) {
		if reflect.TypeOf(cmd()) == wantMouse {
			foundMouse = true
		}
	}
	if !foundMouse {
		t.Fatal("leaving terminal did not restore dashboard mouse reporting")
	}
}

func TestTerminalScreenResizesSharedPTY(t *testing.T) {
	m, client, _ := openTestTerminal(t)
	next, command := m.Update(tea.WindowSizeMsg{Width: 100, Height: 40})
	m = next.(model)
	if command != nil {
		t.Fatalf("resize unexpectedly returned command %T", command)
	}
	_, resizes, _ := client.snapshot()
	if len(resizes) != 1 || resizes[0] != [2]int{100, 37} {
		t.Fatalf("resizes = %#v", resizes)
	}
	if m.term.emulator.Width != 100 || m.term.emulator.Height != 37 {
		t.Fatalf("emulator size = %dx%d", m.term.emulator.Width, m.term.emulator.Height)
	}

	client.resizeErr = errors.New("bad\x1b]0;owned\a")
	next, _ = m.Update(tea.WindowSizeMsg{Width: 90, Height: 35})
	m = next.(model)
	if m.mode != modeNormal || strings.Contains(m.err, "\x1b") || !strings.Contains(m.err, "bad]0;owned") {
		t.Fatalf("resize failure mode=%v err=%q", m.mode, m.err)
	}
	_, _, detached := client.snapshot()
	if detached != 1 {
		t.Fatalf("resize failure detach count = %d", detached)
	}
}

func TestTerminalScreenIgnoresStaleAttachAfterDetach(t *testing.T) {
	m := terminalDashboard(t)
	client := newFakeTerminalAttachment()
	oldAttach := attachTerminalScreen
	t.Cleanup(func() { attachTerminalScreen = oldAttach })
	attachTerminalScreen = func(*system, string, relay.TerminalSessionInfo, int, int) (terminalAttachment, error) {
		return client, nil
	}
	m.cursor = rowIndex(m.rows, rowTerminal, "project-a", "a-tab")
	next, command := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = next.(model)
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyCtrlG})
	m = next.(model)
	for _, cmd := range command().(tea.BatchMsg) {
		if attached, ok := cmd().(terminalAttachedMsg); ok {
			next, _ = m.Update(attached)
			m = next.(model)
		}
	}
	_, _, detached := client.snapshot()
	if m.mode != modeNormal || m.term != nil || detached != 1 {
		t.Fatalf("stale attach mode=%v terminal=%v detach=%d", m.mode, m.term != nil, detached)
	}
}

func TestTerminalScreenDetachesSupersededClient(t *testing.T) {
	m, client, _ := openTestTerminal(t)
	next, command := m.Update(terminalCreatedMsg{
		projectRoot: "/alpha", info: relay.TerminalSessionInfo{ID: "new-record", ProjectID: "project-a", SessionID: "new-tab", State: relay.TerminalStateRunning, Generation: 1},
	})
	m = next.(model)
	_, _, detached := client.snapshot()
	if detached != 1 || m.mode != modeTerminal || m.term == nil || m.term.label != "new-tab" || command == nil {
		t.Fatalf("superseded terminal detach=%d mode=%v terminal=%#v command=%v", detached, m.mode, m.term, command != nil)
	}
}

func TestTerminalScreenAttachFailureReturnsSafely(t *testing.T) {
	m := terminalDashboard(t)
	oldAttach := attachTerminalScreen
	t.Cleanup(func() { attachTerminalScreen = oldAttach })
	attachTerminalScreen = func(*system, string, relay.TerminalSessionInfo, int, int) (terminalAttachment, error) {
		return nil, errors.New("denied\x1b]0;owned\a")
	}
	m.cursor = rowIndex(m.rows, rowTerminal, "project-a", "a-tab")
	next, command := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = next.(model)
	for _, cmd := range command().(tea.BatchMsg) {
		if attached, ok := cmd().(terminalAttachedMsg); ok {
			next, _ = m.Update(attached)
			m = next.(model)
		}
	}
	if m.mode != modeNormal || m.term != nil || strings.Contains(m.err, "\x1b") || !strings.Contains(m.err, "denied]0;owned") {
		t.Fatalf("attach failure mode=%v terminal=%v err=%q", m.mode, m.term != nil, m.err)
	}
}

func TestTerminalScreenRetriesBackpressureAndReturnsErrorsSafely(t *testing.T) {
	m, client, _ := openTestTerminal(t)
	client.writeErrs = []error{ErrTerminalInputBackpressure, ErrTerminalInputBackpressure, nil}
	next, write := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("same")})
	m = next.(model)
	next, _ = m.Update(write())
	m = next.(model)
	writes, _, _ := client.snapshot()
	if len(writes) != 3 {
		t.Fatalf("write attempts = %d", len(writes))
	}
	for i, input := range writes {
		if string(input) != "same" {
			t.Fatalf("attempt %d = %q", i, input)
		}
	}

	client.writeErrs = []error{errors.New("write failed\x1b]0;owned\a")}
	next, write = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("x")})
	m = next.(model)
	next, _ = m.Update(write())
	m = next.(model)
	if m.mode != modeNormal || strings.Contains(m.err, "\x1b") || !strings.Contains(m.err, "write failed") {
		t.Fatalf("write failure mode=%v err=%q", m.mode, m.err)
	}
}

func TestTerminalScreenTracksModesAndEncodesPasteAndCursor(t *testing.T) {
	m, _, _ := openTestTerminal(t)
	generation := m.term.generation
	next, _ := m.Update(terminalOutputMsg{
		generation: generation,
		data:       []byte("\x1b[?1h\x1b[?2004h"),
	})
	m = next.(model)
	if !m.term.applicationCursor || !m.term.bracketedPaste {
		t.Fatalf("enabled modes cursor=%v paste=%v", m.term.applicationCursor, m.term.bracketedPaste)
	}
	if got := string(terminalKeyBytes(tea.KeyMsg{Type: tea.KeyUp}, m.term.applicationCursor, m.term.bracketedPaste)); got != "\x1bOA" {
		t.Fatalf("application cursor = %q", got)
	}
	paste := tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("echo one\necho two"), Paste: true}
	if got := string(terminalKeyBytes(paste, m.term.applicationCursor, m.term.bracketedPaste)); got != "\x1b[200~echo one\necho two\x1b[201~" {
		t.Fatalf("bracketed paste = %q", got)
	}

	next, _ = m.Update(terminalOutputMsg{
		generation: generation,
		data:       []byte("\x1b[?1l\x1b[?2004l"),
	})
	m = next.(model)
	if m.term.applicationCursor || m.term.bracketedPaste {
		t.Fatalf("disabled modes cursor=%v paste=%v", m.term.applicationCursor, m.term.bracketedPaste)
	}
	if got := string(terminalKeyBytes(paste, false, false)); got != "echo one\necho two" {
		t.Fatalf("plain paste = %q", got)
	}
}

func TestTerminalScreenClampsEmulatorAndSerializesDeviceResponses(t *testing.T) {
	m, client, _ := openTestTerminal(t)
	generation := m.term.generation
	next, firstWrite := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("x")})
	m = next.(model)
	next, _ = m.Update(terminalOutputMsg{
		generation: generation,
		data:       []byte("\x1b[999dX\x1b[999L"),
	})
	m = next.(model)
	if m.term.emulator.Height != 27 || m.term.emulator.Width != 80 {
		t.Fatalf("emulator grew to %dx%d", m.term.emulator.Width, m.term.emulator.Height)
	}
	view := m.View()
	if strings.Count(view, "\n")+1 != 30 || !strings.Contains(view, "ormos terminal") || !strings.Contains(view, "Ctrl-G: dashboard") {
		t.Fatalf("grown terminal hid chrome:\n%s", view)
	}
	next, _ = m.Update(terminalOutputMsg{generation: generation, data: []byte("\x1b[6n\x1b[c")})
	m = next.(model)
	if len(m.term.input) != 2 || string(m.term.input[0]) != "x" || string(m.term.input[1]) != "\x1b[27;2R\x1b[?62;22c" {
		t.Fatalf("serialized input = %q", m.term.input)
	}

	next, secondWrite := m.Update(firstWrite())
	m = next.(model)
	if secondWrite == nil {
		t.Fatal("device response was not scheduled after keyboard input")
	}
	next, _ = m.Update(secondWrite())
	m = next.(model)
	writes, _, _ := client.snapshot()
	if len(writes) != 2 || string(writes[0]) != "x" || string(writes[1]) != "\x1b[27;2R\x1b[?62;22c" {
		t.Fatalf("device response writes = %q", writes)
	}
}

func TestTerminalKeyBytes(t *testing.T) {
	tests := []struct {
		name string
		key  tea.KeyMsg
		want string
	}{
		{"runes", tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("λx")}, "λx"},
		{"alt rune", tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'x'}, Alt: true}, "\x1bx"},
		{"paste", tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("a\nb"), Paste: true}, "a\nb"},
		{"enter", tea.KeyMsg{Type: tea.KeyEnter}, "\r"},
		{"backspace", tea.KeyMsg{Type: tea.KeyBackspace}, "\x7f"},
		{"up", tea.KeyMsg{Type: tea.KeyUp}, "\x1b[A"},
		{"ctrl left", tea.KeyMsg{Type: tea.KeyCtrlLeft}, "\x1b[1;5D"},
		{"alt shift down", tea.KeyMsg{Type: tea.KeyShiftDown, Alt: true}, "\x1b[1;4B"},
		{"home", tea.KeyMsg{Type: tea.KeyHome}, "\x1b[H"},
		{"ctrl shift end", tea.KeyMsg{Type: tea.KeyCtrlShiftEnd}, "\x1b[1;6F"},
		{"delete", tea.KeyMsg{Type: tea.KeyDelete}, "\x1b[3~"},
		{"ctrl page down", tea.KeyMsg{Type: tea.KeyCtrlPgDown}, "\x1b[6;5~"},
		{"f1", tea.KeyMsg{Type: tea.KeyF1}, "\x1bOP"},
		{"alt f5", tea.KeyMsg{Type: tea.KeyF5, Alt: true}, "\x1b[15;3~"},
		{"f13", tea.KeyMsg{Type: tea.KeyF13}, "\x1b[1;2P"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := string(terminalKeyBytes(test.key, false, false)); got != test.want {
				t.Fatalf("encoded = %q, want %q", got, test.want)
			}
		})
	}
}

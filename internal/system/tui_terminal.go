//go:build (linux && !android) || (darwin && !ios)

package system

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/nicodes/ormos/relay"
	"github.com/vito/midterm"
)

const (
	terminalChromeRows      = 3
	terminalInputRetryWait  = 10 * time.Millisecond
	terminalInputRetryLimit = 2 * time.Second
)

type terminalAttachment interface {
	Output() <-chan []byte
	Done() <-chan struct{}
	Write([]byte) error
	Resize(int, int) error
	Detach()
}

type terminalScreen struct {
	generation uint64
	label      string
	client     terminalAttachment
	emulator   *midterm.Terminal
	responses  bytes.Buffer
	input      [][]byte
	writing    bool
}

type terminalAttachedMsg struct {
	generation uint64
	client     terminalAttachment
	cols, rows int
	err        error
}

type terminalOutputMsg struct {
	generation uint64
	data       []byte
}

type terminalInputWrittenMsg struct {
	generation uint64
	done       bool
	err        error
}

type terminalFinishedMsg struct {
	generation uint64
	err        error
}

var attachTerminalScreen = func(d *system, projectRoot, terminalKey string, cols, rows int) (terminalAttachment, error) {
	return d.AttachTerminal(projectRoot, terminalKey, cols, rows)
}

func (m model) startTerminal(projectRoot, terminalKey, label string) (model, tea.Cmd) {
	m.termGeneration++
	if clean := strings.TrimSpace(sanitize(label)); clean != "" {
		label = clean
	} else {
		label = "terminal"
	}
	m.mode = modeTerminal
	m.err = ""
	m.term = &terminalScreen{generation: m.termGeneration, label: label}

	generation := m.termGeneration
	cols, rows := terminalDimensions(m.width, m.height)
	ctx, d := m.appCtx, m.d
	return m, func() tea.Msg {
		if err := ctx.Err(); err != nil {
			return terminalAttachedMsg{generation: generation, err: err}
		}
		client, err := attachTerminalScreen(d, projectRoot, terminalKey, cols, rows)
		if err == nil {
			select {
			case <-ctx.Done():
				client.Detach()
				return terminalAttachedMsg{generation: generation, err: ctx.Err()}
			default:
			}
		}
		return terminalAttachedMsg{generation: generation, client: client, cols: cols, rows: rows, err: err}
	}
}

func terminalDimensions(width, height int) (int, int) {
	cols, rows := width, height-terminalChromeRows
	if !relay.ValidTerminalSize(cols, rows) {
		return 80, 21
	}
	return cols, rows
}

func (m model) updateTerminalAttached(msg terminalAttachedMsg) (tea.Model, tea.Cmd) {
	if m.term == nil || m.mode != modeTerminal || msg.generation != m.term.generation {
		if msg.client != nil {
			msg.client.Detach()
		}
		return m, nil
	}
	if msg.err != nil {
		return m.leaveTerminal(fmt.Errorf("attach terminal: %w", msg.err))
	}
	if msg.client == nil {
		return m.leaveTerminal(errors.New("attach terminal: no terminal client"))
	}

	cols, rows := terminalDimensions(m.width, m.height)
	if cols != msg.cols || rows != msg.rows {
		if err := msg.client.Resize(cols, rows); err != nil {
			msg.client.Detach()
			return m.leaveTerminal(fmt.Errorf("resize terminal: %w", err))
		}
	}
	m.term.client = msg.client
	m.term.emulator = midterm.NewTerminal(rows, cols)
	m.term.emulator.ForwardResponses = &m.term.responses
	return m, m.waitTerminalOutputCmd()
}

func (m model) waitTerminalOutputCmd() tea.Cmd {
	if m.term == nil || m.term.client == nil {
		return nil
	}
	generation, client, ctx := m.term.generation, m.term.client, m.appCtx
	return func() tea.Msg {
		select {
		case data := <-client.Output():
			return terminalOutputMsg{generation: generation, data: append([]byte(nil), data...)}
		case <-client.Done():
			return terminalFinishedMsg{generation: generation}
		case <-ctx.Done():
			return terminalFinishedMsg{generation: generation}
		}
	}
}

func (m model) updateTerminalOutput(msg terminalOutputMsg) (tea.Model, tea.Cmd) {
	if m.term == nil || msg.generation != m.term.generation || m.term.emulator == nil {
		return m, nil
	}
	n, err := m.term.emulator.Write(msg.data)
	if err == nil && n != len(msg.data) {
		err = io.ErrShortWrite
	}
	if err != nil {
		return m.leaveTerminal(fmt.Errorf("render terminal output: %w", err))
	}

	commands := []tea.Cmd{m.waitTerminalOutputCmd()}
	if m.term.responses.Len() > 0 {
		response := append([]byte(nil), m.term.responses.Bytes()...)
		m.term.responses.Reset()
		var write tea.Cmd
		m, write = m.enqueueTerminalInput(response)
		if write != nil {
			commands = append(commands, write)
		}
	}
	return m, tea.Batch(commands...)
}

func (m model) updateTerminalKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if msg.Type == tea.KeyCtrlG {
		return m.leaveTerminal(nil)
	}
	if m.term == nil || m.term.client == nil {
		return m, nil
	}
	input := terminalKeyBytes(msg)
	if len(input) == 0 {
		return m, nil
	}
	return m.enqueueTerminalInput(input)
}

func (m model) enqueueTerminalInput(input []byte) (model, tea.Cmd) {
	if m.term == nil || m.term.client == nil || len(input) == 0 {
		return m, nil
	}
	m.term.input = append(m.term.input, append([]byte(nil), input...))
	if m.term.writing {
		return m, nil
	}
	m.term.writing = true
	return m, m.writeTerminalInputCmd()
}

func (m model) writeTerminalInputCmd() tea.Cmd {
	if m.term == nil || m.term.client == nil || len(m.term.input) == 0 {
		return nil
	}
	generation, client, ctx := m.term.generation, m.term.client, m.appCtx
	input := append([]byte(nil), m.term.input[0]...)
	return func() tea.Msg {
		deadline := time.NewTimer(terminalInputRetryLimit)
		defer deadline.Stop()
		for {
			err := client.Write(input)
			if err == nil {
				return terminalInputWrittenMsg{generation: generation}
			}
			if !errors.Is(err, ErrTerminalInputBackpressure) {
				return terminalInputWrittenMsg{generation: generation, err: err}
			}

			wait := time.NewTimer(terminalInputRetryWait)
			select {
			case <-ctx.Done():
				stopTimer(wait)
				return terminalInputWrittenMsg{generation: generation, done: true}
			case <-client.Done():
				stopTimer(wait)
				return terminalInputWrittenMsg{generation: generation, done: true}
			case <-deadline.C:
				stopTimer(wait)
				return terminalInputWrittenMsg{generation: generation, err: fmt.Errorf("terminal input remained busy for %s", terminalInputRetryLimit)}
			case <-wait.C:
			}
		}
	}
}

func (m model) updateTerminalInputWritten(msg terminalInputWrittenMsg) (tea.Model, tea.Cmd) {
	if m.term == nil || msg.generation != m.term.generation {
		return m, nil
	}
	if msg.done {
		return m.leaveTerminal(nil)
	}
	if msg.err != nil {
		return m.leaveTerminal(fmt.Errorf("write terminal input: %w", msg.err))
	}
	if len(m.term.input) > 0 {
		m.term.input = m.term.input[1:]
	}
	m.term.writing = false
	if len(m.term.input) == 0 {
		return m, nil
	}
	m.term.writing = true
	return m, m.writeTerminalInputCmd()
}

func (m model) updateTerminalFinished(msg terminalFinishedMsg) (tea.Model, tea.Cmd) {
	if m.term == nil || msg.generation != m.term.generation {
		return m, nil
	}
	return m.leaveTerminal(msg.err)
}

func (m model) resizeTerminal() (tea.Model, tea.Cmd) {
	if m.term == nil || m.term.client == nil || m.term.emulator == nil {
		return m, nil
	}
	cols, rows := terminalDimensions(m.width, m.height)
	if err := m.term.client.Resize(cols, rows); err != nil {
		return m.leaveTerminal(fmt.Errorf("resize terminal: %w", err))
	}
	m.term.emulator.Resize(rows, cols)
	return m, nil
}

func (m model) leaveTerminal(err error) (tea.Model, tea.Cmd) {
	if m.term != nil && m.term.client != nil {
		m.term.client.Detach()
	}
	m.term = nil
	m.termGeneration++
	m.mode = modeNormal
	if err != nil {
		m.err = sanitizeRelayOutput(err.Error())
	} else {
		m.err = ""
	}
	return m, tea.Batch(tea.EnableMouseCellMotion, m.terminalsCmd())
}

func (m model) terminalView() string {
	cols, rows := terminalDimensions(m.width, m.height)
	label := "terminal"
	if m.term != nil {
		label = m.term.label
	}
	state := offStyle.Render("● offline")
	if m.status.Connected {
		state = onStyle.Render("● connected")
	}
	header := clip(titleStyle.Render("ormos terminal")+labelStyle.Render("  "+label+"  ")+state, cols)
	body := placeTerminalMessage(rows, cols, hintStyle.Render("connecting…"))
	if m.term != nil && m.term.emulator != nil {
		var rendered strings.Builder
		_ = m.term.emulator.Render(&rendered)
		body = rendered.String()
	}
	footer := divider(cols) + "\n" + clip(hintStyle.Render("Ctrl-G: dashboard"), cols)
	return header + "\n" + body + "\n" + footer
}

func placeTerminalMessage(height, width int, content string) string {
	lines := make([]string, height)
	lines[0] = clip(content, width)
	return strings.Join(lines, "\n")
}

func terminalKeyBytes(msg tea.KeyMsg) []byte {
	if msg.Type == tea.KeyRunes {
		return withAlt(msg.Alt, []byte(string(msg.Runes)))
	}
	if msg.Type == tea.KeySpace {
		return withAlt(msg.Alt, []byte{' '})
	}
	if msg.Type >= 0 && msg.Type <= 31 || msg.Type == 127 {
		return withAlt(msg.Alt, []byte{byte(msg.Type)})
	}

	var sequence string
	switch msg.Type {
	case tea.KeyUp, tea.KeyDown, tea.KeyRight, tea.KeyLeft:
		sequence = cursorSequence(msg.Type, modifier(msg.Alt, false, false))
	case tea.KeyShiftUp, tea.KeyShiftDown, tea.KeyShiftRight, tea.KeyShiftLeft:
		sequence = cursorSequence(msg.Type, modifier(msg.Alt, true, false))
	case tea.KeyCtrlUp, tea.KeyCtrlDown, tea.KeyCtrlRight, tea.KeyCtrlLeft:
		sequence = cursorSequence(msg.Type, modifier(msg.Alt, false, true))
	case tea.KeyCtrlShiftUp, tea.KeyCtrlShiftDown, tea.KeyCtrlShiftRight, tea.KeyCtrlShiftLeft:
		sequence = cursorSequence(msg.Type, modifier(msg.Alt, true, true))
	case tea.KeyHome, tea.KeyEnd, tea.KeyShiftHome, tea.KeyShiftEnd, tea.KeyCtrlHome, tea.KeyCtrlEnd, tea.KeyCtrlShiftHome, tea.KeyCtrlShiftEnd:
		sequence = homeEndSequence(msg.Type, msg.Alt)
	case tea.KeyShiftTab:
		sequence = "\x1b[Z"
	case tea.KeyInsert:
		sequence = tildeSequence(2, modifier(msg.Alt, false, false))
	case tea.KeyDelete:
		sequence = tildeSequence(3, modifier(msg.Alt, false, false))
	case tea.KeyPgUp:
		sequence = tildeSequence(5, modifier(msg.Alt, false, false))
	case tea.KeyPgDown:
		sequence = tildeSequence(6, modifier(msg.Alt, false, false))
	case tea.KeyCtrlPgUp:
		sequence = tildeSequence(5, modifier(msg.Alt, false, true))
	case tea.KeyCtrlPgDown:
		sequence = tildeSequence(6, modifier(msg.Alt, false, true))
	default:
		sequence = functionKeySequence(msg.Type, msg.Alt)
	}
	return []byte(sequence)
}

func withAlt(alt bool, input []byte) []byte {
	if !alt {
		return input
	}
	return append([]byte{0x1b}, input...)
}

func modifier(alt, shift, ctrl bool) int {
	value := 1
	if shift {
		value++
	}
	if alt {
		value += 2
	}
	if ctrl {
		value += 4
	}
	return value
}

func cursorSequence(key tea.KeyType, modifier int) string {
	final := byte('A')
	switch key {
	case tea.KeyDown, tea.KeyShiftDown, tea.KeyCtrlDown, tea.KeyCtrlShiftDown:
		final = 'B'
	case tea.KeyRight, tea.KeyShiftRight, tea.KeyCtrlRight, tea.KeyCtrlShiftRight:
		final = 'C'
	case tea.KeyLeft, tea.KeyShiftLeft, tea.KeyCtrlLeft, tea.KeyCtrlShiftLeft:
		final = 'D'
	}
	if modifier == 1 {
		return fmt.Sprintf("\x1b[%c", final)
	}
	return fmt.Sprintf("\x1b[1;%d%c", modifier, final)
}

func homeEndSequence(key tea.KeyType, alt bool) string {
	final := byte('H')
	shift, ctrl := false, false
	switch key {
	case tea.KeyEnd, tea.KeyShiftEnd, tea.KeyCtrlEnd, tea.KeyCtrlShiftEnd:
		final = 'F'
	}
	switch key {
	case tea.KeyShiftHome, tea.KeyShiftEnd:
		shift = true
	case tea.KeyCtrlHome, tea.KeyCtrlEnd:
		ctrl = true
	case tea.KeyCtrlShiftHome, tea.KeyCtrlShiftEnd:
		shift, ctrl = true, true
	}
	mod := modifier(alt, shift, ctrl)
	if mod == 1 {
		return fmt.Sprintf("\x1b[%c", final)
	}
	return fmt.Sprintf("\x1b[1;%d%c", mod, final)
}

func tildeSequence(code, modifier int) string {
	if modifier == 1 {
		return fmt.Sprintf("\x1b[%d~", code)
	}
	return fmt.Sprintf("\x1b[%d;%d~", code, modifier)
}

func functionKeySequence(key tea.KeyType, alt bool) string {
	if key <= tea.KeyF1 && key >= tea.KeyF4 {
		final := byte('P') + byte(tea.KeyF1-key)
		if !alt {
			return fmt.Sprintf("\x1bO%c", final)
		}
		return fmt.Sprintf("\x1b[1;3%c", final)
	}
	if key <= tea.KeyF5 && key >= tea.KeyF12 {
		codes := [...]int{15, 17, 18, 19, 20, 21, 23, 24}
		return tildeSequence(codes[tea.KeyF5-key], modifier(alt, false, false))
	}
	if key <= tea.KeyF13 && key >= tea.KeyF16 {
		final := byte('P') + byte(tea.KeyF13-key)
		return fmt.Sprintf("\x1b[1;%d%c", modifier(alt, true, false), final)
	}
	if key <= tea.KeyF17 && key >= tea.KeyF20 {
		codes := [...]int{15, 17, 18, 19}
		return tildeSequence(codes[tea.KeyF17-key], modifier(alt, true, false))
	}
	return ""
}

func stopTimer(timer *time.Timer) {
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
}

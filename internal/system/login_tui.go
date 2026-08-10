package system

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/nicodes/ormos/relay"
)

// This file is the pairing screen shown while the agent waits for the user to
// approve its device code in the web app. It is deliberately separate from the
// main dashboard (tui.go): pairing happens before there is a token, so the
// dashboard's data-driven model cannot exist yet. What they share is the
// Elm-architecture shape, the alt-screen program, and the lipgloss styles
// declared in tui.go.

// loginModel renders the current device-flow round: the code to type into the
// web app and how the wait is going. It is drawn centred in the terminal.
type loginModel struct {
	code      string
	url       string
	expiresIn int
	restarts  int // codes that expired unapproved before this one
	ticking   bool
	width     int
	height    int
	out       *relay.ProvisionResponse
	err       error
}

// loginCodeMsg carries a freshly issued device code to the pairing screen.
type loginCodeMsg struct {
	start     relay.DeviceStartResponse
	restarted bool
}

// loginFinishedMsg ends the screen: out is set on approval, err on failure.
type loginFinishedMsg struct {
	out *relay.ProvisionResponse
	err error
}

// tickMsg drives the one-per-second expiry countdown.
type loginTickMsg struct{}

func loginTick() tea.Cmd {
	return tea.Tick(time.Second, func(time.Time) tea.Msg { return loginTickMsg{} })
}

func (m loginModel) Init() tea.Cmd { return nil }

func (m loginModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "ctrl+c", "q", "esc":
			m.err = errLoginCancelled
			return m, tea.Quit
		}
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
	case loginCodeMsg:
		m.code = msg.start.UserCode
		m.url = msg.start.VerificationURL
		m.expiresIn = msg.start.ExpiresIn
		if msg.restarted {
			m.restarts++
		}
		// Start the countdown exactly once; a re-issued code just resets the
		// number the single ticker is counting down.
		if !m.ticking {
			m.ticking = true
			return m, loginTick()
		}
	case loginTickMsg:
		if m.expiresIn > 0 {
			m.expiresIn--
		}
		return m, loginTick()
	case loginFinishedMsg:
		m.out = msg.out
		m.err = msg.err
		return m, tea.Quit
	}
	return m, nil
}

func (m loginModel) View() string {
	title := titleStyle.Render(blockOrmos())

	var body string
	if m.code == "" {
		body = title + "\n\n" + hintStyle.Render("requesting a pairing code…")
	} else {
		// The URL is a real OSC 8 hyperlink: clickable in terminals that
		// support it, plain text in those that don't.
		link := dirStyle.Render(hyperlink(m.url, m.url))
		body = strings.Join([]string{
			title,
			"",
			"Open " + link + " and enter the code:",
			selStyle.Render(m.code),
			hintStyle.Render(fmt.Sprintf("expires in %d seconds", m.expiresIn)),
			hintStyle.Render("ctrl-c to cancel"),
		}, "\n")
	}

	// Centre every line under the block title, then place the whole block in
	// the middle of the terminal (horizontally and vertically).
	body = lipgloss.NewStyle().Align(lipgloss.Center).Render(body)
	if m.width > 0 && m.height > 0 {
		return lipgloss.Place(m.width, m.height, lipgloss.Center, lipgloss.Center, body)
	}
	return body
}

// blockOrmos renders "ORMOS" as five-row block letters.
func blockOrmos() string {
	glyphs := map[rune][5]string{
		'O': {"█████", "█   █", "█   █", "█   █", "█████"},
		'R': {"████ ", "█   █", "████ ", "█  █ ", "█   █"},
		'M': {"█   █", "██ ██", "█ █ █", "█   █", "█   █"},
		'S': {"█████", "█    ", "█████", "    █", "█████"},
	}
	rows := make([]string, 5)
	for _, ch := range "ORMOS" {
		g := glyphs[ch]
		for i := 0; i < 5; i++ {
			if rows[i] != "" {
				rows[i] += " "
			}
			rows[i] += g[i]
		}
	}
	return strings.Join(rows, "\n")
}

// hyperlink wraps text in an OSC 8 escape so supporting terminals make it
// clickable; the rest just print the label.
func hyperlink(url, label string) string {
	return "\x1b]8;;" + url + "\x1b\\" + label + "\x1b]8;;\x1b\\"
}

// runLoginTUI shows the device code on the pairing screen while running the
// flow beside it, and returns the approved provisioning payload.
func runLoginTUI(ctx context.Context, httpBase string, req relay.DeviceStartRequest) (*relay.ProvisionResponse, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel() // stop the flow goroutine when the screen exits first

	p := tea.NewProgram(loginModel{}, tea.WithContext(ctx), tea.WithAltScreen())
	go func() {
		out, err := runDeviceLogin(ctx, httpBase, req, func(s relay.DeviceStartResponse, restarted bool) {
			p.Send(loginCodeMsg{start: s, restarted: restarted})
		})
		p.Send(loginFinishedMsg{out: out, err: err})
	}()
	final, err := p.Run()
	if err != nil {
		// WithContext kills the program on ctx cancel; the signal handler turns
		// SIGINT into an interrupt. Both are the user walking away.
		if errors.Is(err, tea.ErrProgramKilled) || errors.Is(err, tea.ErrInterrupted) {
			return nil, errLoginCancelled
		}
		return nil, err
	}
	fm := final.(loginModel)
	if fm.err != nil {
		if errors.Is(fm.err, context.Canceled) {
			return nil, errLoginCancelled
		}
		return nil, fm.err
	}
	return fm.out, nil
}

package system

import (
	"context"
	"errors"
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/nicodes/ormos/relay"
)

// This file is the pairing screen shown while the agent waits for the user to
// approve its device code in the web app. It is deliberately separate from the
// main dashboard (tui.go): pairing happens before there is a token, so the
// dashboard's data-driven model cannot exist yet. What they share is the
// Elm-architecture shape and the lipgloss styles declared in tui.go.

// loginModel renders the current device-flow round: the code to type into the
// web app and how the wait is going.
type loginModel struct {
	code      string
	url       string
	expiresIn int
	restarts  int // codes that expired unapproved before this one
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

func (m loginModel) Init() tea.Cmd { return nil }

func (m loginModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "ctrl+c", "q", "esc":
			m.err = errLoginCancelled
			return m, tea.Quit
		}
	case loginCodeMsg:
		m.code = msg.start.UserCode
		m.url = msg.start.VerificationURL
		m.expiresIn = msg.start.ExpiresIn
		if msg.restarted {
			m.restarts++
		}
	case loginFinishedMsg:
		m.out = msg.out
		m.err = msg.err
		return m, tea.Quit
	}
	return m, nil
}

func (m loginModel) View() string {
	var b strings.Builder
	b.WriteString(titleStyle.Render("ormos — register this machine") + "\n\n")
	if m.code == "" {
		b.WriteString(labelStyle.Render("requesting a pairing code…") + "\n")
		return b.String()
	}
	b.WriteString("Open " + dirStyle.Render(m.url) + " and enter\n\n")
	b.WriteString("  " + selStyle.Render(m.code) + "\n\n")
	b.WriteString(hintStyle.Render(fmt.Sprintf("expires in %d seconds", m.expiresIn)))
	if m.restarts > 0 {
		b.WriteString(hintStyle.Render(fmt.Sprintf("  ·  code #%d (the previous one expired)", m.restarts+1)))
	}
	b.WriteString("\n\nwaiting for approval — " + hintStyle.Render("ctrl-c to cancel") + "\n")
	return b.String()
}

// runLoginTUI shows the device code on the pairing screen while running the
// flow beside it, and returns the approved provisioning payload.
func runLoginTUI(ctx context.Context, httpBase string, req relay.DeviceStartRequest) (*relay.ProvisionResponse, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel() // stop the flow goroutine when the screen exits first

	p := tea.NewProgram(loginModel{}, tea.WithContext(ctx))
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

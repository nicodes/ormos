//go:build (linux && !android) || (darwin && !ios)

package system

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/nicodes/ormos/relay"
)

// machineName renders the box as "<distro> - <host> - <goos>/<goarch>", e.g.
// "Arch Linux - omarchy - linux/amd64". The distro is dropped on macOS, where
// there is none to read, rather than repeating the platform.
func machineName() string {
	parts := make([]string, 0, 3)
	if pretty := prettyOSName(); pretty != "" {
		parts = append(parts, pretty)
	}
	parts = append(parts, hostName(), runtime.GOOS+"/"+runtime.GOARCH)
	return strings.Join(parts, " - ")
}

// prettyOSName reads PRETTY_NAME from /etc/os-release, empty if unavailable.
// The agent runs on Linux and macOS only, so the other case is macOS, which has
// no such file — hence the early return rather than a failed read.
func prettyOSName() string {
	if runtime.GOOS != "linux" {
		return ""
	}
	b, err := os.ReadFile("/etc/os-release")
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(b), "\n") {
		if v, ok := strings.CutPrefix(line, "PRETTY_NAME="); ok {
			return strings.Trim(v, `"`)
		}
	}
	return ""
}

// hostName returns the host's name, or "?" if it can't be determined.
func hostName() string {
	h, err := os.Hostname()
	if err != nil || h == "" {
		return "?"
	}
	return h
}

// runTUI starts the system tunnel and renders a Bubble Tea dashboard that also
// lets the operator manage this system's projects and ports.
func runTUI(ctx context.Context, d *system) {
	go d.Run(ctx)

	p := tea.NewProgram(newModelContext(ctx, d), tea.WithAltScreen(), tea.WithMouseCellMotion())

	// Quit the UI if the process is signalled.
	go func() {
		<-ctx.Done()
		p.Quit()
	}()

	_, _ = p.Run()
}

// ---- messages & tick ------------------------------------------------------

type tickMsg time.Time

func tick() tea.Cmd {
	return tea.Tick(500*time.Millisecond, func(t time.Time) tea.Msg { return tickMsg(t) })
}

// projectsMsg carries the result of a project-list refresh.
type projectsMsg struct {
	projects []relay.ProjectInfo
	err      error
}

// terminalsMsg carries an independent persisted-terminal refresh.
type terminalsMsg struct {
	terminals []relay.TerminalSessionInfo
	err       error
}

// terminalCreatedMsg starts the interactive attachment only after persistence
// has succeeded.
type terminalCreatedMsg struct {
	projectRoot string
	info        relay.TerminalSessionInfo
	err         error
}

var terminalSessionsCommand = func(d *system) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		terminals, err := d.fetchTerminalSessions(ctx)
		return terminalsMsg{terminals: terminals, err: err}
	}
}

// mutatedMsg carries the result of a create/update/delete.
type mutatedMsg struct {
	ok  string
	err error
}

// systemInfoMsg carries the system's own info (name, etc.) from the relay.
type systemInfoMsg struct {
	info *relay.SystemInfo
	err  error
}

// eventMsg fires when the relay pushes a "data changed" nudge (web-UI edit).
type eventMsg struct{}

// ---- model ----------------------------------------------------------------

type uiMode int

const (
	modeNormal uiMode = iota
	modeInput
	modeConfirm
	modeTerminal
)

type rowKind int

const (
	rowSystemName rowKind = iota // the editable system name, under the SYSTEM header
	rowProject
	rowTerminal // a persisted terminal tab under a project
	rowAddTerminal
	rowPort       // a port under a project
	rowAddPort    // the "+ port" action under a project
	rowAddProject // the "+ new project" action at the end of the list
)

// row is one line in the flattened project/port list. rowProject/rowPort/
// rowAddPort carry a projectIdx so project-scoped actions work from any of them.
type row struct {
	kind        rowKind
	projectIdx  int
	portIdx     int
	terminalIdx int
	projectID   string
	itemID      string
}

type terminalTab struct {
	info  relay.TerminalSessionInfo
	label string
}

// wizField is one prompt in an input wizard.
type wizField struct {
	label       string
	placeholder string
	initial     string
	numeric     bool
	optional    bool
}

// wizard collects one or more fields, then builds the mutation command.
type wizard struct {
	title  string
	fields []wizField
	idx    int
	values []string
	build  func(vals []string) tea.Cmd
}

// confirmPrompt is a yes/no gate before a destructive action.
type confirmPrompt struct {
	text string
	run  func() tea.Cmd
}

type model struct {
	d      *system
	appCtx context.Context

	projects  []relay.ProjectInfo
	terminals []terminalTab
	rows      []row
	cursor    int
	live      map[int]bool // ports currently listening on the host

	mode           uiMode
	input          textinput.Model
	wiz            *wizard
	conf           *confirmPrompt
	term           *terminalScreen
	termGeneration uint64

	status      Status
	logs        []string          // tail of the agent's log ring, for the ACTIVITY pane
	info        *relay.SystemInfo // system's own name/etc. from the relay
	machine     string            // "<distro> - <host> - <goos>/<goarch>"
	fingerprint string            // sealing-key fingerprint for out-of-band verification
	notice      string            // last success line
	err         string            // last error line
	projectErr  string            // latest project-list error, independent of actions
	terminalErr string            // latest terminal-list error, independent of actions
	ticks       int

	width, height int
}

// activityLines is the most log lines the ACTIVITY pane will show. It shows
// fewer when the rest of the dashboard needs the rows — see activityBudget.
//
// Small on purpose: these are the last few things that happened, not a log
// viewer. The whole ring belongs to the agent, and `ormos` run without a TTY
// prints every line to stderr.
const activityLines = 6

func newModel(d *system) model {
	return newModelContext(context.Background(), d)
}

func newModelContext(ctx context.Context, d *system) model {
	ti := textinput.New()
	ti.Prompt = "› "
	ti.CharLimit = 512
	return model{
		d:           d,
		appCtx:      ctx,
		status:      d.Snapshot(),
		logs:        d.RecentLogs(activityLines),
		input:       ti,
		live:        map[int]bool{},
		machine:     machineName(),
		fingerprint: d.Fingerprint(),
	}
}

func (m model) Init() tea.Cmd {
	return tea.Batch(tick(), m.refreshCmd(), m.terminalsCmd(), m.infoCmd(), m.waitEventCmd())
}

// waitEventCmd blocks until the relay pushes a data-change nudge, then re-arms
// (from Update) so UI-side edits reflect in the TUI instantly.
func (m model) waitEventCmd() tea.Cmd {
	ch := m.d.Events()
	return func() tea.Msg {
		<-ch
		return eventMsg{}
	}
}

// ---- commands -------------------------------------------------------------

func (m model) refreshCmd() tea.Cmd {
	d := m.d
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		ps, err := d.fetchProjects(ctx)
		return projectsMsg{projects: ps, err: err}
	}
}

func (m model) infoCmd() tea.Cmd {
	d := m.d
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		info, err := d.fetchSystemInfo(ctx)
		return systemInfoMsg{info: info, err: err}
	}
}

func (m model) terminalsCmd() tea.Cmd {
	return terminalSessionsCommand(m.d)
}

func (m model) createTerminalCmd(p relay.ProjectInfo) tea.Cmd {
	d := m.d
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		info, err := d.createTerminalSession(ctx, p.ID)
		return terminalCreatedMsg{projectRoot: p.RootDir, info: info, err: err}
	}
}

// mutateCmd runs a mutation with a timeout and reports the outcome.
func mutateCmd(ok string, fn func(ctx context.Context) error) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		return mutatedMsg{ok: ok, err: fn(ctx)}
	}
}

// ---- update ---------------------------------------------------------------

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.input.Width = msg.Width - 6
		if m.mode == modeTerminal {
			return m.resizeTerminal()
		}
		return m, nil

	case tickMsg:
		m.status = m.d.Snapshot()
		m.logs = m.d.RecentLogs(activityLines)
		m.live = m.status.Live
		m.ticks++
		// Fallback refresh (~10s) in case a pushed nudge was missed; the relay's
		// realtime push is the primary path for prompt updates.
		if m.ticks%20 == 0 {
			return m, tea.Batch(tick(), m.refreshCmd(), m.terminalsCmd(), m.infoCmd())
		}
		return m, tick()

	case projectsMsg:
		if msg.err != nil {
			m.projectErr = sanitize("load: " + msg.err.Error())
			return m, nil
		}
		// Clear it: every pane starts at once under `make dev`, so the first
		// refresh usually loses a race with `go run ./api` still compiling. That
		// "connection refused" used to stay on screen for the rest of the
		// session even though the very next poll succeeded.
		m.projectErr = ""
		m.projects = sanitizeProjects(msg.projects)
		sort.SliceStable(m.projects, func(i, j int) bool { return m.projects[i].Name < m.projects[j].Name })
		m.rebuildRows()
		return m, nil

	case terminalsMsg:
		if msg.err != nil {
			m.terminalErr = sanitize("load terminals: " + msg.err.Error())
			return m, nil
		}
		m.terminalErr = ""
		m.terminals = sanitizeTerminals(msg.terminals)
		m.rebuildRows()
		return m, nil

	case systemInfoMsg:
		if msg.err == nil && msg.info != nil {
			info := *msg.info
			info.Name = sanitize(info.Name)
			m.info = &info
		}
		return m, nil

	case eventMsg:
		// Relay pushed a data-change nudge: refetch now and re-arm the waiter.
		return m, tea.Batch(m.refreshCmd(), m.terminalsCmd(), m.infoCmd(), m.waitEventCmd())

	case mutatedMsg:
		if msg.err != nil {
			m.err = sanitize(msg.err.Error())
		} else {
			m.err = ""
			// Sanitised like m.err even though every ok string is an agent
			// literal today: the two sit one line apart, and an asymmetry with
			// no reason written down is one a future relay-derived notice
			// inherits silently.
			m.notice = sanitize(msg.ok)
		}
		// Refresh both the project list and system info (a rename changes the header).
		return m, tea.Batch(m.refreshCmd(), m.terminalsCmd(), m.infoCmd())

	case terminalCreatedMsg:
		if msg.err != nil {
			m.err = sanitizeRelayOutput(msg.err.Error())
			return m, m.terminalsCmd()
		}
		m.err = ""
		m.notice = "terminal created"
		m, attach := m.startTerminal(msg.projectRoot, msg.info, msg.info.SessionID)
		return m, tea.Batch(
			tea.DisableMouse,
			m.terminalsCmd(),
			attach,
		)

	case terminalAttachedMsg:
		return m.updateTerminalAttached(msg)

	case terminalOutputMsg:
		return m.updateTerminalOutput(msg)

	case terminalInputWrittenMsg:
		return m.updateTerminalInputWritten(msg)

	case terminalFinishedMsg:
		return m.updateTerminalFinished(msg)

	case tea.MouseMsg:
		if m.mode == modeTerminal {
			return m, nil
		}
		mouse := tea.MouseEvent(msg)
		if m.mode == modeNormal && mouse.Action == tea.MouseActionPress && mouse.Button == tea.MouseButtonLeft {
			if cursor, ok := m.rowAtScreen(mouse.X, mouse.Y); ok {
				m.cursor = cursor
			}
		}
		return m, nil

	case tea.KeyMsg:
		switch m.mode {
		case modeInput:
			return m.updateInput(msg)
		case modeConfirm:
			return m.updateConfirm(msg)
		case modeTerminal:
			return m.updateTerminalKey(msg)
		default:
			return m.updateNormal(msg)
		}
	}
	return m, nil
}

func (m model) updateNormal(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "q", "ctrl+c", "esc":
		return m, tea.Quit
	case "up", "k":
		if m.cursor > 0 {
			m.cursor--
		}
	case "down", "j":
		if m.cursor < len(m.rows)-1 {
			m.cursor++
		}
	case "g", "home":
		m.cursor = 0
	case "G", "end":
		if len(m.rows) > 0 {
			m.cursor = len(m.rows) - 1
		}
	case "r":
		// Rename the selected item: a project's name or a port's label.
		if r, ok := m.selectedRow(); ok {
			switch r.kind {
			case rowProject:
				p := m.projects[r.projectIdx]
				m.startWizard(&wizard{
					title:  "Rename project",
					fields: []wizField{{label: "name", initial: p.Name}},
					build: func(v []string) tea.Cmd {
						id := p.ID
						return mutateCmd("renamed to "+v[0], func(ctx context.Context) error { return m.d.updateProject(ctx, id, v[0], "") })
					},
				})
			case rowPort:
				pt := m.projects[r.projectIdx].Ports[r.portIdx]
				m.startWizard(&wizard{
					title:  fmt.Sprintf("Label for :%d", pt.Port),
					fields: []wizField{{label: "label", initial: pt.Label, optional: true}},
					build: func(v []string) tea.Cmd {
						id, port := pt.ID, pt.Port
						return mutateCmd(fmt.Sprintf("relabeled :%d", port), func(ctx context.Context) error { return m.d.updatePort(ctx, id, v[0]) })
					},
				})
			}
		}
	case "e":
		if pi, ok := m.selectedProjectRow(); ok {
			p := m.projects[pi]
			m.startWizard(&wizard{
				title:  "Edit root dir",
				fields: []wizField{{label: "root dir", initial: p.RootDir}},
				build: func(v []string) tea.Cmd {
					id := p.ID
					return mutateCmd("updated dir", func(ctx context.Context) error { return m.d.updateProject(ctx, id, "", v[0]) })
				},
			})
		}
	case "enter":
		// Action rows mutate; terminal rows enter the already-persisted tab.
		if r, ok := m.selectedRow(); ok {
			switch r.kind {
			case rowSystemName:
				cur := ""
				if m.info != nil {
					cur = m.info.Name
				}
				m.startWizard(&wizard{
					title:  "Rename system",
					fields: []wizField{{label: "system name", initial: cur}},
					build: func(v []string) tea.Cmd {
						return mutateCmd("system renamed to "+v[0], func(ctx context.Context) error { return m.d.renameSystem(ctx, v[0]) })
					},
				})
			case rowAddProject:
				m.startWizard(&wizard{
					title: "New project",
					fields: []wizField{
						{label: "name", placeholder: "my-app"},
						{label: "root dir", placeholder: "/home/you/code/my-app"},
					},
					build: func(v []string) tea.Cmd {
						return mutateCmd("created "+v[0], func(ctx context.Context) error { return m.d.createProject(ctx, v[0], v[1]) })
					},
				})
			case rowAddPort:
				p := m.projects[r.projectIdx]
				m.startWizard(&wizard{
					title: "Add port to " + p.Name,
					fields: []wizField{
						{label: "port", placeholder: "3000", numeric: true},
						{label: "label (optional)", placeholder: "web", optional: true},
					},
					build: func(v []string) tea.Cmd {
						id := p.ID
						port, _ := strconv.Atoi(v[0])
						return mutateCmd(fmt.Sprintf("added :%d", port), func(ctx context.Context) error { return m.d.addPort(ctx, id, port, v[1]) })
					},
				})
			case rowTerminal:
				p := m.projects[r.projectIdx]
				tab := m.terminals[r.terminalIdx]
				if tab.info.State == relay.TerminalStateExited {
					recordID := tab.info.ID
					return m, func() tea.Msg {
						ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
						defer cancel()
						info, err := m.d.restartTerminalSession(ctx, recordID)
						return terminalCreatedMsg{projectRoot: p.RootDir, info: info, err: err}
					}
				}
				if tab.info.State != "" && tab.info.State != relay.TerminalStateRunning {
					m.err = "terminal is closing"
					return m, nil
				}
				m, attach := m.startTerminal(p.RootDir, tab.info, tab.label)
				return m, tea.Batch(tea.DisableMouse, attach)
			case rowAddTerminal:
				m.notice = ""
				return m, m.createTerminalCmd(m.projects[r.projectIdx])
			}
		}
	case "L":
		// Sign this machine out. Shift-L rather than "l" so a stray keypress
		// while navigating cannot unpair the machine.
		who := m.d.cfg.Email
		if who == "" {
			who = "this machine"
		}
		m.mode = modeConfirm
		m.conf = &confirmPrompt{
			text: fmt.Sprintf("Sign out %s and quit? Logging in again re-registers this same system.", who),
			run: func() tea.Cmd {
				return func() tea.Msg {
					if err := clearLoginConfig(); err != nil {
						return mutatedMsg{err: fmt.Errorf("signing out: %w", err)}
					}
					return tea.QuitMsg{}
				}
			},
		}
	case "d":
		// Delete the selected item: a project (with confirm) or a port.
		if r, ok := m.selectedRow(); ok {
			switch r.kind {
			case rowProject:
				p := m.projects[r.projectIdx]
				m.conf = &confirmPrompt{
					text: fmt.Sprintf("Delete project %q and its %d port(s)?", p.Name, len(p.Ports)),
					run: func() tea.Cmd {
						id, name := p.ID, p.Name
						return mutateCmd("deleted "+name, func(ctx context.Context) error { return m.d.deleteProject(ctx, id) })
					},
				}
				m.mode = modeConfirm
			case rowPort:
				port := m.projects[r.projectIdx].Ports[r.portIdx]
				m.notice = ""
				return m, mutateCmd(fmt.Sprintf("removed :%d", port.Port), func(ctx context.Context) error { return m.d.deletePort(ctx, port.ID) })
			case rowTerminal:
				tab := m.terminals[r.terminalIdx]
				return m, mutateCmd("deleted terminal", func(ctx context.Context) error { return m.d.deleteTerminalSession(ctx, tab.info.ID) })
			}
		}
	}
	return m, nil
}

func (m model) updateInput(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		m.mode = modeNormal
		m.wiz = nil
		m.input.Blur()
		return m, nil
	case "enter":
		val := strings.TrimSpace(m.input.Value())
		f := m.wiz.fields[m.wiz.idx]
		if val == "" && !f.optional {
			m.err = f.label + " is required"
			return m, nil
		}
		if f.numeric && val != "" {
			n, err := strconv.Atoi(val)
			if err != nil || !relay.ValidPort(n) {
				m.err = "port must be a number 1-65535"
				return m, nil
			}
		}
		m.err = ""
		m.wiz.values = append(m.wiz.values, val)
		m.wiz.idx++
		if m.wiz.idx < len(m.wiz.fields) {
			m.applyField(m.wiz.fields[m.wiz.idx])
			return m, textinput.Blink
		}
		w := m.wiz
		m.wiz = nil
		m.mode = modeNormal
		m.input.Blur()
		return m, w.build(w.values)
	default:
		var cmd tea.Cmd
		m.input, cmd = m.input.Update(msg)
		return m, cmd
	}
}

func (m model) updateConfirm(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "y", "Y":
		run := m.conf.run
		m.conf = nil
		m.mode = modeNormal
		m.notice = ""
		return m, run()
	case "n", "N", "esc", "q":
		m.conf = nil
		m.mode = modeNormal
		return m, nil
	}
	return m, nil
}

// ---- wizard/row helpers ---------------------------------------------------

func (m *model) startWizard(w *wizard) {
	w.idx = 0
	w.values = nil
	m.wiz = w
	m.mode = modeInput
	m.err = ""
	m.notice = ""
	m.applyField(w.fields[0])
}

func (m *model) applyField(f wizField) {
	m.input.Placeholder = f.placeholder
	m.input.SetValue(f.initial)
	m.input.CursorEnd()
	m.input.Focus()
}

func (m *model) rebuildRows() {
	selected, hadSelection := m.selectedRow()
	rows := make([]row, 0, len(m.projects)+len(m.terminals)+2)
	rows = append(rows, row{kind: rowSystemName})
	for pi := range m.projects {
		projectID := m.projects[pi].ID
		rows = append(rows, row{kind: rowProject, projectIdx: pi, projectID: projectID})
		for ti := range m.terminals {
			if m.terminals[ti].info.ProjectID == projectID {
				rows = append(rows, row{kind: rowTerminal, projectIdx: pi, terminalIdx: ti,
					projectID: projectID, itemID: m.terminals[ti].info.SessionID})
			}
		}
		rows = append(rows, row{kind: rowAddTerminal, projectIdx: pi, projectID: projectID})
		for qi := range m.projects[pi].Ports {
			rows = append(rows, row{kind: rowPort, projectIdx: pi, portIdx: qi,
				projectID: projectID, itemID: m.projects[pi].Ports[qi].ID})
		}
		rows = append(rows, row{kind: rowAddPort, projectIdx: pi, projectID: projectID})
	}
	rows = append(rows, row{kind: rowAddProject})
	m.rows = rows
	if hadSelection {
		if idx := rowIndex(rows, selected.kind, selected.projectID, selected.itemID); idx >= 0 {
			m.cursor = idx
		} else if selected.kind == rowTerminal {
			// A removed selected tab falls back to a useful action in the same
			// project, or its project row if that action is unavailable.
			if idx := rowIndex(rows, rowAddTerminal, selected.projectID, ""); idx >= 0 {
				m.cursor = idx
			} else if idx := rowIndex(rows, rowProject, selected.projectID, ""); idx >= 0 {
				m.cursor = idx
			}
		}
	}
	if m.cursor >= len(rows) {
		m.cursor = len(rows) - 1
	}
	if m.cursor < 0 {
		m.cursor = 0
	}
}

func rowIndex(rows []row, kind rowKind, projectID, itemID string) int {
	for i, r := range rows {
		if r.kind == kind && r.projectID == projectID && r.itemID == itemID {
			return i
		}
	}
	return -1
}

func (m model) selectedRow() (row, bool) {
	if m.cursor < 0 || m.cursor >= len(m.rows) {
		return row{}, false
	}
	return m.rows[m.cursor], true
}

// selectedProjectRow returns the project index only when the cursor is on a
// project row, so project-scoped actions are gated to a selected project.
func (m model) selectedProjectRow() (int, bool) {
	r, ok := m.selectedRow()
	if !ok || r.kind != rowProject {
		return 0, false
	}
	return r.projectIdx, true
}

// ---- view -----------------------------------------------------------------

var (
	titleStyle   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("205"))
	onStyle      = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("42"))
	offStyle     = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("196"))
	labelStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("245"))
	dirStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("109"))
	selStyle     = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("212"))
	hintStyle    = lipgloss.NewStyle().Faint(true)
	noticeStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("42"))
	errStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("203"))
	sectionStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("246"))
)

// divider draws a faint full-width horizontal rule, used to fence a screen's
// footer off from its body.
func divider(width int) string {
	if width < 1 {
		width = 1
	}
	return hintStyle.Render(strings.Repeat("─", width))
}

func (m model) View() string {
	if m.mode == modeTerminal {
		return m.terminalView()
	}
	s := m.status
	state := offStyle.Render("● offline")
	if s.Connected {
		state = onStyle.Render("● connected")
	}
	// How many streams the relay currently has open against this machine. Every
	// one of them is a shell or a connection to a local service, so "is anything
	// happening on my machine right now" should be answerable from the header
	// rather than by reading the activity pane.
	// Always, including zero: "nothing is happening" and "this field is not
	// rendered" must not look the same, and an unbalanced addSession showing a
	// negative should be visible rather than hidden.
	if s.Sessions == 1 {
		state += labelStyle.Render("  1 session")
	} else {
		state += labelStyle.Render(fmt.Sprintf("  %d sessions", s.Sessions))
	}

	name := "ormos system"
	if m.info != nil && m.info.Name != "" {
		name = m.info.Name
	}
	var b strings.Builder
	// Which account this machine is registered under, on the title line: the
	// agent can be paired to any account, and a terminal here is a shell on this
	// box, so "who is this connected as" should never need looking up.
	head := titleStyle.Render("ormos")
	if email := sanitize(m.d.cfg.Email); email != "" {
		head += "  " + labelStyle.Render(email)
	}
	b.WriteString(head + "   " + state + "\n\n")
	b.WriteString(sectionStyle.Render("SYSTEM") + "\n")
	// The system name is a targetable row (cursor 0); enter renames it.
	nameCursor := "  "
	nameVal := name
	if r, ok := m.selectedRow(); ok && r.kind == rowSystemName {
		nameCursor = selStyle.Render("› ")
		nameVal = selStyle.Render(name)
	}
	b.WriteString(nameCursor + labelStyle.Render("name     ") + nameVal + "\n")
	b.WriteString("  " + labelStyle.Render("machine  ") + m.machine + "\n")
	// Which relay this agent is talking to. ORMOS_API_URL and the saved config
	// can each set it, so "connected to what" is worth stating outright rather
	// than leaving to be inferred from a connect line in the activity pane.
	// Sanitised like everything else on this screen. checkRelayTransport does
	// not parse the URL, so an ORMOS_API_URL carrying an escape sequence would
	// otherwise reach the alt screen intact. It is the operator's own
	// environment rather than the relay's, which is why it is here rather than
	// at a boundary — but the point of sanitising at all is that no site has to
	// be remembered, and this line was the sixth.
	b.WriteString("  " + labelStyle.Render("relay    ") + sanitize(s.RelayURL) + "\n")
	// The sealing-key fingerprint, for out-of-band verification against the app:
	// if the two do not match, a relay has swapped the key.
	b.WriteString("  " + labelStyle.Render("sealing  ") + m.fingerprint + hintStyle.Render("  verify in app") + "\n\n")

	b.WriteString(sectionStyle.Render("PROJECTS") + "\n")
	if len(m.projects) == 0 {
		b.WriteString(hintStyle.Render("  no projects yet") + "\n")
	}
	for i, r := range m.rows {
		if r.kind == rowSystemName {
			continue // rendered in the SYSTEM section above
		}
		cursor := "  "
		if i == m.cursor {
			cursor = selStyle.Render("› ")
		}
		switch r.kind {
		case rowProject:
			p := m.projects[r.projectIdx]
			name := pad(p.Name, 18)
			if i == m.cursor {
				name = selStyle.Render(name)
			}
			summary := labelStyle.Render(portSummary(p, m.live))
			b.WriteString(cursor + name + " " + dirStyle.Render(pad(p.RootDir, 32)) + " " + summary + "\n")
		case rowTerminal:
			tab := m.terminals[r.terminalIdx]
			label := tab.label
			if tab.info.State == relay.TerminalStateExited {
				label += " (exited)"
			}
			if tab.info.State == relay.TerminalStateClosing {
				label += " (closing)"
			}
			if i == m.cursor {
				label = selStyle.Render(label)
			}
			b.WriteString(fmt.Sprintf("    %s terminal %s\n", cursor, label))
		case rowAddTerminal:
			label := "+ terminal"
			if i == m.cursor {
				label = selStyle.Render(label)
			} else {
				label = hintStyle.Render(label)
			}
			b.WriteString(fmt.Sprintf("    %s %s\n", cursor, label))
		case rowPort:
			p := m.projects[r.projectIdx].Ports[r.portIdx]
			// Selected → pink; otherwise live ports render green. No leading dot.
			portStr := fmt.Sprintf(":%-5d", p.Port)
			switch {
			case i == m.cursor:
				portStr = selStyle.Render(portStr)
			case m.live[p.Port]:
				portStr = onStyle.Render(portStr)
			}
			b.WriteString(fmt.Sprintf("    %s %s %s\n", cursor, portStr, p.Label))
		case rowAddPort:
			label := "+ port"
			if i == m.cursor {
				label = selStyle.Render(label)
			} else {
				label = hintStyle.Render(label)
			}
			b.WriteString(fmt.Sprintf("    %s %s\n", cursor, label))
		case rowAddProject:
			label := "+ new project"
			if i == m.cursor {
				label = selStyle.Render(label)
			} else {
				label = hintStyle.Render(label)
			}
			b.WriteString(cursor + label + "\n")
		}
	}

	// Clipped as a block, so the line count below is also the ROW count.
	body := clipLines(strings.TrimRight(b.String(), "\n"), m.width)

	// Footer: input wizard, confirm prompt, or key hints — fenced from the body
	// by a full-width divider and pinned to the bottom of the terminal.
	var f strings.Builder
	switch m.mode {
	case modeInput:
		fld := m.wiz.fields[m.wiz.idx]
		f.WriteString(titleStyle.Render(m.wiz.title) + labelStyle.Render(fmt.Sprintf("  (%s)", fld.label)) + "\n")
		f.WriteString(m.input.View() + "\n")
		f.WriteString(hintStyle.Render("enter: ok · esc: cancel"))
	case modeConfirm:
		f.WriteString(errStyle.Render(m.conf.text) + hintStyle.Render("  (y/n)"))
	default:
		// Show only the actions that apply to the selected row.
		hints := "↑/↓ move"
		if r, ok := m.selectedRow(); ok {
			switch r.kind {
			case rowSystemName:
				hints += " · enter rename system"
			case rowProject:
				hints += " · r rename · e dir · d delete"
			case rowTerminal:
				hints += " · enter open in TUI (Ctrl-G detaches)"
			case rowAddTerminal:
				hints += " · enter new terminal in TUI (Ctrl-G detaches)"
			case rowPort:
				hints += " · r label · d delete"
			case rowAddPort:
				hints += " · enter add port"
			case rowAddProject:
				hints += " · enter new project"
			}
		}
		hints += " · L sign out · q quit"
		f.WriteString(hintStyle.Render(hints))
	}
	// Status line: last error or success notice, under the hints. Both are
	// sanitised where they are ASSIGNED rather than here — one place per
	// string, so there is no asymmetry for a future field to inherit.
	displayErr := m.err
	if displayErr == "" {
		if m.terminalErr != "" {
			displayErr = m.terminalErr
		} else {
			displayErr = m.projectErr
		}
	}
	if displayErr != "" {
		f.WriteString("\n" + errStyle.Render("✗ "+displayErr))
	} else if m.notice != "" {
		f.WriteString("\n" + noticeStyle.Render("✓ "+m.notice))
	}
	footer := clipLines(divider(m.width)+"\n"+f.String(), m.width)

	// ACTIVITY: the tail of the agent's log ring, rendered last and given only
	// the rows nothing above it needed. Headless mode echoes every line to
	// stderr; the dashboard owns the whole screen, so without this pane a
	// tunnel that will not connect shows as "offline" and nothing else — the
	// reason goes to a ring the operator has no way to look at.
	if n := m.activityBudget(body, footer); n > 0 {
		var a strings.Builder
		a.WriteString(body + "\n\n" + sectionStyle.Render("ACTIVITY") + "\n")
		for _, line := range m.logs[len(m.logs)-n:] {
			// Sanitised because part of the ring is relay-influenced and an
			// escape sequence must never reach the terminal. The width is
			// handled by clipLines below, with every other row.
			a.WriteString("  " + hintStyle.Render(sanitize(line)) + "\n")
		}
		body = clipLines(strings.TrimRight(a.String(), "\n"), m.width)
	}

	// Pin the footer to the bottom when the body fits. When it does not, the
	// view goes out over-tall and Bubble Tea keeps its LAST height rows — so
	// the TOP of the body is what is lost, not the footer. That is what
	// activityBudget exists to stop the pane from causing; a project list long
	// enough to overflow on its own still does it.
	if m.width > 0 && m.height > 0 {
		if avail := m.height - lipgloss.Height(footer); avail >= 1 && lipgloss.Height(body) <= avail {
			return lipgloss.Place(m.width, avail, lipgloss.Left, lipgloss.Top, body) + "\n" + footer
		}
	}
	return body + "\n" + footer
}

// portSummary describes a project's port count and how many are live.
func portSummary(p relay.ProjectInfo, live map[int]bool) string {
	if len(p.Ports) == 0 {
		return "no ports"
	}
	liveN := 0
	for _, pt := range p.Ports {
		if live[pt.Port] {
			liveN++
		}
	}
	return fmt.Sprintf("%d ports · %d live", len(p.Ports), liveN)
}

// activityBudget returns how many log lines the ACTIVITY pane may draw.
//
// The pane is the last thing on the screen and the first thing that should give
// way. Everything above it is worth more than a sixth line of history — the
// connection state, the account this machine is paired to, and the sealing
// fingerprint the user reads against the app to catch a relay that swapped the
// key. And an over-tall view does not scroll: Bubble Tea's renderer keeps the
// LAST height rows, so it deletes the header rather than the oldest log line.
// Unbudgeted, four projects were enough to push the whole SYSTEM block off an
// 80x24 terminal.
//
// A height of zero means no WindowSizeMsg has arrived yet, so there is nothing
// to budget against; the pane draws at full size and the first resize corrects
// it.
func (m model) activityBudget(body, footer string) int {
	if m.height <= 0 {
		// No WindowSizeMsg yet, so there is nothing to budget against: draw
		// what there is and let the first resize correct it.
		return min(activityLines, len(m.logs))
	}
	// The pane costs a blank separator and a heading before any log line, and
	// it can never draw more lines than the ring has.
	avail := m.height - lipgloss.Height(body) - lipgloss.Height(footer) - 2
	return min(max(avail, 0), activityLines, len(m.logs))
}

// sanitizeProjects copies the relay's project list with every string it chose
// stripped of control characters.
//
// At the boundary, not at each render site: the names and directories here are
// painted in five places, and a sanitiser applied per site is one someone
// forgets at the sixth. Everything downstream of this inherits it.
func sanitizeProjects(in []relay.ProjectInfo) []relay.ProjectInfo {
	out := make([]relay.ProjectInfo, len(in))
	for i, p := range in {
		p.Name = sanitize(p.Name)
		p.RootDir = sanitize(p.RootDir)
		ports := make([]relay.PortEntry, len(p.Ports))
		for j, pt := range p.Ports {
			pt.Label = sanitize(pt.Label)
			ports[j] = pt
		}
		p.Ports = ports
		out[i] = p
	}
	return out
}

// sanitizeTerminals retains the raw session id used in the PTY key while
// assigning a safe, non-empty display label exactly once at the relay boundary.
func sanitizeTerminals(in []relay.TerminalSessionInfo) []terminalTab {
	out := make([]terminalTab, len(in))
	for i, info := range in {
		info.ProjectName = sanitize(info.ProjectName)
		label := strings.TrimSpace(sanitize(info.SessionID))
		if label == "" {
			label = "terminal"
		}
		out[i] = terminalTab{info: info, label: label}
	}
	return out
}

// rowAtScreenY maps a mouse coordinate back to a durable model row. The body
// starts with a fixed SYSTEM/header block; an over-tall Bubble Tea view keeps
// its last height rows, so the same top-clipping offset is applied here.
func (m model) rowAtScreenY(y int) (int, bool) {
	if y < 0 || (m.height > 0 && y >= m.height) {
		return 0, false
	}
	topClip := 0
	if m.height > 0 {
		topClip = max(0, lipgloss.Height(m.View())-m.height)
	}
	line := 3 - topClip // SYSTEM name row
	if y == line {
		return 0, true
	}
	line = 9 - topClip // first PROJECTS row
	if len(m.projects) == 0 {
		line++ // "no projects yet"
	}
	for i, r := range m.rows {
		if r.kind == rowSystemName {
			continue
		}
		if y == line {
			return i, true
		}
		line++
	}
	return 0, false
}

func (m model) rowAtScreen(x, y int) (int, bool) {
	if x < 0 || (m.width > 0 && x >= m.width) {
		return 0, false
	}
	return m.rowAtScreenY(y)
}

// sanitize strips anything unprintable from text before it is painted into the
// terminal.
//
// Most of what reaches this screen is chosen by the relay: the system name, the
// project names and root directories, the port labels, and — inside "relay
// returned %s" — an HTTP reason phrase, which Go does not sanitise either.
// Painted raw into the alt screen, a relay could set the window title, clear
// the display, move the cursor, or paint a convincing line of its own — from a
// pane whose whole purpose is to tell the operator the truth about their
// machine. A newline is dropped for a second reason: it would add a full-width
// row and make the pane's height depend on what the relay sent.
func sanitize(s string) string {
	return strings.Map(func(r rune) rune {
		if !unicode.IsPrint(r) {
			return -1
		}
		return r
	}, s)
}

// sanitizeRelayOutput makes relay-derived text safe for a terminal-facing
// output. Unlike sanitize, it never returns an empty string: callers use this
// for errors and status text, where silence would hide what failed. Pairing
// codes and URLs are instead rejected at deviceStart when they sanitize to
// empty; substituting this diagnostic for either would look actionable but
// could never complete a pairing.
func sanitizeRelayOutput(s string) string {
	if clean := sanitize(s); clean != "" {
		return clean
	}
	return "[relay supplied no printable text]"
}

// clipLines caps every line in s at w columns, so a block's line count is also
// its ROW count.
//
// lipgloss.Height counts newlines, not the rows a terminal will use, and
// lipgloss.Place pads every line out to the width of the widest one — so one
// over-wide row inflates the whole block, and the budget above is then computed
// against a number that no longer describes the screen. A single long project
// name was enough to turn a 24-line view into 41 rows; since Bubble Tea keeps
// the LAST height rows, what falls off the top is the header and the sealing
// fingerprint. Clipping every row is what makes lipgloss.Height honest.
func clipLines(s string, w int) string {
	if w <= 0 {
		return s
	}
	lines := strings.Split(s, "\n")
	for i, line := range lines {
		lines[i] = clip(line, w)
	}
	return strings.Join(lines, "\n")
}

// clip shortens s to at most w display columns, marking the cut with an
// ellipsis. A width of zero or less means the terminal size is not known yet
// (no WindowSizeMsg has arrived), in which case nothing is clipped.
//
// Columns, not bytes: "→→→→→→→→→→" is ten columns and thirty bytes, so slicing
// by length both truncated lines that fitted and cut runes in half, producing
// invalid UTF-8 on screen.
func clip(s string, w int) string {
	if w <= 0 {
		return s
	}
	return ansi.Truncate(s, w, "…")
}

// pad right-pads s to w display columns, truncating with an ellipsis when it is
// too long, for column layout. Width-aware for the same reason clip is: these
// are project names and root directories, which are the user's own text.
func pad(s string, w int) string {
	if w <= 0 {
		return ""
	}
	s = ansi.Truncate(s, w, "…")
	if gap := w - ansi.StringWidth(s); gap > 0 {
		return s + strings.Repeat(" ", gap)
	}
	return s
}

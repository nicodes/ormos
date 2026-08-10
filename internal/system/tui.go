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

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/nicodes/ormos/relay"
)

// machineName renders the box as "<distro> - <host> - <goos>/<goarch>", e.g.
// "Arch Linux - omarchy - linux/amd64". The distro is dropped where there is
// none to read (macOS, Windows) rather than repeating the platform.
func machineName() string {
	parts := make([]string, 0, 3)
	if pretty := prettyOSName(); pretty != "" {
		parts = append(parts, pretty)
	}
	parts = append(parts, hostName(), runtime.GOOS+"/"+runtime.GOARCH)
	return strings.Join(parts, " - ")
}

// prettyOSName reads PRETTY_NAME from /etc/os-release, empty if unavailable.
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

	p := tea.NewProgram(newModel(d), tea.WithAltScreen())

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
)

type rowKind int

const (
	rowSystemName rowKind = iota // the editable system name, under the SYSTEM header
	rowProject
	rowPort       // a port under a project
	rowAddPort    // the "+ port" action under a project
	rowAddProject // the "+ new project" action at the end of the list
)

// row is one line in the flattened project/port list. rowProject/rowPort/
// rowAddPort carry a projectIdx so project-scoped actions work from any of them.
type row struct {
	kind       rowKind
	projectIdx int
	portIdx    int
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
	d *system

	projects []relay.ProjectInfo
	rows     []row
	cursor   int
	live     map[int]bool // ports currently listening on the host

	mode  uiMode
	input textinput.Model
	wiz   *wizard
	conf  *confirmPrompt

	status      Status
	info        *relay.SystemInfo // system's own name/etc. from the relay
	machine     string            // "<distro> - <host> - <goos>/<goarch>"
	fingerprint string            // sealing-key fingerprint for out-of-band verification
	notice      string            // last success line
	err         string            // last error line
	ticks       int

	width, height int
}

func newModel(d *system) model {
	ti := textinput.New()
	ti.Prompt = "› "
	ti.CharLimit = 512
	return model{d: d, status: d.Snapshot(), input: ti, live: map[int]bool{}, machine: machineName(), fingerprint: d.Fingerprint()}
}

func (m model) Init() tea.Cmd {
	return tea.Batch(tick(), m.refreshCmd(), m.infoCmd(), m.waitEventCmd())
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
		return m, nil

	case tickMsg:
		m.status = m.d.Snapshot()
		m.live = m.status.Live
		m.ticks++
		// Fallback refresh (~10s) in case a pushed nudge was missed; the relay's
		// realtime push is the primary path for prompt updates.
		if m.ticks%20 == 0 {
			return m, tea.Batch(tick(), m.refreshCmd(), m.infoCmd())
		}
		return m, tick()

	case projectsMsg:
		if msg.err != nil {
			m.err = "load: " + msg.err.Error()
			return m, nil
		}
		// Clear it: every pane starts at once under `make dev`, so the first
		// refresh usually loses a race with `go run ./api` still compiling. That
		// "connection refused" used to stay on screen for the rest of the
		// session even though the very next poll succeeded.
		m.err = ""
		m.projects = msg.projects
		sort.SliceStable(m.projects, func(i, j int) bool { return m.projects[i].Name < m.projects[j].Name })
		m.rebuildRows()
		return m, nil

	case systemInfoMsg:
		if msg.err == nil && msg.info != nil {
			m.info = msg.info
		}
		return m, nil

	case eventMsg:
		// Relay pushed a data-change nudge: refetch now and re-arm the waiter.
		return m, tea.Batch(m.refreshCmd(), m.infoCmd(), m.waitEventCmd())

	case mutatedMsg:
		if msg.err != nil {
			m.err = msg.err.Error()
		} else {
			m.err = ""
			m.notice = msg.ok
		}
		// Refresh both the project list and system info (a rename changes the header).
		return m, tea.Batch(m.refreshCmd(), m.infoCmd())

	case tea.KeyMsg:
		switch m.mode {
		case modeInput:
			return m.updateInput(msg)
		case modeConfirm:
			return m.updateConfirm(msg)
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
		// The "+ new project" / "+ port" rows are actions: run them on enter.
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
			if err != nil || n < 1 || n > 65535 {
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
	rows := make([]row, 0, len(m.projects)+2)
	rows = append(rows, row{kind: rowSystemName})
	for pi := range m.projects {
		rows = append(rows, row{kind: rowProject, projectIdx: pi})
		for qi := range m.projects[pi].Ports {
			rows = append(rows, row{kind: rowPort, projectIdx: pi, portIdx: qi})
		}
		rows = append(rows, row{kind: rowAddPort, projectIdx: pi})
	}
	rows = append(rows, row{kind: rowAddProject})
	m.rows = rows
	if m.cursor >= len(rows) {
		m.cursor = len(rows) - 1
	}
	if m.cursor < 0 {
		m.cursor = 0
	}
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
	s := m.status
	state := offStyle.Render("● offline")
	if s.Connected {
		state = onStyle.Render("● connected")
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
	if email := m.d.cfg.Email; email != "" {
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
	body := strings.TrimRight(b.String(), "\n")

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
	// Status line: last error or success notice, under the hints.
	if m.err != "" {
		f.WriteString("\n" + errStyle.Render("✗ "+m.err))
	} else if m.notice != "" {
		f.WriteString("\n" + noticeStyle.Render("✓ "+m.notice))
	}
	footer := divider(m.width) + "\n" + f.String()

	// Pin the footer to the bottom when the body fits; otherwise let it follow
	// the content so nothing is clipped on a short terminal.
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

// pad right-pads (or truncates with an ellipsis) s to width w for column layout.
func pad(s string, w int) string {
	if len(s) > w {
		if w <= 1 {
			return s[:w]
		}
		return s[:w-1] + "…"
	}
	return s + strings.Repeat(" ", w-len(s))
}

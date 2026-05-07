package dev

import (
	"bufio"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"strings"
	"sync"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

type PaneSink struct {
	prog    *tea.Program
	wg      sync.WaitGroup
	startMu sync.Mutex
}

func NewPaneSink() *PaneSink {
	model := newPanesModel()
	program := tea.NewProgram(model, tea.WithAltScreen(), tea.WithMouseCellMotion())
	sink := &PaneSink{prog: program}
	sink.wg.Add(1)
	go func() {
		defer sink.wg.Done()
		_, _ = program.Run()
	}()
	return sink
}

func (s *PaneSink) Writer(name string) io.Writer {
	reader, writer := io.Pipe()
	s.ensureTab(name)
	go func() {
		defer func() { _ = reader.Close() }()
		scanner := bufio.NewScanner(reader)
		scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for scanner.Scan() {
			s.prog.Send(appendLineMsg{name: name, line: scanner.Text()})
		}
	}()
	return writer
}

func (s *PaneSink) SystemLine(format string, args ...any) {
	s.ensureTab("angee")
	s.prog.Send(appendLineMsg{name: "angee", line: fmt.Sprintf(format, args...)})
}

func (s *PaneSink) Wait() {
	s.wg.Wait()
}

func (s *PaneSink) Quit() {
	s.prog.Quit()
}

func (s *PaneSink) Done() <-chan struct{} {
	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()
	return done
}

func (s *PaneSink) ensureTab(name string) {
	s.startMu.Lock()
	defer s.startMu.Unlock()
	s.prog.Send(ensureTabMsg{name: name})
}

func IsStdoutTTY() bool {
	info, err := os.Stdout.Stat()
	if err != nil {
		return false
	}
	return (info.Mode() & os.ModeCharDevice) != 0
}

type panesModel struct {
	tabs   []string
	lines  map[string][]string
	views  map[string]viewport.Model
	active int
	width  int
	height int
	ready  bool
}

func newPanesModel() panesModel {
	return panesModel{
		tabs:  []string{"all"},
		lines: map[string][]string{"all": {}},
		views: map[string]viewport.Model{},
	}
}

type appendLineMsg struct{ name, line string }
type ensureTabMsg struct{ name string }

func (m panesModel) Init() tea.Cmd { return nil }

func (m panesModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "ctrl+c", "q":
			return m, tea.Quit
		case "tab", "right", "l":
			m.active = (m.active + 1) % len(m.tabs)
		case "shift+tab", "left", "h":
			m.active = (m.active - 1 + len(m.tabs)) % len(m.tabs)
		}
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.ready = true
		for name, view := range m.views {
			view.Width = msg.Width
			view.Height = m.contentHeight()
			m.views[name] = view
		}
	case ensureTabMsg:
		if !hasTab(m.tabs, msg.name) {
			m.tabs = append(m.tabs, msg.name)
			m.lines[msg.name] = nil
		}
	case appendLineMsg:
		m.lines["all"] = append(m.lines["all"], fmt.Sprintf("[%s] %s", msg.name, msg.line))
		m.lines[msg.name] = append(m.lines[msg.name], msg.line)
		m.setViewportContent(msg.name)
		m.setViewportContent("all")
	}
	if m.ready {
		var cmd tea.Cmd
		view := m.activeViewport()
		view, cmd = view.Update(msg)
		m.views[m.tabs[m.active]] = view
		return m, cmd
	}
	return m, nil
}

func (m panesModel) View() string {
	if !m.ready {
		return "starting..."
	}
	return lipgloss.JoinVertical(
		lipgloss.Left,
		m.renderTabBar(),
		m.activeViewport().View(),
		m.renderStatusBar(),
	)
}

func (m panesModel) renderTabBar() string {
	parts := make([]string, 0, len(m.tabs))
	for index, name := range m.tabs {
		style := tabStyle.Foreground(colorFor(name))
		if index == m.active {
			style = activeTabStyle.Foreground(colorFor(name))
		}
		parts = append(parts, style.Render(name))
	}
	return tabBarStyle.Width(m.width).Render(lipgloss.JoinHorizontal(lipgloss.Top, parts...))
}

func (m panesModel) renderStatusBar() string {
	return statusBarStyle.Width(m.width).Render("tab/shift-tab: switch | q/ctrl-c: quit")
}

func (m panesModel) contentHeight() int {
	if m.height <= 2 {
		return 1
	}
	return m.height - 2
}

func (m *panesModel) setViewportContent(name string) {
	view, ok := m.views[name]
	if !ok {
		view = viewport.New(m.width, m.contentHeight())
	}
	view.SetContent(strings.Join(m.lines[name], "\n"))
	view.GotoBottom()
	m.views[name] = view
}

func (m *panesModel) activeViewport() viewport.Model {
	if m.active < 0 || m.active >= len(m.tabs) {
		return viewport.Model{}
	}
	name := m.tabs[m.active]
	view, ok := m.views[name]
	if !ok {
		view = viewport.New(m.width, m.contentHeight())
		view.SetContent(strings.Join(m.lines[name], "\n"))
		m.views[name] = view
	}
	return view
}

func colorFor(name string) lipgloss.Color {
	colors := []string{"#7DD3FC", "#FCD34D", "#F0ABFC", "#86EFAC", "#93C5FD", "#FCA5A5", "#E5E7EB", "#9CA3AF"}
	index := int(crc32.ChecksumIEEE([]byte(name))) % len(colors)
	return lipgloss.Color(colors[index])
}

func hasTab(tabs []string, name string) bool {
	for _, tab := range tabs {
		if tab == name {
			return true
		}
	}
	return false
}

var (
	tabBarStyle = lipgloss.NewStyle().
			Background(lipgloss.Color("#1F2937")).
			Padding(0, 1)

	tabStyle = lipgloss.NewStyle().
			Padding(0, 1)

	activeTabStyle = lipgloss.NewStyle().
			Padding(0, 1).
			Bold(true).
			Underline(true)

	statusBarStyle = lipgloss.NewStyle().
			Background(lipgloss.Color("#111827")).
			Foreground(lipgloss.Color("#9CA3AF")).
			Padding(0, 1)
)
